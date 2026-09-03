package main

// ---------------------------------------------------------------------------
// Background cleanup workers
//
// Three independent goroutines run every 5 minutes and delete images from
// disk, their thumbnails, and their JSON sidecars:
//
//   startPartialCleanup  — removes images where fewer than 95% of the
//                          expected lines were decoded (i.e. the signal was
//                          lost mid-frame).
//                          Controlled by CLEANUP_PARTIAL_DAYS (default 7).
//
//   startSNRCleanup      — removes images whose average SNR is known, whose
//                          SNR is on a scale this build understands, and which
//                          fall below the threshold for that scale.
//                          Controlled by CLEANUP_SNR_DAYS (default 7).
//
//   startAgeCleanup      — removes ALL images regardless of quality once
//                          they are older than the configured age.  Acts as
//                          a general-purpose retention limit.
//                          Controlled by CLEANUP_ALL_DAYS (default 30).
//
// Setting any variable to 0 disables that worker entirely.
// ---------------------------------------------------------------------------

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const cleanupInterval = 5 * time.Minute

// The low-SNR threshold, and why it moved
// ---------------------------------------
//
// This worker deletes images, so the units of the number it compares against
// matter more than the number.
//
// While this addon spoke audio protocol version 2, the server's packet header
// carried radiod's noise as N0, a power spectral DENSITY in dBFS/Hz, next to a
// baseband power in dBFS integrated over the whole passband.  snrAccumulator
// stores baseband - noise, so on version 2 that difference was not an SNR at
// all: it was S/N0 in dB·Hz, reading 10·log10(bandwidth) too high.  Version 3
// changed the meaning of that field to the noise POWER inside the demodulator
// passband, in dBFS, which is what makes the subtraction a real SNR -- see
// channelNoisePower in the server's radiod_status.go, and channelSignalQuality
// in its websocket.go, which switches on `version >= 3`.
//
// Moving this addon from version 2 to version 4 therefore drops every SNR
// figure it records by 10·log10(filterBandwidthHz), with nothing anywhere
// saying so.  Left uncorrected, a threshold calibrated on the old scale
// condemns every image recorded after the migration.
//
// The bandwidth is the server's `usb` preset, which is what this addon gets:
// wsURL sends frequency and mode only, never bandwidthLow/bandwidthHigh, so
// radiod's preset edges stand.  ubersdr-radiod config/presets.conf [usb] is
// low = +50.0, high = +3k, and FilterBandwidthHz is HighEdge - LowEdge:
//
//	usbFilterBandwidthHz = 3000 - 50 = 2950 Hz
//	snrScaleShiftDB      = 10·log10(2950) = 34.70 dB
//	snrCleanupThreshold  = 40.0 - 34.70   =  5.30 dB
//
// The shift is written as a derivation rather than a literal so that the two
// halves cannot drift apart, and so that a reader can check the arithmetic
// against the preset rather than taking 5.3 on faith.  A deployment running a
// narrower USB filter moves the true shift a little -- 2.4 kHz would give 33.8
// dB, 0.9 dB out -- which is noise beside the 34.7 dB error being corrected.
//
// MEASURED, not just derived
// --------------------------
// The arithmetic above is a units correction; it cannot say whether the
// resulting number still separates a fax from an empty channel.  The server's
// reported noise no longer bottoms out near -30 either, so any figure inherited
// from the clamped era is suspect on its own terms.  So the threshold was
// checked against the live receiver -- m9psy.tunnel.ubersdr.org, 12 kHz USB,
// ~1500 packets per frequency, SNR as this addon computes it:
//
//	empty channel  4610 kHz : median -0.54, 6 s means -0.65 .. -0.38
//	empty channel 13500 kHz : median -1.55, 6 s means -0.12 .. +0.57
//	strong fax     7880 kHz : median 32.67, 6 s means 29.40 .. 32.88
//	                          (p05 23.99, p95 35.19, max 42.40)
//
// The two populations sit about 30 dB apart, at roughly 0 dB and 30 dB, and
// 5.30 dB falls in the gap: 4.7 dB clear of the worst empty-channel average
// observed, and 24 dB below the weakest good one.  The worker compares an
// average over a whole image, which is why the windowed means are the figures
// that matter rather than the per-packet spread.
//
// The threshold is deliberately nearer the noise end of that gap than the
// middle.  This operation deletes files: a marginal fax averaging 10-20 dB is
// kept, and only a channel that measured essentially dead is pruned.
//
// Noise itself measured -103 to -123 dBFS across those runs, which is the
// direct evidence that the old -30 floor is gone.
const (
	// usbFilterBandwidthHz is HighEdge - LowEdge for the server's usb preset.
	usbFilterBandwidthHz = 2950.0

	// legacySNRCleanupThreshold is the threshold this worker used while the
	// addon read version 2 headers, on the S/N0 (dB·Hz) scale.  Kept because it
	// is the origin of the number below, and because it documents what the
	// figures in pre-migration sidecars are to be read against.
	legacySNRCleanupThreshold = 40.0
)

var (
	// snrScaleShiftDB is how much lower a version >= 3 SNR reads than the
	// version 2 figure for the same signal.
	snrScaleShiftDB = 10 * math.Log10(usbFilterBandwidthHz)

	// snrCleanupThreshold is the minimum acceptable average SNR, on the
	// passband-SNR scale that audio protocol versions 3 and up report.
	snrCleanupThreshold = legacySNRCleanupThreshold - snrScaleShiftDB
)

// snrThresholdFor returns the low-SNR deletion threshold that applies to a
// record produced by the given audio protocol version, and whether any
// threshold applies at all.
//
// A record whose audioProtocol is 0 was written before this field existed,
// which is to say before the version 4 migration, so its SNR is on the old
// S/N0 scale.  It is NOT judged against legacySNRCleanupThreshold even though
// that is the threshold it was recorded under: nothing in the sidecar proves
// which scale it is on, only the absence of a marker suggests it, and this
// worker deletes files permanently.  Guessing wrong here is unbounded and
// silent; declining to guess costs bounded disk space, because startAgeCleanup
// (CLEANUP_ALL_DAYS, default 30) still collects these images and no new
// unmarked record can ever be written.  The unmarked set drains and never
// refills.
func snrThresholdFor(audioProtocol int) (float64, bool) {
	if audioProtocol >= 3 {
		return snrCleanupThreshold, true
	}
	return 0, false
}

// isLowSNRCandidate reports whether r should be pruned by the low-SNR worker,
// ignoring its age, which the caller checks.
func isLowSNRCandidate(r *imageRecord) bool {
	// Only filter records where SNR is known (non-zero count).
	// Old sidecars without SNR data are left alone.
	if r.SNR.Count <= 0 {
		return false
	}
	threshold, ok := snrThresholdFor(r.AudioProtocol)
	if !ok {
		return false
	}
	return float64(r.SNR.AvgDB) < threshold
}

// startPartialCleanup runs a ticker every 5 minutes and deletes images that
// are older than keepDays and have fewer than 95% of their expected lines
// decoded (i.e. the transmission was cut short / signal was lost mid-frame).
// keepDays == 0 disables the worker.
func startPartialCleanup(store *imageStore, outputDir string, keepDays int) {
	if keepDays <= 0 {
		return
	}
	log.Printf("cleanup: partial-image worker started (delete after %d day(s), check every 5 min)", keepDays)
	go func() {
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			runPartialCleanup(store, outputDir, keepDays)
		}
	}()
}

// startSNRCleanup runs a ticker every 5 minutes and deletes images that are
// older than keepDays and have a known average SNR below snrCleanupThreshold.
// keepDays == 0 disables the worker.
func startSNRCleanup(store *imageStore, outputDir string, keepDays int) {
	if keepDays <= 0 {
		return
	}
	log.Printf("cleanup: low-SNR worker started (delete <%.1f dB after %d day(s), check every 5 min; "+
		"images recorded before the audio protocol v4 migration are on the old S/N0 scale and are never pruned by SNR)",
		snrCleanupThreshold, keepDays)
	go func() {
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			runSNRCleanup(store, outputDir, keepDays)
		}
	}()
}

// startAgeCleanup runs a ticker every 5 minutes and deletes ALL images that
// are older than keepDays, regardless of quality.  This acts as a general
// retention limit — useful for keeping disk usage bounded on long-running
// deployments.
// keepDays == 0 disables the worker.
func startAgeCleanup(store *imageStore, outputDir string, keepDays int) {
	if keepDays <= 0 {
		return
	}
	log.Printf("cleanup: age worker started (delete all images after %d day(s), check every 5 min)", keepDays)
	go func() {
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			runAgeCleanup(store, outputDir, keepDays)
		}
	}()
}

// runPartialCleanup performs one pass of the partial-image cleanup.
// An image is considered partial when its line count is less than 95% of the
// expected height for its configured image width (using the standard IOC-576
// aspect ratio as a proxy).  Images without line data are left alone.
func runPartialCleanup(store *imageStore, outputDir string, keepDays int) {
	cutoff := time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour)

	store.mu.RLock()
	var candidates []*imageRecord
	for _, r := range store.records {
		if r.SavedAt.After(cutoff) {
			continue // too recent — skip
		}
		// Use a fixed expected-height proxy: for IOC-576 (1809 px wide) a
		// typical complete image is ~1200 lines; for IOC-288 (~904 px) ~600.
		// We simply require at least 500 lines (minSaveRows) as the floor and
		// flag anything below 95% of a reasonable expected height.
		// If Lines == 0 the sidecar predates this field — leave it alone.
		if r.Lines > 0 {
			expectedLines := 1200
			if r.Width > 0 && r.Width < 1000 {
				expectedLines = 600
			}
			if float64(r.Lines) < float64(expectedLines)*0.95 {
				candidates = append(candidates, r)
			}
		}
	}
	store.mu.RUnlock()

	if len(candidates) == 0 {
		return
	}
	log.Printf("cleanup: partial-image pass — %d image(s) older than %d day(s)", len(candidates), keepDays)
	for _, rec := range candidates {
		deleteRecordFiles(store, outputDir, rec, "partial")
	}
}

// runSNRCleanup performs one pass of the low-SNR cleanup.
func runSNRCleanup(store *imageStore, outputDir string, keepDays int) {
	cutoff := time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour)

	store.mu.RLock()
	var candidates []*imageRecord
	for _, r := range store.records {
		if r.SavedAt.After(cutoff) {
			continue // too recent — skip
		}
		if isLowSNRCandidate(r) {
			candidates = append(candidates, r)
		}
	}
	store.mu.RUnlock()

	if len(candidates) == 0 {
		return
	}
	log.Printf("cleanup: low-SNR pass — %d image(s) older than %d day(s)", len(candidates), keepDays)
	for _, rec := range candidates {
		threshold, _ := snrThresholdFor(rec.AudioProtocol)
		deleteRecordFiles(store, outputDir, rec, fmt.Sprintf("SNR %.1f dB < %.1f dB", rec.SNR.AvgDB, threshold))
	}
}

// runAgeCleanup performs one pass of the age-based cleanup.
func runAgeCleanup(store *imageStore, outputDir string, keepDays int) {
	cutoff := time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour)

	store.mu.RLock()
	var candidates []*imageRecord
	for _, r := range store.records {
		if r.SavedAt.Before(cutoff) {
			candidates = append(candidates, r)
		}
	}
	store.mu.RUnlock()

	if len(candidates) == 0 {
		return
	}
	log.Printf("cleanup: age pass — %d image(s) older than %d day(s)", len(candidates), keepDays)
	for _, rec := range candidates {
		deleteRecordFiles(store, outputDir, rec, fmt.Sprintf("older than %d days", keepDays))
	}
}

// deleteRecordFiles removes the image, thumbnail, and JSON sidecar for rec
// from disk, then removes it from the in-memory store and broadcasts a delete
// SSE event so all open browser tabs update immediately.
// reason is a short string used only for the log line.
func deleteRecordFiles(store *imageStore, outputDir string, rec *imageRecord, reason string) {
	id := rec.ID

	// Remove image and thumbnail files.
	for _, name := range []string{rec.Filename, rec.ThumbFile} {
		if name == "" {
			continue
		}
		p := filepath.Join(outputDir, name)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("cleanup: remove %s: %v", p, err)
		}
	}

	// Remove the JSON sidecar — try the base-name-derived path first, then
	// fall back to a directory scan (same strategy as the DELETE HTTP handler).
	sidecarRemoved := false
	if rec.Filename != "" {
		base := strings.TrimSuffix(rec.Filename, filepath.Ext(rec.Filename))
		candidate := filepath.Join(outputDir, base+".json")
		if err := os.Remove(candidate); err == nil {
			sidecarRemoved = true
		}
	}
	if !sidecarRemoved {
		if entries, err := os.ReadDir(outputDir); err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				p := filepath.Join(outputDir, e.Name())
				data, err := os.ReadFile(p)
				if err != nil {
					continue
				}
				var tmp struct {
					ID string `json:"id"`
				}
				if json.Unmarshal(data, &tmp) == nil && tmp.ID == id {
					os.Remove(p) //nolint:errcheck
					break
				}
			}
		}
	}

	// Remove from in-memory store and mark as deleted so loadExisting() on
	// restart cannot re-insert this record.
	store.mu.Lock()
	store.deleted[id] = struct{}{}
	delete(store.byID, id)
	filtered := make([]*imageRecord, 0, len(store.records)-1)
	for _, r := range store.records {
		if r.ID != id {
			filtered = append(filtered, r)
		}
	}
	store.records = filtered
	store.mu.Unlock()

	// Broadcast a delete SSE event so every open browser tab removes the card.
	store.hub.broadcast(sseEvent{
		Event: "image_deleted",
		Data:  map[string]string{"id": id},
	})

	log.Printf("cleanup: deleted %s (%s)", id, reason)
}

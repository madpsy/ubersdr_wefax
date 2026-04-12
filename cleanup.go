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
//   startSNRCleanup      — removes images whose average SNR is known and
//                          below 40 dB.
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
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	cleanupInterval = 5 * time.Minute
	// snrCleanupThreshold is the minimum acceptable average SNR.
	// Images with a known SNR below this value are pruned by startSNRCleanup.
	snrCleanupThreshold = 40.0
)

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
	log.Printf("cleanup: low-SNR worker started (delete <%.0f dB after %d day(s), check every 5 min)", snrCleanupThreshold, keepDays)
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
		// Only filter records where SNR is known (non-zero count).
		// Old sidecars without SNR data are left alone.
		if r.SNR.Count > 0 && float64(r.SNR.AvgDB) < snrCleanupThreshold {
			candidates = append(candidates, r)
		}
	}
	store.mu.RUnlock()

	if len(candidates) == 0 {
		return
	}
	log.Printf("cleanup: low-SNR pass — %d image(s) older than %d day(s)", len(candidates), keepDays)
	for _, rec := range candidates {
		deleteRecordFiles(store, outputDir, rec, fmt.Sprintf("SNR %.1f dB < %.0f dB", rec.SNR.AvgDB, snrCleanupThreshold))
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

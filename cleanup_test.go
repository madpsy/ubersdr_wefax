package main

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These tests guard a silent data-loss path.
//
// startSNRCleanup permanently deletes images -- the PNG, the thumbnail, the
// sidecar, and the store entry -- whose average SNR falls below a fixed
// threshold. That threshold was calibrated while this addon read version 2
// audio headers, whose noise field was a spectral DENSITY in dBFS/Hz, making
// `baseband - noise` an S/N0 in dB·Hz rather than an SNR. Version 3 changed the
// field to the noise power inside the demodulator passband, so the same
// subtraction now reads about 34.7 dB lower for the same signal.
//
// Nothing about that change is visible at runtime: no error, no warning, no
// decode failure. A threshold left at 40 simply starts condemning every image
// the receiver produces, and the deletions land CLEANUP_SNR_DAYS later, by
// which point the cause is a week in the past. These tests are the only thing
// standing between that change and the user's images.

// The constants below are MEASURED, not assumed. Captured from
// m9psy.tunnel.ubersdr.org at 12 kHz USB, ~1500 packets per frequency, using
// this addon's own decoder and SNR arithmetic. They matter because the server's
// noise no longer bottoms out near -30, so the observed distribution is the
// only trustworthy calibration.

// snrDBGoodV4 is a strong fax: 7880 kHz measured a median of 32.67 dB with 6 s
// windowed means of 29.40 .. 32.88. 30 is the conservative end of that.
const snrDBGoodV4 = 30.0

// snrDBMarginalV4 is a weak but plausibly decodable signal, between the two
// observed populations. Nothing measured here sat at 12 dB for a whole image,
// but a fading fax would, and the worker must keep it.
const snrDBMarginalV4 = 12.0

// snrDBDeadChannelV4 is an empty channel: 4610 and 13500 kHz measured 6 s means
// of -0.65 .. +0.57. 0.5 is the worst (highest) of those, i.e. the hardest dead
// channel to distinguish from signal.
const snrDBDeadChannelV4 = 0.5

func recordWithSNR(id string, protocol int, avgDB float32, savedAt time.Time) *imageRecord {
	return &imageRecord{
		ID:            id,
		Label:         "7880000_usb",
		Filename:      id + ".png",
		ThumbFile:     id + "_thumb.png",
		SavedAt:       savedAt,
		Lines:         1200,
		Width:         1809,
		AudioProtocol: protocol,
		SNR:           SNRStats{Count: 600, AvgDB: avgDB},
	}
}

// The rescale itself. If someone restores the literal 40.0 -- the single edit
// that reintroduces the bug -- this fails first and says why.
func TestSNRThresholdIsOnThePassbandScale(t *testing.T) {
	// 10*log10(2950 Hz), the server's usb filter width.
	const wantShift = 34.69822
	if math.Abs(snrScaleShiftDB-wantShift) > 0.001 {
		t.Errorf("snrScaleShiftDB = %.5f, want %.5f (10*log10(%.0f))",
			snrScaleShiftDB, wantShift, usbFilterBandwidthHz)
	}

	const wantThreshold = legacySNRCleanupThreshold - wantShift // ≈ 5.302
	if math.Abs(snrCleanupThreshold-wantThreshold) > 0.001 {
		t.Errorf("snrCleanupThreshold = %.5f, want %.5f", snrCleanupThreshold, wantThreshold)
	}

	// The bug, stated directly: the old threshold on the new scale deletes
	// good images. If snrCleanupThreshold ever drifts back up near it, every
	// image this receiver records is condemned.
	if snrCleanupThreshold > snrDBGoodV4 {
		t.Fatalf("threshold %.2f dB is above a good v4 signal (%.1f dB) -- "+
			"this configuration deletes every image the receiver produces",
			snrCleanupThreshold, snrDBGoodV4)
	}
}

// The threshold has to fall in the gap between the two populations that were
// actually measured on the live receiver, not merely be arithmetically derived.
// This is the test that would catch a threshold which is correct in its units
// and still useless in practice.
func TestThresholdSeparatesTheMeasuredPopulations(t *testing.T) {
	// Empty channels measured 6 s means of -0.65 .. +0.57 dB.
	for _, dead := range []float32{-0.65, -0.38, -0.12, 0.57} {
		if dead >= float32(snrCleanupThreshold) {
			t.Errorf("a measured empty channel (%.2f dB) is at or above the threshold "+
				"%.2f dB, so dead air would be kept", dead, snrCleanupThreshold)
		}
	}
	// A strong fax measured 6 s means of 29.40 .. 32.88 dB, p05 23.99.
	for _, good := range []float32{23.99, 29.40, 32.88, 42.40} {
		if good <= float32(snrCleanupThreshold) {
			t.Errorf("a measured fax signal (%.2f dB) is at or below the threshold "+
				"%.2f dB, so real images would be deleted", good, snrCleanupThreshold)
		}
	}

	// And it must sit nearer the noise end of the gap: this deletes files, so a
	// marginal signal is kept rather than pruned.
	midpoint := (0.0 + 30.0) / 2
	if snrCleanupThreshold > midpoint {
		t.Errorf("threshold %.2f dB is above the midpoint (%.1f dB) of the measured "+
			"0 dB / 30 dB populations; a destructive worker should err toward keeping",
			snrCleanupThreshold, midpoint)
	}
}

// A marginal fax -- weak, fading, but real -- must survive. It sits between the
// two measured populations, which is exactly where a badly placed threshold
// does its damage.
func TestMarginalSignalIsKept(t *testing.T) {
	rec := recordWithSNR("marginal", 4, snrDBMarginalV4, time.Now().Add(-30*24*time.Hour))
	if isLowSNRCandidate(rec) {
		t.Errorf("a marginal but real signal (%.1f dB) was selected for deletion at "+
			"threshold %.2f dB", rec.SNR.AvgDB, snrCleanupThreshold)
	}
}

// The regression this whole file exists for: a good image recorded after the
// migration must survive, even though its number is far below the threshold
// the old code used.
func TestGoodV4ImageIsNotDeleted(t *testing.T) {
	rec := recordWithSNR("good", 4, snrDBGoodV4, time.Now().Add(-30*24*time.Hour))

	// Establish that this record really is in the danger zone: the pre-migration
	// code would have deleted it. Without this the test could pass against a
	// threshold that never moved.
	if float64(rec.SNR.AvgDB) >= legacySNRCleanupThreshold {
		t.Fatalf("test is not exercising the bug: %.1f dB is not below the old threshold %.1f",
			rec.SNR.AvgDB, legacySNRCleanupThreshold)
	}

	if isLowSNRCandidate(rec) {
		t.Errorf("a good v4-scale image (%.1f dB, threshold %.2f dB) was selected for deletion",
			rec.SNR.AvgDB, snrCleanupThreshold)
	}
}

// The worker still has to do its job, or the rescale has just disabled it.
func TestNoiseV4ImageIsStillDeleted(t *testing.T) {
	rec := recordWithSNR("bad", 4, snrDBDeadChannelV4, time.Now().Add(-30*24*time.Hour))
	if !isLowSNRCandidate(rec) {
		t.Errorf("a noise-floor v4 image (%.1f dB, threshold %.2f dB) was not selected; "+
			"the low-SNR worker has stopped working", rec.SNR.AvgDB, snrCleanupThreshold)
	}
}

// Pre-migration records carry SNR on the old S/N0 scale and no marker saying
// so. They must never be judged against the new threshold -- and this build
// declines to judge them at all.
func TestPreMigrationRecordsAreNeverDeleted(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour)

	cases := []struct {
		name  string
		avgDB float32
	}{
		// Good on the old scale. Sits far above both thresholds; safe either way.
		{"good on the v2 scale", 45},
		// The dangerous one: below the OLD threshold, above the new one. A
		// build that judged unmarked records by either threshold would have to
		// pick, and picking wrong deletes a good image or keeps a bad one.
		{"below the old threshold", 38},
		// Numerically tiny. On the old scale this is noise, but nothing in the
		// sidecar proves the scale, so it is still not ours to delete.
		{"numerically low", 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := recordWithSNR("legacy", 0, tc.avgDB, old)
			if _, ok := snrThresholdFor(rec.AudioProtocol); ok {
				t.Fatal("a threshold was offered for an unmarked record")
			}
			if isLowSNRCandidate(rec) {
				t.Errorf("a pre-migration record (%.1f dB, unknown scale) was selected for deletion",
					rec.SNR.AvgDB)
			}
		})
	}
}

// Records with no SNR measurement at all are left alone, as before.
func TestRecordsWithoutSNRAreNotDeleted(t *testing.T) {
	rec := recordWithSNR("nosnr", 4, 0, time.Now().Add(-30*24*time.Hour))
	rec.SNR.Count = 0
	if isLowSNRCandidate(rec) {
		t.Error("a record with no SNR samples was selected for deletion")
	}
}

// End to end through the real worker, against real files: the thing that
// actually deletes must delete exactly one of these three.
func TestRunSNRCleanupDeletesOnlyTheNoiseImage(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-30 * 24 * time.Hour)

	good := recordWithSNR("good", 4, snrDBGoodV4, old)
	bad := recordWithSNR("bad", 4, snrDBDeadChannelV4, old)
	legacy := recordWithSNR("legacy", 0, 12, old) // pre-migration, old scale
	recent := recordWithSNR("recent", 4, snrDBDeadChannelV4, time.Now())

	store := &imageStore{
		records:   []*imageRecord{good, bad, legacy, recent},
		byID:      map[string]*imageRecord{},
		deleted:   map[string]struct{}{},
		hub:       newSSEHub(),
		outputDir: dir,
	}
	for _, r := range store.records {
		store.byID[r.ID] = r
		for _, name := range []string{r.Filename, r.ThumbFile} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("png"), 0o644); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
		}
	}

	runSNRCleanup(store, dir, 7)

	survives := map[string]bool{"good": true, "bad": false, "legacy": true, "recent": true}
	for id, want := range survives {
		_, err := os.Stat(filepath.Join(dir, id+".png"))
		got := err == nil
		if got != want {
			if want {
				t.Errorf("%s.png was DELETED and should have survived", id)
			} else {
				t.Errorf("%s.png survived and should have been deleted", id)
			}
		}
		if _, inStore := store.byID[id]; inStore != want {
			t.Errorf("%s: in store = %v, want %v", id, inStore, want)
		}
	}
}

// The marker has to be written by the real save path and to survive a round
// trip through the sidecar, because the sidecar is what the store reads back
// after a restart. If saveImage stops stamping, every new record reads as
// pre-migration: the low-SNR worker quietly stops pruning anything, forever,
// and no other test would notice.
func TestSaveImageStampsTheAudioProtocol(t *testing.T) {
	dir := t.TempDir()
	store := &imageStore{
		byID:      map[string]*imageRecord{},
		deleted:   map[string]struct{}{},
		hub:       newSSEHub(),
		outputDir: dir,
	}

	rows := make([][]byte, 4)
	for i := range rows {
		rows[i] = make([]byte, 8)
	}
	img := &inProgressImage{
		rows:      rows,
		startedAt: time.Now().Add(-time.Minute),
		freqHz:    7880000,
		audioMode: "usb",
		label:     "7880000_usb",
		snr:       SNRStats{Count: 600, AvgDB: snrDBGoodV4},
	}

	if err := saveImage(img, store, store.hub); err != nil {
		t.Fatalf("saveImage: %v", err)
	}

	// In memory.
	if len(store.records) != 1 {
		t.Fatalf("store holds %d records, want 1", len(store.records))
	}
	saved := store.records[0]
	if saved.AudioProtocol < 3 {
		t.Fatalf("saveImage stamped audio_protocol = %d; records on the passband-SNR "+
			"scale must be marked >= 3 or the low-SNR worker skips them forever",
			saved.AudioProtocol)
	}

	// And on disk, which is what survives a restart.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var sidecar string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			sidecar = filepath.Join(dir, e.Name())
		}
	}
	if sidecar == "" {
		t.Fatal("saveImage wrote no JSON sidecar")
	}
	data, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var readBack imageRecord
	if err := json.Unmarshal(data, &readBack); err != nil {
		t.Fatalf("unmarshal sidecar: %v", err)
	}
	if readBack.AudioProtocol != saved.AudioProtocol {
		t.Errorf("sidecar audio_protocol = %d, want %d", readBack.AudioProtocol, saved.AudioProtocol)
	}
	if _, ok := snrThresholdFor(readBack.AudioProtocol); !ok {
		t.Error("a record round-tripped through the sidecar reads as pre-migration")
	}

	// The saved image is a good one; the worker must not want it.
	if isLowSNRCandidate(&readBack) {
		t.Errorf("a freshly saved good image (%.1f dB) is already a deletion candidate",
			readBack.SNR.AvgDB)
	}
}

// ---------------------------------------------------------------------------
// Per-line SNR on the wire
// ---------------------------------------------------------------------------

// On the passband-SNR scale 0 dB and below are ordinary readings from a weak
// signal, so 0 can no longer double as "no measurement". handleImageLine sends
// null instead, and the client tests for null rather than for > 0. If this
// regresses, every genuinely weak line is silently redrawn with the channel
// average instead of its own value.
func TestFaxLineSNRIsNullOnlyWhenUnmeasured(t *testing.T) {
	inst := newInstance(7880000, 1900, "usb", "ws://example.invalid/ws", "")
	ch := newWefaxChannel(inst, WEFAXConfig{}, &imageStore{
		byID:    map[string]*imageRecord{},
		deleted: map[string]struct{}{},
		hub:     newSSEHub(),
	}, newSSEHub())
	ch.currentImg = &inProgressImage{label: ch.label}

	// One image line: type byte, line number, width, then the pixels.
	msg := make([]byte, 9+4)
	binary.BigEndian.PutUint32(msg[1:5], 1)
	binary.BigEndian.PutUint32(msg[5:9], 4)

	lineSNR := func() (interface{}, bool) {
		sub := ch.hub.subscribe(ch.label)
		defer ch.hub.unsubscribe(sub)
		ch.handleImageLine(msg)
		for {
			select {
			case ev := <-sub.ch:
				if ev.Event != "fax_line" {
					continue
				}
				data, ok := ev.Data.(map[string]interface{})
				if !ok {
					t.Fatalf("fax_line Data is %T", ev.Data)
				}
				v, present := data["snr_db"]
				return v, present
			case <-time.After(2 * time.Second):
				t.Fatal("no fax_line event")
				return nil, false
			}
		}
	}

	// No measurement yet: the field is present but null.
	v, present := lineSNR()
	if !present {
		t.Fatal("fax_line carried no snr_db field at all")
	}
	if v != nil {
		t.Errorf("snr_db = %v with no measurement, want nil "+
			"(0 is a real reading on the passband-SNR scale and must not be the sentinel)", v)
	}

	// A real measurement of exactly 0 dB must come through as 0, not as null
	// and not be suppressed. baseband == noise is 0 dB SNR.
	inst.snrAccum.add(-40, -40)
	v, _ = lineSNR()
	f, ok := v.(float32)
	if !ok {
		t.Fatalf("snr_db = %v (%T) for a real 0 dB measurement, want a float32 0", v, v)
	}
	if f != 0 {
		t.Errorf("snr_db = %v, want 0", f)
	}

	// And a negative one, which the old `> 0` client check would have dropped.
	inst.snrAccum.drain()
	inst.snrAccum.add(-50, -44) // -6 dB
	v, _ = lineSNR()
	if f, ok := v.(float32); !ok || f >= 0 {
		t.Errorf("snr_db = %v for a -6 dB measurement, want a negative float32", v)
	}
}

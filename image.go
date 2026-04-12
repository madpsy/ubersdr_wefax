// image.go — image assembler: save completed fax images as PNG + JSON sidecar,
// maintain the in-memory imageStore, and notify SSE clients.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// imageRecord — metadata for one completed fax image
// ---------------------------------------------------------------------------

type imageRecord struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	FreqHz    int       `json:"freq_hz"`
	AudioMode string    `json:"audio_mode"`
	StartedAt time.Time `json:"started_at"`
	SavedAt   time.Time `json:"saved_at"`
	Lines     int       `json:"lines"`
	Width     int       `json:"width"`
	Filename  string    `json:"filename"`   // PNG filename (basename)
	ThumbFile string    `json:"thumb_file"` // thumbnail filename (basename)
	SNR       SNRStats  `json:"snr"`        // signal quality over the receive
}

// ---------------------------------------------------------------------------
// imageStore — thread-safe in-memory list of completed images
// ---------------------------------------------------------------------------

type imageStore struct {
	mu        sync.RWMutex
	records   []*imageRecord // newest first
	byID      map[string]*imageRecord
	deleted   map[string]struct{} // IDs removed by cleanup; prevents re-insertion on restart
	hub       *sseHub             // for broadcasting delete events from cleanup workers
	outputDir string
}

func newImageStore(outputDir string, hub *sseHub) *imageStore {
	s := &imageStore{
		byID:      make(map[string]*imageRecord),
		deleted:   make(map[string]struct{}),
		hub:       hub,
		outputDir: outputDir,
	}
	s.loadExisting()
	return s
}

// loadExisting scans outputDir for *.json sidecar files and populates the store.
func (s *imageStore) loadExisting() {
	entries, err := os.ReadDir(s.outputDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[imageStore] readdir %s: %v", s.outputDir, err)
		}
		return
	}
	var loaded []*imageRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.outputDir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("[imageStore] read %s: %v", path, err)
			continue
		}
		if len(data) == 0 {
			log.Printf("[imageStore] skip empty sidecar %s", e.Name())
			continue
		}
		var rec imageRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			log.Printf("[imageStore] skip corrupt sidecar %s: %v", e.Name(), err)
			continue
		}
		// Sanitise any NaN/Inf SNR fields that may have been written by older
		// versions of the code before the NaN guard was added.
		rec.SNR.Sanitise()
		// Skip records that were deleted by a cleanup worker before restart.
		if _, wasDel := s.deleted[rec.ID]; wasDel {
			continue
		}
		// Verify the PNG still exists.
		if _, err := os.Stat(filepath.Join(s.outputDir, rec.Filename)); err != nil {
			continue
		}
		loaded = append(loaded, &rec)
	}
	// Sort newest first.
	sort.Slice(loaded, func(i, j int) bool {
		return loaded[i].SavedAt.After(loaded[j].SavedAt)
	})
	s.mu.Lock()
	for _, rec := range loaded {
		s.records = append(s.records, rec)
		s.byID[rec.ID] = rec
	}
	s.mu.Unlock()
	log.Printf("[imageStore] loaded %d existing images from %s", len(loaded), s.outputDir)
}

// add inserts a new record at the front (newest first).
func (s *imageStore) add(rec *imageRecord) {
	s.mu.Lock()
	s.records = append([]*imageRecord{rec}, s.records...)
	s.byID[rec.ID] = rec
	s.mu.Unlock()
}

// list returns a copy of all records (newest first), optionally filtered by label.
func (s *imageStore) list(label string, limit, offset int) []*imageRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*imageRecord
	for _, r := range s.records {
		if label != "" && r.Label != label {
			continue
		}
		out = append(out, r)
	}
	if offset >= len(out) {
		return nil
	}
	out = out[offset:]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// get returns a record by ID.
func (s *imageStore) get(id string) (*imageRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.byID[id]
	return r, ok
}

// delete removes a record and its files from disk.
func (s *imageStore) delete(id string) error {
	s.mu.Lock()
	rec, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("record %s not found", id)
	}
	delete(s.byID, id)
	for i, r := range s.records {
		if r.ID == id {
			s.records = append(s.records[:i], s.records[i+1:]...)
			break
		}
	}
	s.mu.Unlock()

	// Remove files (best-effort).
	for _, name := range []string{rec.Filename, rec.ThumbFile, strings.TrimSuffix(rec.Filename, ".png") + ".json"} {
		if name == "" {
			continue
		}
		_ = os.Remove(filepath.Join(s.outputDir, name))
	}
	return nil
}

// ---------------------------------------------------------------------------
// saveImage — write PNG + thumbnail + JSON sidecar, then notify store + SSE
// ---------------------------------------------------------------------------

func saveImage(img *inProgressImage, store *imageStore, hub *sseHub) error {
	if len(img.rows) == 0 {
		return fmt.Errorf("no rows to save")
	}

	if err := os.MkdirAll(store.outputDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", store.outputDir, err)
	}

	id := uuid.New().String()
	ts := time.Now()
	base := fmt.Sprintf("%s_%s", ts.UTC().Format("20060102_150405"), id[:8])

	width := len(img.rows[0])
	height := len(img.rows)

	// Build grayscale PNG.
	gray := image.NewGray(image.Rect(0, 0, width, height))
	for y, row := range img.rows {
		for x, px := range row {
			if x < width {
				gray.SetGray(x, y, color.Gray{Y: px})
			}
		}
	}

	pngName := base + ".png"
	pngPath := filepath.Join(store.outputDir, pngName)
	f, err := os.Create(pngPath)
	if err != nil {
		return fmt.Errorf("create png: %w", err)
	}
	if err := png.Encode(f, gray); err != nil {
		f.Close()
		return fmt.Errorf("encode png: %w", err)
	}
	f.Close()

	// Build thumbnail (max 400 px wide, proportional height).
	thumbName := base + "_thumb.png"
	thumbPath := filepath.Join(store.outputDir, thumbName)
	if err := saveThumbnail(gray, thumbPath, 400); err != nil {
		log.Printf("[saveImage] thumbnail: %v", err)
		thumbName = "" // non-fatal
	}

	rec := &imageRecord{
		ID:        id,
		Label:     img.label,
		FreqHz:    img.freqHz,
		AudioMode: img.audioMode,
		StartedAt: img.startedAt,
		SavedAt:   ts,
		Lines:     height,
		Width:     width,
		Filename:  pngName,
		ThumbFile: thumbName,
		SNR:       img.snr,
	}

	// Write JSON sidecar atomically: marshal → temp file → rename, so a
	// mid-write kill never leaves a zero-byte or truncated sidecar.
	jsonPath := filepath.Join(store.outputDir, base+".json")
	jdata, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		log.Printf("[saveImage] marshal json: %v", err)
	} else {
		tmpPath := jsonPath + ".tmp"
		if err := os.WriteFile(tmpPath, jdata, 0o644); err != nil {
			log.Printf("[saveImage] write json tmp: %v", err)
		} else if err := os.Rename(tmpPath, jsonPath); err != nil {
			log.Printf("[saveImage] rename json: %v", err)
			_ = os.Remove(tmpPath)
		}
	}

	store.add(rec)
	log.Printf("[saveImage] saved %s (%dx%d)", pngName, width, height)

	// Notify SSE clients.
	hub.broadcast(sseEvent{
		Event: "new_image",
		Data:  rec,
	})

	return nil
}

// saveThumbnail writes a downscaled grayscale PNG.
func saveThumbnail(src *image.Gray, path string, maxWidth int) error {
	srcW := src.Bounds().Dx()
	srcH := src.Bounds().Dy()
	if srcW == 0 || srcH == 0 {
		return fmt.Errorf("empty source image")
	}

	dstW := srcW
	dstH := srcH
	if dstW > maxWidth {
		dstH = dstH * maxWidth / dstW
		dstW = maxWidth
	}

	thumb := image.NewGray(image.Rect(0, 0, dstW, dstH))
	for y := 0; y < dstH; y++ {
		for x := 0; x < dstW; x++ {
			srcX := x * srcW / dstW
			srcY := y * srcH / dstH
			thumb.SetGray(x, y, src.GrayAt(srcX, srcY))
		}
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, thumb)
}

// encodePixelsB64 base64-encodes a raw pixel row for SSE transport.
func encodePixelsB64(pixels []byte) string {
	return base64.StdEncoding.EncodeToString(pixels)
}

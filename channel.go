// channel.go — per-frequency wefax channel: wires instance → WEFAXDecoder → imageStore
package main

import (
	"context"
	"encoding/binary"
	"log"
	"sync"
	"time"
)

// wefaxChannel owns one UberSDR instance and one WEFAXDecoder.
// It converts raw PCM bytes → []int16 → decoder → image assembler.
type wefaxChannel struct {
	inst    *instance
	decoder *WEFAXDecoder
	label   string // same as inst.label

	// resultChan receives raw decoder messages (MsgImageLine / MsgStart / MsgStop)
	resultChan chan []byte

	// audioChan receives []int16 slices for the decoder
	audioChan chan []int16

	store *imageStore
	hub   *sseHub

	mu          sync.Mutex
	currentImg  *inProgressImage // nil when not receiving
	decoding    bool             // true while an image is in progress
	cancelAssem context.CancelFunc
}

// inProgressImage accumulates pixel rows for one fax image.
type inProgressImage struct {
	rows      [][]byte  // one entry per emitted output line
	startedAt time.Time
	freqHz    int
	audioMode string
	label     string
	snr       SNRStats  // drained from instance at STOP time
	startSeen bool      // true only when a real START tone opened this image
}

func newWefaxChannel(inst *instance, cfg WEFAXConfig, store *imageStore, hub *sseHub) *wefaxChannel {
	return &wefaxChannel{
		inst:       inst,
		label:      inst.label,
		resultChan: make(chan []byte, 512),
		audioChan:  make(chan []int16, 256),
		store:      store,
		hub:        hub,
	}
}

// run starts the channel and blocks until ctx is cancelled.
func (c *wefaxChannel) run(ctx context.Context, cfg WEFAXConfig) {
	// We need the sample rate before we can create the decoder.
	// Start the instance first, then wait for the first packet to learn the rate.
	go c.inst.start(ctx)

	// Wait for first audio packet to determine sample rate.
	var firstChunk []byte
	select {
	case <-ctx.Done():
		return
	case chunk, ok := <-c.inst.AudioCh:
		if !ok {
			return
		}
		firstChunk = chunk
	}

	c.inst.streamMu.RLock()
	sampleRate := c.inst.streamSampleRate
	c.inst.streamMu.RUnlock()

	if sampleRate == 0 {
		sampleRate = 8000 // fallback
	}

	log.Printf("[%s] creating WEFAX decoder at %d Hz", c.label, sampleRate)
	c.decoder = NewWEFAXDecoder(sampleRate, cfg)

	if err := c.decoder.Start(c.audioChan, c.resultChan); err != nil {
		log.Printf("[%s] decoder start: %v", c.label, err)
		return
	}
	defer c.decoder.Stop()

	// Always open an in-progress image immediately so lines are captured from
	// the very first packet, regardless of whether a START tone has been heard.
	// The decoder now starts with autoStarted=true so lines flow right away.
	// When a real START tone is detected, handleStart() will discard the
	// partial and begin a fresh image; a STOP tone saves the image and resets
	// the decoder to wait for the next START — preserving normal boundaries.
	// Use silent=true so the frontend badge doesn't flash blue at startup.
	log.Printf("[%s] opening image immediately (lines flow from first packet)", c.label)
	c.openImageSilently()

	// Start the image assembler goroutine.
	go c.assembleImages(ctx)

	// Feed the first chunk then keep pumping.
	c.feedPCM(firstChunk)
	for {
		select {
		case <-ctx.Done():
			return
		case chunk, ok := <-c.inst.AudioCh:
			if !ok {
				return
			}
			c.feedPCM(chunk)
		}
	}
}

// feedPCM converts a raw S16LE byte slice to []int16 and sends it to the decoder.
func (c *wefaxChannel) feedPCM(raw []byte) {
	n := len(raw) / 2
	if n == 0 {
		return
	}
	samples := make([]int16, n)
	for i := 0; i < n; i++ {
		samples[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	select {
	case c.audioChan <- samples:
	default:
		log.Printf("[%s] audioChan full, dropping PCM chunk", c.label)
	}
}

// assembleImages reads from resultChan and builds complete fax images.
func (c *wefaxChannel) assembleImages(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-c.resultChan:
			if !ok {
				return
			}
			if len(msg) == 0 {
				continue
			}
			switch msg[0] {
			case MsgStart:
				c.handleStart() // real START tone — notify frontend
			case MsgStop:
				c.handleStop()
			case MsgImageLine:
				c.handleImageLine(msg)
			}
		}
	}
}

// openImageSilently opens a new in-progress image without broadcasting a
// channel_start SSE event.  Used at startup so badges don't flash blue
// before any real signal has been detected.
func (c *wefaxChannel) openImageSilently() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.currentImg = &inProgressImage{
		startedAt: time.Now(),
		freqHz:    c.inst.freqHz,
		audioMode: c.inst.audioMode,
		label:     c.label,
	}
	// Do NOT set c.decoding = true here — the badge should only go blue
	// when a real START tone is detected.
}

func (c *wefaxChannel) handleStart() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.currentImg != nil {
		log.Printf("[%s] START received while image in progress — discarding partial (%d rows)", c.label, len(c.currentImg.rows))
	}
	c.currentImg = &inProgressImage{
		startedAt: time.Now(),
		freqHz:    c.inst.freqHz,
		audioMode: c.inst.audioMode,
		label:     c.label,
		startSeen: true, // real START tone — image is eligible to be saved
	}
	c.decoding = true
	log.Printf("[%s] START — new image begun", c.label)

	// Notify SSE clients of a new live image starting.
	c.hub.broadcast(sseEvent{
		Event: "channel_start",
		Data:  map[string]interface{}{"label": c.label, "freq_hz": c.inst.freqHz},
	})
}

func (c *wefaxChannel) handleStop() {
	c.mu.Lock()
	img := c.currentImg
	c.currentImg = nil
	c.decoding = false
	c.mu.Unlock()

	// Always emit channel_stop so the frontend badge updates.
	c.hub.broadcast(sseEvent{
		Event: "channel_stop",
		Data:  map[string]interface{}{"label": c.label, "freq_hz": c.inst.freqHz},
	})

	if img == nil {
		log.Printf("[%s] STOP received but no image in progress", c.label)
		return
	}

	if !img.startSeen {
		// No real START tone was ever received for this image — discard it.
		// (Lines were flowing from startup but the image boundary was never
		// established, so saving would produce a partial/garbage image.)
		log.Printf("[%s] STOP received but no START was seen — discarding %d rows", c.label, len(img.rows))
		// Open a fresh silent buffer so lines continue to be captured until
		// the next real START tone arrives.
		c.openImageSilently()
		return
	}

	// Drain accumulated SNR for this image.
	img.snr = c.inst.DrainSNR()
	log.Printf("[%s] STOP — saving image with %d rows, SNR avg=%.1f dB (n=%d)",
		c.label, len(img.rows), img.snr.AvgDB, img.snr.Count)

	go func() {
		if err := saveImage(img, c.store, c.hub); err != nil {
			log.Printf("[%s] saveImage: %v", c.label, err)
		}
	}()
}

// snapshotRows returns a copy of the rows accumulated so far in the current
// in-progress image, along with the channel's frequency.  Used by the replay
// endpoint so a newly-connected browser can catch up on a mid-receive image.
func (c *wefaxChannel) snapshotRows() (freqHz int, rows [][]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.currentImg == nil {
		return c.inst.freqHz, nil
	}
	freqHz = c.inst.freqHz
	rows = make([][]byte, len(c.currentImg.rows))
	for i, r := range c.currentImg.rows {
		cp := make([]byte, len(r))
		copy(cp, r)
		rows[i] = cp
	}
	return
}

func (c *wefaxChannel) handleImageLine(msg []byte) {
	// Protocol: [type:1][line:4BE][width:4BE][pixels:width]
	if len(msg) < 9 {
		return
	}
	lineNum := binary.BigEndian.Uint32(msg[1:5])
	width := binary.BigEndian.Uint32(msg[5:9])
	if len(msg) < 9+int(width) {
		return
	}
	pixels := make([]byte, width)
	copy(pixels, msg[9:9+width])

	c.mu.Lock()
	img := c.currentImg
	c.mu.Unlock()

	if img == nil {
		// Receiving lines before START — ignore (autoStart not yet triggered)
		return
	}

	img.rows = append(img.rows, pixels)

	// Broadcast live row to SSE clients.
	c.hub.broadcast(sseEvent{
		Event: "fax_line",
		Data: map[string]interface{}{
			"label":    c.label,
			"freq_hz":  c.inst.freqHz,
			"line":     lineNum,
			"width":    width,
			"pixels_b64": encodePixelsB64(pixels),
		},
	})
}

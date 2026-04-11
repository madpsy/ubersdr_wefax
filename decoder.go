package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"sync"
)

// Bandwidth represents FIR filter bandwidth options
type Bandwidth int

const (
	BandwidthNarrow Bandwidth = 0
	BandwidthMiddle Bandwidth = 1
	BandwidthWide   Bandwidth = 2
)

// HeaderType represents the type of line detected
type HeaderType int

const (
	HeaderImage HeaderType = 0
	HeaderStart HeaderType = 1
	HeaderStop  HeaderType = 2
)

// WEFAXMsg types sent on the result channel
const (
	MsgImageLine byte = 0x01 // [type:1][line:4BE][width:4BE][pixels:width]
	MsgStart     byte = 0x02 // [type:1]
	MsgStop      byte = 0x03 // [type:1]
)

// FIRFilter implements a 17-tap low-pass filter
type FIRFilter struct {
	bandwidth Bandwidth
	buffer    [17]float64
	current   int
}

// NewFIRFilter creates a new FIR filter with the specified bandwidth
func NewFIRFilter(bandwidth Bandwidth) *FIRFilter {
	return &FIRFilter{
		bandwidth: bandwidth,
		current:   0,
	}
}

// Apply applies the FIR filter to a sample
func (f *FIRFilter) Apply(sample float64) float64 {
	// Low pass filter coefficients from ACfax
	lpfCoeff := [3][17]float64{
		{-7, -18, -15, 11, 56, 116, 177, 223, 240, 223, 177, 116, 56, 11, -15, -18, -7}, // Narrow
		{0, -18, -38, -39, 0, 83, 191, 284, 320, 284, 191, 83, 0, -39, -38, -18, 0},     // Middle
		{6, 20, 7, -42, -74, -12, 159, 353, 440, 353, 159, -12, -74, -42, 7, 20, 6},     // Wide
	}

	coeff := lpfCoeff[f.bandwidth]

	f.buffer[f.current] = sample

	sum := 0.0
	idx := f.current
	for i := 0; i < 17; i++ {
		sum += f.buffer[idx] * coeff[i]
		idx++
		if idx >= 17 {
			idx = 0
		}
	}

	f.current--
	if f.current < 0 {
		f.current = 16
	}

	return sum
}

// WEFAXDecoder implements weather fax decoding
type WEFAXDecoder struct {
	// Configuration
	lpm                      int
	imageWidth               int
	bitsPerPixel             int
	carrier                  float64
	deviation                float64
	minusSaturationThreshold float64
	includeHeadersInImages   bool
	usePhasing               bool
	autoStop                 bool
	autoStart                bool
	skipHeaderDetection      bool

	// Sample rate
	samplesPerSecNom      float64
	samplesPerSecFrac     float64
	samplesPerSecFracPrev float64
	sampleRateRatio       float64
	samplesPerLine        int

	// Demodulation state
	firFilters [2]*FIRFilter
	iPrev      float64
	qPrev      float64

	// Sample buffering
	samples   []int16
	sampIdx   int
	fi        float64
	demodData []uint8
	skip      int

	// Image state
	imgData      []uint8
	outImage     []uint8
	imageLine    int
	imageColors  int
	height       int
	imgPos       int
	lineIncrFrac float64
	lineIncrAcc  float64
	lineBlend    float64

	// Header detection
	startIOC576Frequency int
	startIOC288Frequency int
	stopFrequency        int
	startStopLength      int
	lastType             HeaderType
	typeCount            int

	// Phasing
	phasingLines     int
	phasingPos       []int
	phasingLinesLeft int
	phasingSkipData  int
	havePhasing      bool

	// Control
	autoStopped bool
	autoStarted bool
	running     bool
	mu          sync.Mutex
	stopChan    chan struct{}
	wg          sync.WaitGroup
}

// WEFAXConfig contains configuration parameters for the WEFAX decoder
type WEFAXConfig struct {
	LPM                      int       `json:"lpm"`
	ImageWidth               int       `json:"image_width"`
	BitsPerPixel             int       `json:"bits_per_pixel"`
	Carrier                  float64   `json:"carrier"`
	Deviation                float64   `json:"deviation"`
	Bandwidth                Bandwidth `json:"bandwidth"`
	MinusSaturationThreshold float64   `json:"minus_saturation_threshold"`
	IncludeHeadersInImages   bool      `json:"include_headers_in_images"`
	UsePhasing               bool      `json:"use_phasing"`
	AutoStop                 bool      `json:"auto_stop"`
	AutoStart                bool      `json:"auto_start"`
}

// DefaultWEFAXConfig returns default configuration
func DefaultWEFAXConfig() WEFAXConfig {
	return WEFAXConfig{
		LPM:                      120,
		ImageWidth:               1809,
		BitsPerPixel:             8,
		Carrier:                  1900.0,
		Deviation:                400.0,
		Bandwidth:                BandwidthMiddle,
		MinusSaturationThreshold: 0.0,
		IncludeHeadersInImages:   false,
		UsePhasing:               true,
		AutoStop:                 true,
		AutoStart:                true,
	}
}

// NewWEFAXDecoder creates a new WEFAX decoder
func NewWEFAXDecoder(sampleRate int, config WEFAXConfig) *WEFAXDecoder {
	d := &WEFAXDecoder{
		lpm:                      config.LPM,
		imageWidth:               config.ImageWidth,
		bitsPerPixel:             config.BitsPerPixel,
		carrier:                  config.Carrier,
		deviation:                config.Deviation,
		minusSaturationThreshold: config.MinusSaturationThreshold,
		includeHeadersInImages:   config.IncludeHeadersInImages,
		usePhasing:               config.UsePhasing,
		autoStop:                 config.AutoStop,
		autoStart:                config.AutoStart,
		samplesPerSecNom:         float64(sampleRate),
		samplesPerSecFrac:        float64(sampleRate),
		samplesPerSecFracPrev:    float64(sampleRate),
		imageColors:              1,
		startIOC576Frequency:     300,
		startIOC288Frequency:     675,
		stopFrequency:            450,
		startStopLength:          5,
		phasingLines:             40,
		lastType:                 HeaderImage,
		stopChan:                 make(chan struct{}),
	}

	d.firFilters[0] = NewFIRFilter(config.Bandwidth)
	d.firFilters[1] = NewFIRFilter(config.Bandwidth)

	d.skipHeaderDetection = !d.usePhasing && !d.autoStop && !d.autoStart

	samplesPerMin := d.samplesPerSecNom * 60.0
	d.samplesPerLine = int(samplesPerMin / float64(d.lpm))

	d.samples = make([]int16, d.samplesPerLine)
	d.demodData = make([]uint8, d.samplesPerLine)
	d.phasingPos = make([]int, d.phasingLines)

	d.height = 256
	d.imgData = make([]uint8, d.imageWidth*d.height*d.imageColors)
	d.outImage = make([]uint8, d.imageWidth*d.imageColors)

	d.lineIncrFrac = float64(d.imageWidth) / (math.Pi * 576)
	d.sampleRateRatio = d.samplesPerSecFrac / d.samplesPerSecNom

	log.Printf("[WEFAX] Initialized: LPM=%d, Width=%d, Carrier=%.1f Hz, Deviation=%.1f Hz, SamplesPerLine=%d",
		d.lpm, d.imageWidth, d.carrier, d.deviation, d.samplesPerLine)

	if d.autoStart {
		log.Printf("[WEFAX] Auto-start enabled — waiting for START signal before decoding")
	}

	return d
}

// Start begins processing audio samples
func (d *WEFAXDecoder) Start(audioChan <-chan []int16, resultChan chan<- []byte) error {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return fmt.Errorf("decoder already running")
	}
	d.running = true
	d.mu.Unlock()

	d.wg.Add(1)
	go d.processLoop(audioChan, resultChan)

	return nil
}

// Stop stops the decoder
func (d *WEFAXDecoder) Stop() error {
	d.mu.Lock()
	if !d.running {
		d.mu.Unlock()
		return nil
	}
	d.mu.Unlock()

	close(d.stopChan)
	d.wg.Wait()

	d.mu.Lock()
	d.running = false
	d.mu.Unlock()

	return nil
}

// processLoop is the main processing loop
func (d *WEFAXDecoder) processLoop(audioChan <-chan []int16, resultChan chan<- []byte) {
	defer d.wg.Done()

	for {
		select {
		case <-d.stopChan:
			return
		case samples, ok := <-audioChan:
			if !ok {
				return
			}
			d.processSamples(samples, resultChan)
		}
	}
}

// processSamples processes incoming audio samples
func (d *WEFAXDecoder) processSamples(samps []int16, resultChan chan<- []byte) {
	i := 0

	if d.skip > 0 {
		skip := d.skip
		if skip > len(samps) {
			skip = len(samps)
		}
		samps = samps[skip:]
		d.skip -= skip
	}

	for i < len(samps) {
		for i < len(samps) && d.sampIdx < d.samplesPerLine {
			d.samples[d.sampIdx] = samps[i]
			d.sampIdx++
			d.fi += d.sampleRateRatio
			i = int(math.Trunc(d.fi))
		}

		if d.sampIdx == d.samplesPerLine {
			d.decodeFaxLine(resultChan)
			d.sampIdx = 0
		}
	}

	d.fi -= float64(len(samps))
}

// decodeFaxLine decodes a single fax line
func (d *WEFAXDecoder) decodeFaxLine(resultChan chan<- []byte) {
	const phasingSkipLines = 2

	d.demodulateData()

	var lineType HeaderType
	if d.skipHeaderDetection {
		lineType = HeaderImage
	} else {
		bufferLen := d.samplesPerLine
		if bufferLen > 3000 {
			bufferLen = 3000
		}
		lineType = d.detectLineType(d.demodData, bufferLen)
	}

	if lineType == d.lastType && lineType != HeaderImage {
		d.typeCount++
	} else {
		d.typeCount--
		if d.typeCount < 0 {
			d.typeCount = 0
		}
	}
	d.lastType = lineType

	if lineType != HeaderImage {
		leewayLines := 4
		threshold := d.startStopLength*d.lpm/60 - leewayLines

		if d.typeCount == threshold {
			if lineType == HeaderStart {
				if !d.includeHeadersInImages {
					d.imageLine = 0
					d.imgPos = 0
					d.lineIncrAcc = 0
				}
				d.phasingLinesLeft = d.phasingLines
				d.phasingSkipData = 0
				d.havePhasing = false
				if d.autoStopped {
					d.autoStopped = false
					log.Printf("[WEFAX] Auto-stop cleared at line %d", d.imageLine)
				}
				if d.autoStart && !d.autoStarted {
					d.autoStarted = true
					log.Printf("[WEFAX] START signal detected, beginning decode at line %d", d.imageLine)
				}
				d.sendStartSignal(resultChan)
			} else if lineType == HeaderStop {
				if d.autoStop {
					d.autoStopped = true
					log.Printf("[WEFAX] Auto-stopped at line %d", d.imageLine)
				}
				if d.autoStart && d.autoStarted {
					d.autoStarted = false
					log.Printf("[WEFAX] STOP signal detected, waiting for next START at line %d", d.imageLine)
				}
				d.sendStopSignal(resultChan)
			}
		}
	}

	if d.usePhasing && d.phasingLinesLeft > 0 && d.phasingLinesLeft <= d.phasingLines-phasingSkipLines {
		d.phasingPos[d.phasingLinesLeft-1] = d.faxPhasingLinePosition(d.demodData)
	}

	if d.usePhasing && lineType == HeaderImage && d.phasingLinesLeft >= -phasingSkipLines {
		d.phasingLinesLeft--
		if d.phasingLinesLeft == 0 {
			d.phasingSkipData = median(d.phasingPos[:d.phasingLines-phasingSkipLines])

			tenPct := percentile(d.phasingPos[:d.phasingLines-phasingSkipLines], 10)
			ninetyPct := percentile(d.phasingPos[:d.phasingLines-phasingSkipLines], 90)

			if (ninetyPct - tenPct) > d.samplesPerLine/6 {
				log.Printf("[WEFAX] Bad phasing data detected, ignoring")
				d.phasingSkipData = 0
			} else {
				log.Printf("[WEFAX] Phasing detected: skip=%d pixels", d.phasingSkipData)
			}
		}
	}

	if d.includeHeadersInImages || !d.usePhasing || (lineType == HeaderImage && d.phasingLinesLeft < -phasingSkipLines) {
		if d.imageLine >= d.height {
			d.height *= 2
			newData := make([]uint8, d.imageWidth*d.height*d.imageColors)
			copy(newData, d.imgData)
			d.imgData = newData
		}

		shouldDecode := !d.autoStopped && (!d.autoStart || d.autoStarted)

		if shouldDecode {
			d.decodeImageLine(d.demodData, resultChan)
		}

		d.phasingSkipData %= d.samplesPerLine
		if d.phasingSkipData != 0 && d.usePhasing && !d.havePhasing {
			d.skip = d.phasingSkipData
			d.havePhasing = true
			log.Printf("[WEFAX] Applied phasing offset: %d samples", d.phasingSkipData)
		}

		d.imgPos += d.imageWidth * d.imageColors
		d.imageLine++
	}
}

// demodulateData performs FM demodulation
func (d *WEFAXDecoder) demodulateData() {
	phaseInc := d.carrier / d.samplesPerSecFrac
	phase := 0.0

	scale := -1.3 * (d.samplesPerSecNom / d.deviation / 8)

	for i := 0; i < d.samplesPerLine; i++ {
		samp := float64(d.samples[i]) / 32768.0

		iCur := d.firFilters[0].Apply(samp * math.Cos(2*math.Pi*phase))
		qCur := d.firFilters[1].Apply(samp * math.Sin(2*math.Pi*phase))

		phase += phaseInc
		if phase > 1.0 {
			phase -= 1.0
		}

		mag := math.Sqrt(qCur*qCur + iCur*iCur)
		if mag > 0 {
			iCur /= mag
			qCur /= mag
		}

		x := (iCur*(qCur-d.qPrev) - qCur*(iCur-d.iPrev)) * scale
		x = x/2.0 + 0.5

		pixel := int(x * 255.0)
		if pixel < 0 {
			pixel = 0
		} else if pixel > 255 {
			pixel = 255
		}

		d.demodData[i] = uint8(pixel)

		d.iPrev = iCur
		d.qPrev = qCur
	}
}

// fourierTransformSub performs Fourier transform at a specific frequency
func (d *WEFAXDecoder) fourierTransformSub(buffer []uint8, freq int) float64 {
	k := -2 * math.Pi * float64(freq) * 60.0 / float64(d.lpm) / float64(d.samplesPerLine)
	retr := 0.0
	reti := 0.0

	for n := 0; n < len(buffer); n++ {
		retr += float64(buffer[n]) * math.Cos(k*float64(n))
		reti += float64(buffer[n]) * math.Sin(k*float64(n))
	}

	return math.Sqrt(retr*retr + reti*reti)
}

// detectLineType detects if line is START, STOP, or IMAGE
func (d *WEFAXDecoder) detectLineType(buffer []uint8, bufferLen int) HeaderType {
	const threshold = 5.0

	startDet := d.fourierTransformSub(buffer[:bufferLen], d.startIOC576Frequency) / float64(bufferLen)
	stopDet := d.fourierTransformSub(buffer[:bufferLen], d.stopFrequency) / float64(bufferLen)

	if startDet > threshold {
		return HeaderStart
	}
	if stopDet > threshold {
		return HeaderStop
	}
	return HeaderImage
}

// faxPhasingLinePosition detects the start position from phasing line
func (d *WEFAXDecoder) faxPhasingLinePosition(image []uint8) int {
	n := int(float64(d.samplesPerLine) * 0.07)
	minTotal := -1
	minPos := 0

	pixelResolution := 4
	sampsIncr := (d.samplesPerLine / d.imageWidth) * pixelResolution

	for i := 0; i < d.samplesPerLine; i += sampsIncr {
		total := 0
		for j := 0; j < n; j += pixelResolution {
			wedge := n/2 - absInt(j-n/2)
			idx := (i + j) % d.samplesPerLine
			total += wedge * (255 - int(image[idx]))
		}

		if total < minTotal || minTotal == -1 {
			minTotal = total
			minPos = i
		}
	}

	return (minPos + n/2) % d.samplesPerLine
}

// decodeImageLine decodes a single image line
func (d *WEFAXDecoder) decodeImageLine(buffer []uint8, resultChan chan<- []byte) {
	for i := 0; i < d.imageWidth; i++ {
		firstSample := d.samplesPerLine * i / d.imageWidth
		lastSample := d.samplesPerLine*(i+1)/d.imageWidth - 1

		pixel := 0
		pixelSamples := 0

		for sample := firstSample; sample <= lastSample; sample++ {
			pixel += int(buffer[sample])
			pixelSamples++
		}

		pixel /= pixelSamples
		d.imgData[d.imgPos+i] = uint8(pixel)
	}

	emit := false
	if d.lineIncrAcc >= 1.0 {
		d.lineIncrAcc -= 1.0

		if d.imageLine != 0 && d.lineIncrAcc != 0 {
			lineNextBlend := d.lineIncrAcc / d.lineBlend
			linePrevBlend := 1.0 - lineNextBlend

			prevLineStart := d.imgPos - d.imageWidth
			for i := 0; i < d.imageWidth; i++ {
				pixel := float64(d.imgData[d.imgPos+i])*lineNextBlend +
					float64(d.imgData[prevLineStart+i])*linePrevBlend
				if pixel > 255 {
					pixel = 255
				}
				d.outImage[i] = uint8(pixel)
			}
			d.lineBlend = d.lineIncrFrac
		} else {
			copy(d.outImage, d.imgData[d.imgPos:d.imgPos+d.imageWidth])
		}
		emit = true
	} else {
		d.lineBlend += d.lineIncrFrac
	}
	d.lineIncrAcc += d.lineIncrFrac

	if emit {
		d.sendImageLine(resultChan)
	}
}

// sendImageLine sends a decoded image line to the result channel
// Protocol: [type:1][line_number:4BE][width:4BE][pixel_data:width]
func (d *WEFAXDecoder) sendImageLine(resultChan chan<- []byte) {
	msg := make([]byte, 1+4+4+d.imageWidth)
	msg[0] = MsgImageLine
	binary.BigEndian.PutUint32(msg[1:5], uint32(d.imageLine))
	binary.BigEndian.PutUint32(msg[5:9], uint32(d.imageWidth))
	copy(msg[9:], d.outImage[:d.imageWidth])

	select {
	case resultChan <- msg:
	default:
	}
}

// sendStartSignal sends a START signal
func (d *WEFAXDecoder) sendStartSignal(resultChan chan<- []byte) {
	msg := []byte{MsgStart}
	select {
	case resultChan <- msg:
		log.Printf("[WEFAX] Sent START signal")
	default:
	}
}

// sendStopSignal sends a STOP signal
func (d *WEFAXDecoder) sendStopSignal(resultChan chan<- []byte) {
	msg := []byte{MsgStop}
	select {
	case resultChan <- msg:
		log.Printf("[WEFAX] Sent STOP signal")
	default:
	}
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func median(values []int) int {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]int, len(values))
	copy(sorted, values)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[i] > sorted[j] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	return sorted[len(sorted)/2]
}

func percentile(values []int, pct int) int {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]int, len(values))
	copy(sorted, values)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[i] > sorted[j] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	idx := (len(sorted) * pct) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

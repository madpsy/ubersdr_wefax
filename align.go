// align.go — slant and margin correction for completed fax images.
//
// Two separate faults move a chart sideways by an amount that grows steadily
// with the line number, so one correction undoes both.  Line y is moved LEFT
// by round(shift + slant*y) pixels, wrapping, so no pixel is ever lost:
//
//   - slant: the transmitter's line rate (or our sample clock) is a few ppm off
//     120 lpm.  Northwood (GYA) runs about 34 ppm off and slants on every
//     receiver; DWD about +6 ppm.
//   - shift: where the line start fell.  Phasing normally puts it right; when
//     phasing fails the chart is wrapped at an arbitrary column.
//
// Nothing here is tied to a station.  A long straight vertical line (the
// margin, or a map frame) runs exactly straight down the page once the slant
// is taken out, so summed along the right slant it becomes one sharp peak,
// while map lines, which are curved or diagonal, smear out.  measureSlant
// tries every plausible slant and keeps the one giving the strongest line.
//
// One chart can still fool that: some charts have thick, genuinely sloping
// meridians.  But the slant belongs to the station's clock, not to the chart,
// so it is the same on every chart from one frequency.  slantConsensus keeps
// the recent confident measurements per frequency and applies their median,
// which a stray chart cannot move.  On m9psy's archive (2,255 charts) the
// consensus settled within six charts and stayed within ±0.6 ppm.
//
// The shift is only applied when the decoder's phasing did not succeed: then
// the margin is moved to the left edge.  A correctly phased chart is never
// shifted, because a map frame cannot be told apart from a margin by looking.
//
// The margin scoring follows VibeSDR's src/utils/wefaxAlign.ts (Stuart,
// confirmed on air on Northwood 4610 kHz, 2026-10-04).
package main

import (
	"math"
	"sort"
	"sync"
)

const (
	// alignMaxPPM bounds the slant search.  alignStepPPM moves a 1809 px
	// line by about a pixel over a full chart.
	alignMaxPPM  = 80.0
	alignStepPPM = 0.25

	// alignBlock lines are summed together before the slant search.  The
	// line drifts at most 80 ppm × 1809 px × 32 ≈ 5 px within a block, which
	// the 5-column smoothing absorbs.
	alignBlock = 32

	// alignMinLines is the fewest lines worth measuring.
	alignMinLines = 150

	// A line is accepted when its whole-chart score (mean darkness per line
	// above the paper either side) is at least lineMinScore and lineMinRatio
	// times the 30th-best column's, it is present in at least lineMinCoverage
	// of the blocks, and where present it is at least lineMinDarkness dark
	// and at most lineMaxWidth pixels wide.  Fitted and cross-validated on
	// m9psy's archive: map features that pass the first two tests are fainter,
	// patchier or wider than a margin or frame.
	lineMinScore    = 60.0
	lineMinRatio    = 2.0
	lineMinCoverage = 0.3
	lineMinDarkness = 120.0
	lineMaxWidth    = 8

	// lineBlockScore is the score a single block must reach for the line to
	// count as present in it.
	lineBlockScore = 40.0

	// marginTargetX is where the margin's left edge is put after shifting.
	marginTargetX = 2
)

// alignOffset is the whole-pixel left move for line y, in 0..width-1.
func alignOffset(shift int, slant float64, y, width int) int {
	if width <= 0 {
		return 0
	}
	o := (int(math.Round(slant*float64(y))) + shift) % width
	if o < 0 {
		o += width
	}
	return o
}

// ppmToSlant converts a line-rate error to pixels per line.
func ppmToSlant(ppm float64, width int) float64 { return ppm * 1e-6 * float64(width) }

// lineMeasurement describes the strongest straight line found at one slant.
type lineMeasurement struct {
	ppm      float64 // slant, as a line-rate error
	col      int     // column at line 0 (the line's centre)
	left     int     // the line's left edge at line 0
	score    float64 // whole-chart score of col
	ref      float64 // 30th-best column's score
	coverage float64 // fraction of blocks the line is present in

	// Where the line is present: its darkness above the paper either side,
	// and its width in pixels at half that darkness.
	darkness float64
	width    int
}

// ok applies the acceptance tests.
func (m lineMeasurement) ok() bool {
	return m.score >= lineMinScore && m.score >= lineMinRatio*math.Max(m.ref, 1) &&
		m.coverage >= lineMinCoverage && m.darkness >= lineMinDarkness && m.width <= lineMaxWidth
}

// darkBlocks holds per-column darkness (255 - pixel) summed over blocks of
// alignBlock lines.
type darkBlocks struct {
	width int
	n     int // lines summed in all
	sum   [][]float64
	y     []int // each block's mean line number
	cnt   []int // lines in each block
}

func sumBlocks(rows [][]byte, width int) *darkBlocks {
	d := &darkBlocks{width: width}
	for y0 := 0; y0 < len(rows); y0 += alignBlock {
		s := make([]float64, width)
		cnt, ysum := 0, 0
		for y := y0; y < y0+alignBlock && y < len(rows); y++ {
			if len(rows[y]) != width {
				continue
			}
			for x, p := range rows[y] {
				s[x] += float64(255 - int(p))
			}
			ysum += y
			cnt++
		}
		if cnt == 0 {
			continue
		}
		d.sum = append(d.sum, s)
		d.y = append(d.y, (ysum+cnt/2)/cnt)
		d.cnt = append(d.cnt, cnt)
		d.n += cnt
	}
	return d
}

func (d *darkBlocks) wrap(x int) int { return ((x % d.width) + d.width) % d.width }

// peak finds the strongest line at slant s (pixels per line): its column at
// line 0, its score and the 30th-best score.  A line is darker than BOTH
// sides; the edge of a dark band is darker on one side only, so each column
// scores its own darkness (smoothed over 5 columns) minus the darker of the
// two averages 25–40 px away.
func (d *darkBlocks) peak(s float64, prof, sm, narrow []float64) (best int, score, ref float64) {
	w := d.width
	for x := range prof {
		prof[x] = 0
	}
	for i, b := range d.sum {
		off := alignOffset(0, s, d.y[i], w)
		for x := 0; x < w; x++ {
			prof[x] += b[(x+off)%w]
		}
	}
	for x := 0; x < w; x++ {
		var a float64
		for k := -2; k <= 2; k++ {
			a += prof[d.wrap(x+k)]
		}
		sm[x] = a / 5 / float64(d.n)
	}
	for x := 0; x < w; x++ {
		var l, r float64
		for k := 25; k <= 40; k++ {
			r += sm[d.wrap(x+k)]
			l += sm[d.wrap(x-k)]
		}
		narrow[x] = sm[x] - math.Max(l, r)/16
	}
	for x := 1; x < w; x++ {
		if narrow[x] > narrow[best] {
			best = x
		}
	}
	sorted := append([]float64(nil), narrow...)
	sort.Sort(sort.Reverse(sort.Float64Slice(sorted)))
	return best, narrow[best], sorted[min(29, len(sorted)-1)]
}

// describe fills in coverage, darkness, width and left edge for the line at
// col, slant s.
func (d *darkBlocks) describe(m *lineMeasurement, s float64) {
	const half = 48
	near := make([]float64, 2*half+1)
	nearN, present := 0, 0
	for i, b := range d.sum {
		c := m.col + alignOffset(0, s, d.y[i], d.width)
		px := func(dx int) float64 { return b[d.wrap(c+dx)] }
		at := func(dx int) float64 {
			var a float64
			for k := -2; k <= 2; k++ {
				a += px(dx + k)
			}
			return a / 5
		}
		var l, r float64
		for k := 25; k <= 40; k++ {
			r += at(k)
			l += at(-k)
		}
		if at(0)-math.Max(l, r)/16 < lineBlockScore*float64(d.cnt[i]) {
			continue
		}
		present++
		for k := -half; k <= half; k++ {
			near[k+half] += px(k)
		}
		nearN += d.cnt[i]
	}
	m.coverage = float64(present) / float64(len(d.sum))
	if nearN > 0 {
		for i := range near {
			near[i] /= float64(nearN)
		}
		var lo int
		m.darkness, m.width, lo = markWidth(near, half)
		m.left = d.wrap(m.col + lo - half)
	}
}

// markWidth measures the dark mark near the centre c (±8 px) of a darkness
// profile: its peak above the paper 25–40 px away, and how many contiguous
// pixels are at least half way to that peak, starting at index lo.
func markWidth(p []float64, c int) (dark float64, width, lo int) {
	pk := c
	for i := c - 8; i <= c+8; i++ {
		if p[i] > p[pk] {
			pk = i
		}
	}
	var l, r float64
	for k := 25; k <= 40; k++ {
		l += p[max(pk-k, 0)]
		r += p[min(pk+k, len(p)-1)]
	}
	paper := math.Min(l, r) / 16
	dark = p[pk] - paper
	if dark <= 0 {
		return 0, 0, c
	}
	thr := paper + dark/2
	lo, hi := pk, pk
	for lo > 0 && p[lo-1] >= thr {
		lo--
	}
	for hi < len(p)-1 && p[hi+1] >= thr {
		hi++
	}
	return dark, hi - lo + 1, lo
}

// measure finds the strongest straight line at each of the given slants
// (ppm) and returns the best.  ok is false when there are too few lines.
// rows hold greyscale pixels, 0 = black; rows of the wrong width are ignored.
func measure(rows [][]byte, ppms []float64) (m lineMeasurement, ok bool) {
	if len(rows) == 0 {
		return m, false
	}
	width := len(rows[0])
	if width < 100 {
		return m, false
	}
	d := sumBlocks(rows, width)
	if d.n < alignMinLines {
		return m, false
	}
	prof := make([]float64, width)
	sm := make([]float64, width)
	narrow := make([]float64, width)
	m.score = math.Inf(-1)
	for _, ppm := range ppms {
		col, score, ref := d.peak(ppmToSlant(ppm, width), prof, sm, narrow)
		// Prefer the smaller slant on a tie, so an undrifted chart stays at 0.
		if score > m.score || (score == m.score && math.Abs(ppm) < math.Abs(m.ppm)) {
			m = lineMeasurement{ppm: ppm, col: col, score: score, ref: ref}
		}
	}
	d.describe(&m, ppmToSlant(m.ppm, width))
	return m, true
}

// slantSearch is every slant measureSlant tries, in ppm.
var slantSearch = func() []float64 {
	steps := int(math.Round(alignMaxPPM / alignStepPPM))
	out := make([]float64, 0, 2*steps+1)
	for i := -steps; i <= steps; i++ {
		out = append(out, float64(i)*alignStepPPM)
	}
	return out
}()

// measureSlant measures this chart's slant in ppm from its strongest straight
// line.  ok is false when no line passes the acceptance tests.
func measureSlant(rows [][]byte) (ppm float64, ok bool) {
	m, ok := measure(rows, slantSearch)
	if !ok || !m.ok() {
		return 0, false
	}
	return m.ppm, true
}

// findMargin returns the left edge at line 0 of the strongest straight line
// running at the given slant, or ok=false when there is none.
func findMargin(rows [][]byte, ppm float64) (left int, ok bool) {
	m, ok := measure(rows, []float64{ppm})
	if !ok || !m.ok() {
		return 0, false
	}
	return m.left, true
}

// applyAlign moves line y of rows left by round(shift + slant*y), in place.
func applyAlign(rows [][]byte, shift int, slant float64) {
	if len(rows) == 0 || (shift == 0 && slant == 0) {
		return
	}
	width := len(rows[0])
	tmp := make([]byte, width)
	for y, r := range rows {
		if len(r) != width {
			continue
		}
		off := alignOffset(shift, slant, y, width)
		if off == 0 {
			continue
		}
		for x := 0; x < width; x++ {
			tmp[x] = r[(x+off)%width]
		}
		copy(r, tmp)
	}
}

// ---------------------------------------------------------------------------
// slantConsensus — the per-frequency slant, from recent charts' measurements
// ---------------------------------------------------------------------------

const (
	// consensusKeep recent confident measurements are kept per frequency.
	// The median is used once there are consensusMin of them and at least
	// consensusAgree of them lie within consensusTolPPM of it; otherwise
	// (e.g. two stations sharing a frequency) no slant is applied.
	consensusKeep   = 30
	consensusMin    = 5
	consensusAgree  = 0.6
	consensusTolPPM = 5.0
)

type slantConsensus struct {
	mu     sync.Mutex
	byFreq map[int][]float64 // oldest first
}

func newSlantConsensus() *slantConsensus {
	return &slantConsensus{byFreq: make(map[int][]float64)}
}

// add records one chart's measured slant.
func (c *slantConsensus) add(freqHz int, ppm float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := append(c.byFreq[freqHz], ppm)
	if len(h) > consensusKeep {
		h = h[len(h)-consensusKeep:]
	}
	c.byFreq[freqHz] = h
}

// get returns the frequency's slant in ppm, or ok=false when there is no
// consensus yet.
func (c *slantConsensus) get(freqHz int) (ppm float64, ok bool) {
	c.mu.Lock()
	h := append([]float64(nil), c.byFreq[freqHz]...)
	c.mu.Unlock()
	if len(h) < consensusMin {
		return 0, false
	}
	sort.Float64s(h)
	med := h[len(h)/2]
	if len(h)%2 == 0 {
		med = (h[len(h)/2-1] + h[len(h)/2]) / 2
	}
	agree := 0
	for _, v := range h {
		if math.Abs(v-med) <= consensusTolPPM {
			agree++
		}
	}
	if float64(agree) < consensusAgree*float64(len(h)) {
		return 0, false
	}
	return med, true
}

// alignResult is what alignImage measured and applied.
type alignResult struct {
	measuredPPM *float64 // this chart's own slant, when it could be measured
	ppm         float64  // slant applied (the frequency's consensus)
	slant       float64  // the same in pixels per line
	shift       int      // left move applied at line 0
}

// alignImage corrects rows in place: the frequency's consensus slant always,
// and a shift that puts the margin at the left edge when phasing did not
// succeed.  The chart's own measurement is added to the consensus first.
func alignImage(rows [][]byte, freqHz int, phasingOK bool, cons *slantConsensus) (r alignResult) {
	if len(rows) == 0 {
		return r
	}
	width := len(rows[0])
	if ppm, ok := measureSlant(rows); ok {
		r.measuredPPM = &ppm
		cons.add(freqHz, ppm)
	}
	if ppm, ok := cons.get(freqHz); ok {
		r.ppm = ppm
		r.slant = ppmToSlant(ppm, width)
	}
	if !phasingOK {
		if col, ok := findMargin(rows, r.ppm); ok {
			r.shift = col - marginTargetX
		}
	}
	applyAlign(rows, r.shift, r.slant)
	return r
}

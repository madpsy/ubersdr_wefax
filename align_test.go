package main

import (
	"math"
	"math/rand"
	"testing"
)

// synthChart draws a white chart with speckle noise, some curved and
// diagonal "map" lines, and optionally a thick black margin that starts at
// marginCol on line 0 and drifts with the given slant (ppm), the way a
// transmitter off 120 lpm draws it.
func synthChart(width, height int, ppm float64, marginCol int, withMargin bool, seed int64) [][]byte {
	rng := rand.New(rand.NewSource(seed))
	rows := make([][]byte, height)
	slant := ppmToSlant(ppm, width)
	for y := range rows {
		r := make([]byte, width)
		for x := range r {
			r[x] = 245
			if rng.Intn(40) == 0 {
				r[x] = byte(rng.Intn(120))
			}
		}
		// Map features: two diagonals and a sine "coastline".
		for _, x := range []int{(300 + y/3) % width, (1400 - y/2 + width) % width,
			(900 + int(120*math.Sin(float64(y)/60)) + width) % width} {
			r[x], r[(x+1)%width] = 20, 20
		}
		if withMargin {
			c := marginCol + int(math.Round(slant*float64(y)))
			for k := 0; k < 6; k++ {
				r[((c+k)%width+width)%width] = 10
			}
		}
		rows[y] = r
	}
	return rows
}

func TestMeasureSlantFindsTheDrift(t *testing.T) {
	for _, ppm := range []float64{-34, -10, 0, 6.25, 40} {
		rows := synthChart(1809, 1200, ppm, 700, true, 1)
		got, ok := measureSlant(rows)
		if !ok {
			t.Fatalf("%v ppm: no line found", ppm)
		}
		// One search step is 0.25 ppm; allow two.
		if math.Abs(got-ppm) > 0.5 {
			t.Errorf("measured %.2f ppm, want %.2f", got, ppm)
		}
	}
}

func TestNoLineMeansNoMeasurement(t *testing.T) {
	rows := synthChart(1809, 1200, -34, 0, false, 2)
	if ppm, ok := measureSlant(rows); ok {
		t.Errorf("chart without a margin measured %.2f ppm; want no measurement", ppm)
	}
	if col, ok := findMargin(rows, -34); ok {
		t.Errorf("chart without a margin found one at column %d", col)
	}
}

func TestTooFewLinesIsNotMeasured(t *testing.T) {
	rows := synthChart(1809, alignMinLines-1, -34, 700, true, 3)
	if _, ok := measureSlant(rows); ok {
		t.Error("measured a chart shorter than alignMinLines")
	}
}

// consensusOf returns a consensus already holding n measurements of ppm.
func consensusOf(freq int, ppm float64, n int) *slantConsensus {
	c := newSlantConsensus()
	for i := 0; i < n; i++ {
		c.add(freq, ppm)
	}
	return c
}

func TestAlignImageStraightensAndShiftsWithoutPhasing(t *testing.T) {
	const ppm, col = -34.0, 1100
	rows := synthChart(1809, 1200, ppm, col, true, 4)
	cons := consensusOf(4610000, ppm, consensusMin)

	r := alignImage(rows, 4610000, false, cons)
	if r.measuredPPM == nil || math.Abs(*r.measuredPPM-ppm) > 0.5 {
		t.Fatalf("measured %v, want about %v", r.measuredPPM, ppm)
	}
	if math.Abs(r.ppm-ppm) > 0.5 {
		t.Errorf("applied %.2f ppm, want about %v", r.ppm, ppm)
	}
	// After correction the margin must sit at marginTargetX on every line.
	for _, y := range []int{0, 300, 600, 900, 1199} {
		for k := 0; k < 6; k++ {
			if x := marginTargetX + k; rows[y][x] != 10 {
				t.Fatalf("line %d: pixel %d is %d, want margin (10)", y, x, rows[y][x])
			}
		}
	}
}

func TestAlignImageDoesNotShiftAPhasedChart(t *testing.T) {
	const ppm, col = 6.0, 52 // DWD-like: a frame 52 px in, correctly phased
	rows := synthChart(1809, 1200, ppm, col, true, 5)
	cons := consensusOf(7880000, ppm, consensusMin)

	r := alignImage(rows, 7880000, true, cons)
	if r.shift != 0 {
		t.Errorf("phased chart shifted by %d px", r.shift)
	}
	// Straightened in place: the frame stays at its line-0 column.
	for _, y := range []int{0, 600, 1199} {
		if rows[y][col+2] != 10 {
			t.Errorf("line %d: frame not at column %d after de-slanting", y, col)
		}
	}
}

func TestAlignImageWithoutConsensusOnlyShifts(t *testing.T) {
	rows := synthChart(1809, 1200, 0, 400, true, 6)
	r := alignImage(rows, 3855000, false, newSlantConsensus())
	if r.slant != 0 {
		t.Errorf("applied slant %v with no consensus", r.slant)
	}
	if r.shift != 400-marginTargetX {
		t.Errorf("shift = %d, want %d", r.shift, 400-marginTargetX)
	}
}

func TestChartWithoutMarginIsUntouched(t *testing.T) {
	rows := synthChart(1809, 1200, 0, 0, false, 7)
	orig := make([][]byte, len(rows))
	for i, r := range rows {
		orig[i] = append([]byte(nil), r...)
	}
	r := alignImage(rows, 3855000, false, newSlantConsensus())
	if r.shift != 0 || r.slant != 0 || r.measuredPPM != nil {
		t.Fatalf("got %+v for a chart with no line", r)
	}
	for y := range rows {
		if string(rows[y]) != string(orig[y]) {
			t.Fatalf("line %d changed", y)
		}
	}
}

func TestConsensusNeedsEnoughMeasurements(t *testing.T) {
	c := consensusOf(1, -34, consensusMin-1)
	if _, ok := c.get(1); ok {
		t.Errorf("consensus from %d measurements; want at least %d", consensusMin-1, consensusMin)
	}
	c.add(1, -34)
	if ppm, ok := c.get(1); !ok || ppm != -34 {
		t.Errorf("get = %v, %v; want -34, true", ppm, ok)
	}
	if _, ok := c.get(2); ok {
		t.Error("consensus for a frequency never measured")
	}
}

// A chart whose strongest line is a sloping meridian measures a wild slant;
// it must not move the frequency's consensus.
func TestConsensusIgnoresAStrayChart(t *testing.T) {
	c := newSlantConsensus()
	for i := 0; i < 10; i++ {
		c.add(1, -34+0.25*float64(i%3))
	}
	c.add(1, -75)
	c.add(1, 20)
	ppm, ok := c.get(1)
	if !ok || math.Abs(ppm+34) > 0.5 {
		t.Errorf("get = %.2f, %v; want about -34", ppm, ok)
	}
}

// Two stations sharing a frequency with different clocks: no agreement, so
// no slant rather than a wrong one.
func TestConsensusRefusesWhenMeasurementsDisagree(t *testing.T) {
	c := newSlantConsensus()
	for i := 0; i < 10; i++ {
		c.add(1, -34)
		c.add(1, 6)
	}
	if ppm, ok := c.get(1); ok {
		t.Errorf("consensus %.2f from two disagreeing clusters", ppm)
	}
}

func TestConsensusKeepsOnlyRecent(t *testing.T) {
	c := consensusOf(1, -34, consensusKeep)
	for i := 0; i < consensusKeep; i++ {
		c.add(1, 6)
	}
	if ppm, _ := c.get(1); ppm != 6 {
		t.Errorf("get = %v after a full window of 6 ppm; want 6", ppm)
	}
}

func TestAlignOffsetWraps(t *testing.T) {
	cases := []struct {
		shift int
		slant float64
		y     int
		want  int
	}{
		{0, 0, 100, 0},
		{10, 0, 5, 10},
		{0, -0.06, 1000, 1809 - 60},
		{1809 + 3, 0, 0, 3},
		{-5, 0, 0, 1804},
	}
	for _, c := range cases {
		if got := alignOffset(c.shift, c.slant, c.y, 1809); got != c.want {
			t.Errorf("alignOffset(%d, %v, %d) = %d, want %d", c.shift, c.slant, c.y, got, c.want)
		}
	}
}

// The move is a pure rotation of each line, so it can be undone exactly.
func TestApplyAlignIsReversible(t *testing.T) {
	rows := synthChart(500, 300, -34, 100, true, 8)
	orig := make([][]byte, len(rows))
	for i, r := range rows {
		orig[i] = append([]byte(nil), r...)
	}
	const shift, slant = 37, -0.2
	applyAlign(rows, shift, slant)
	for y, r := range rows {
		off := alignOffset(shift, slant, y, 500)
		back := make([]byte, 500)
		for x := range r {
			back[(x+off)%500] = r[x]
		}
		if string(back) != string(orig[y]) {
			t.Fatalf("line %d not recoverable", y)
		}
	}
}

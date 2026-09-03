package pcmv4

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"testing"
)

// Conformance test for the version 4 receive path.
//
// testdata/pcmv4_stream.bin is a packet stream the SERVER's encoder produced,
// and pcmv4ExpectedSHA is the SHA-256 of the samples that went into it, little
// endian, exactly as DecodePacketLE renders them.
//
// It earns its 90 kB. The version 4 predictor is backward adaptive: the two
// ends derive their filter taps independently from the samples already coded
// and never exchange a coefficient, so any arithmetic difference between this
// decoder and the Go one on the server produces plausible noise rather than an
// error. Nothing short of comparing the samples would catch it: the fax
// demodulator would simply stop finding a phasing signal, with nothing anywhere
// saying why.
//
// The stream covers what the format can do: ordinary mono audio, silent packets
// carrying no body, an escape to verbatim samples on incompressible noise, a
// sample-rate change, and interleaved two-channel data -- including the varying
// packet length that makes the header's sample count necessary, across the
// five-second periodic resynchronisation.
//
// The same fixture and the same expected hash are used by the Go, C++, Python
// and JavaScript ports of this decoder; a change here that is not made there is
// a divergence nothing else would report.
const pcmv4ExpectedSHA = "ba368c898ae406c5acc806653d9f2dbbfa40086eca3707fda5d77c13948f78d1"

// testdata/pcmv4_rice_edge.bin covers what a recording of ordinary traffic will
// not: a Rice codeword whose unary run is exactly 63 bits long and is counted
// out of a full 64-bit accumulator, so the decoder shifts by 64. Go defines
// that as zero and C++ does not, and the difference is silent. It appeared
// roughly once every quarter of a million packets on live IQ.
const pcmv4RiceEdgeSHA = "83e3d94b509efbf7a212a3e10193b3eb281fe1460cbfeef6aabe474c92a718c7"

// readV4Fixture returns the packets in a server-produced fixture file.
//
// Layout: "UV4F", a format byte, a uint32 packet count, then each packet as a
// uint32 length and that many bytes.
func readV4Fixture(t *testing.T, path string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if len(raw) < 9 || string(raw[:4]) != "UV4F" || raw[4] != 0 {
		t.Fatal("fixture: bad header")
	}
	count := int(binary.LittleEndian.Uint32(raw[5:]))
	off := 9

	packets := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		if off+4 > len(raw) {
			t.Fatalf("fixture: truncated length at packet %d", i)
		}
		n := int(binary.LittleEndian.Uint32(raw[off:]))
		off += 4
		if off+n > len(raw) {
			t.Fatalf("fixture: truncated packet %d", i)
		}
		packets = append(packets, raw[off:off+n])
		off += n
	}
	if off != len(raw) {
		t.Fatalf("fixture: %d trailing bytes", len(raw)-off)
	}
	return packets
}

func TestPCMv4DecodesServerStream(t *testing.T) {
	packets := readV4Fixture(t, "testdata/pcmv4_stream.bin")
	dec := NewPCMv4StreamDecoder()
	h := sha256.New()

	// Every distinct (rate, channels) the fixture passes through, in order. A
	// decoder that lost the carried-forward metadata could still hash correctly
	// while mislabelling the stream, and the sample rate is what the caller
	// hands to its demodulator.
	wantParams := [][2]int{{12000, 1}, {24000, 1}, {48000, 2}}
	var gotParams [][2]int

	for i, pkt := range packets {
		if !PCMv4IsHeader(pkt) {
			t.Fatalf("packet %d not recognised as version 4", i)
		}
		pcmLE, rate, channels, _, _, err := dec.DecodePacketLE(pkt)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if len(pcmLE) == 0 || len(pcmLE)%(2*channels) != 0 {
			t.Fatalf("packet %d: %d bytes is not whole frames of %d channels", i, len(pcmLE), channels)
		}
		p := [2]int{rate, channels}
		if len(gotParams) == 0 || gotParams[len(gotParams)-1] != p {
			gotParams = append(gotParams, p)
		}
		h.Write(pcmLE)
	}

	if got := hex.EncodeToString(h.Sum(nil)); got != pcmv4ExpectedSHA {
		t.Fatalf("decoded samples differ from what the server encoded\n got %s\nwant %s", got, pcmv4ExpectedSHA)
	}
	if len(gotParams) != len(wantParams) {
		t.Fatalf("stream parameters: got %v, want %v", gotParams, wantParams)
	}
	for i := range wantParams {
		if gotParams[i] != wantParams[i] {
			t.Fatalf("stream parameters: got %v, want %v", gotParams, wantParams)
		}
	}
}

// The 63-bit unary run. Its own fixture because a recording of ordinary traffic
// holds one only by luck.
func TestPCMv4DecodesRiceEdgeStream(t *testing.T) {
	packets := readV4Fixture(t, "testdata/pcmv4_rice_edge.bin")
	dec := NewPCMv4StreamDecoder()
	h := sha256.New()

	for i, pkt := range packets {
		pcmLE, _, _, _, _, err := dec.DecodePacketLE(pkt)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		h.Write(pcmLE)
	}

	if got := hex.EncodeToString(h.Sum(nil)); got != pcmv4RiceEdgeSHA {
		t.Fatalf("rice-edge stream decoded wrong\n got %s\nwant %s", got, pcmv4RiceEdgeSHA)
	}
}

// A server too old for version 4 answers with the zstd-wrapped version 1 shape.
// Recognising it is what lets the addon say why rather than logging a bad magic
// for every packet.
func TestLegacyServerFramesAreRecognised(t *testing.T) {
	zstd := []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00}
	if !IsZstdFrame(zstd) || PCMv4IsHeader(zstd) {
		t.Error("a zstd frame was misclassified")
	}
	for _, pkt := range readV4Fixture(t, "testdata/pcmv4_stream.bin") {
		if IsZstdFrame(pkt) {
			t.Fatal("a version 4 packet read as zstd")
		}
	}
	for _, short := range [][]byte{nil, {}, {0x50}, {0x50, 0x43, 0x4D}} {
		if PCMv4IsHeader(short) || IsZstdFrame(short) {
			t.Errorf("short frame %v misclassified", short)
		}
	}
}

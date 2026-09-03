package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
)

// Conformance test for the addon's own version 4 receive path.
//
// internal/pcmv4 has its own copy of this against the decoder package; this one
// runs the same server-produced fixture through pcmDecoder, the wrapper the
// websocket loop actually calls, so a mistake in the integration -- a lost
// sample rate, a reintroduced byte swap, a decoder shared across connections --
// fails here rather than in the field.
//
// The version 4 predictor is backward adaptive: the two ends derive their
// filter taps from the samples already coded and never exchange a coefficient,
// so any divergence produces plausible noise rather than an error. The fax
// demodulator would simply stop finding a phasing signal, with nothing anywhere
// saying why. Hence a hash of the samples.
const pcmv4FixtureSHA = "ba368c898ae406c5acc806653d9f2dbbfa40086eca3707fda5d77c13948f78d1"

// readPCMv4Fixture returns the packets in testdata/pcmv4_stream.bin.
//
// Layout: "UV4F", a format byte, a uint32 packet count, then each packet as a
// uint32 length and that many bytes.
func readPCMv4Fixture(t *testing.T) [][]byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/pcmv4_stream.bin")
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

func TestPCMDecoderMatchesServerStream(t *testing.T) {
	packets := readPCMv4Fixture(t)
	dec, err := newPCMDecoder()
	if err != nil {
		t.Fatalf("newPCMDecoder: %v", err)
	}
	defer dec.close()

	h := sha256.New()

	// The sample rate and channel count the fax demodulator is configured from
	// now arrive in the version 4 header rather than a fixed 37-byte one, and
	// are carried forward across packets that omit them. A decoder that lost
	// the carried-forward metadata would still hash correctly while handing the
	// demodulator a zero rate.
	wantParams := [][2]int{{12000, 1}, {24000, 1}, {48000, 2}}
	var gotParams [][2]int

	for i, raw := range packets {
		pkt, err := dec.decode(raw)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if pkt.sampleRate <= 0 || pkt.channels <= 0 {
			t.Fatalf("packet %d: rate=%d channels=%d", i, pkt.sampleRate, pkt.channels)
		}
		if len(pkt.pcm) == 0 || len(pkt.pcm)%(2*pkt.channels) != 0 {
			t.Fatalf("packet %d: %d bytes is not whole frames of %d channels",
				i, len(pkt.pcm), pkt.channels)
		}
		p := [2]int{pkt.sampleRate, pkt.channels}
		if len(gotParams) == 0 || gotParams[len(gotParams)-1] != p {
			gotParams = append(gotParams, p)
		}
		h.Write(pkt.pcm)
	}

	if got := hex.EncodeToString(h.Sum(nil)); got != pcmv4FixtureSHA {
		t.Fatalf("decoded samples differ from what the server encoded\n got %s\nwant %s",
			got, pcmv4FixtureSHA)
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

// A decoder is owned by one connection. runOnce builds a fresh one on every
// connect, and this is why: the predictor's state is derived from the samples
// already decoded, so a decoder carried across a reconnect decodes the new
// stream against the old one's filter taps and yields plausible noise, with no
// error anywhere.
//
// The prefix is the first 50 packets, which stay on one codec profile. The
// stream as a whole switches profile partway through, and a profile change
// builds a new codec -- so replaying the whole fixture would reset the
// predictor incidentally and prove nothing.
func TestPCMDecoderIsResetPerConnection(t *testing.T) {
	const prefix = 50
	packets := readPCMv4Fixture(t)
	if len(packets) < prefix {
		t.Fatalf("fixture has only %d packets", len(packets))
	}

	hashPrefix := func(dec *pcmDecoder) string {
		h := sha256.New()
		for i, raw := range packets[:prefix] {
			pkt, err := dec.decode(raw)
			if err != nil {
				t.Fatalf("packet %d: %v", i, err)
			}
			h.Write(pkt.pcm)
		}
		return hex.EncodeToString(h.Sum(nil))
	}

	first, _ := newPCMDecoder()
	want := hashPrefix(first)

	// What runOnce does on reconnect: a fresh decoder, which reproduces the
	// stream exactly.
	fresh, _ := newPCMDecoder()
	if got := hashPrefix(fresh); got != want {
		t.Fatalf("a fresh decoder decoded the same packets differently\n got %s\nwant %s", got, want)
	}

	// What it must not do: carry the old connection's decoder over. If this
	// ever stops differing, the test above has stopped proving anything.
	if got := hashPrefix(first); got == want {
		t.Fatal("a carried-over decoder reproduced the stream; the reset is no longer load-bearing")
	}
}

// A pre-0.1.63 server answers a version it cannot serve with a zstd frame
// rather than refusing. Saying so once beats logging a bad magic forever.
func TestPCMDecoderReportsLegacyServer(t *testing.T) {
	dec, _ := newPCMDecoder()
	if _, err := dec.decode([]byte{0x28, 0xB5, 0x2F, 0xFD, 0x00}); err == nil {
		t.Fatal("a zstd frame from a legacy server decoded without complaint")
	}
}

// The websocket URL asks for version 4 and keeps the pcm-zstd format name,
// which selects the PCM stream rather than the framing. It also still subtracts
// the WEFAX carrier offset from the published frequency, which is what puts the
// 1900 Hz carrier inside the USB passband.
func TestWSURLRequestsVersion4(t *testing.T) {
	inst := newInstance(7880000, 1900, "usb", "ws://example.invalid/ws", "")
	u, err := url.Parse(inst.wsURL())
	if err != nil {
		t.Fatalf("wsURL: %v", err)
	}
	q := u.Query()

	if got := q.Get("version"); got != "4" {
		t.Errorf("version = %q, want \"4\"", got)
	}
	if got := q.Get("format"); got != "pcm-zstd" {
		t.Errorf("format = %q, want \"pcm-zstd\"", got)
	}
	if got := q.Get("frequency"); got != "7878100" {
		t.Errorf("frequency = %q, want \"7878100\" (7880000 - 1900 carrier offset)", got)
	}
	if got := q.Get("mode"); got != "usb" {
		t.Errorf("mode = %q, want \"usb\"", got)
	}
}

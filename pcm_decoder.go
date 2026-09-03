package main

import (
	"encoding/binary"
	"fmt"

	"github.com/nathm8/ubersdr_wefax/internal/pcmv4"
)

// ---------------------------------------------------------------------------
// PCM binary packet decoder
// ---------------------------------------------------------------------------
// The UberSDR server sends one binary WebSocket message per audio packet. This
// addon speaks audio protocol version 4 only.
//
// Versions 1-3 sent a fixed 29- or 37-byte header followed by big-endian int16
// samples, the whole thing wrapped in zstd. Version 4 replaces both halves: a
// variable-length header that carries only what changed since the last packet,
// and a predictive lossless codec in place of the zstd wrapper -- which was
// making this data larger, an LZ77 byte matcher having nothing to match in a
// band-limited RF signal.
//
// The wire format and the codec live in internal/pcmv4, copied verbatim from
// the server's own encoder so the two ends agree bit for bit. See
// internal/pcmv4/pcm_v4_header.go for the layout.

// pcmPacket is the result of decoding one binary WebSocket message.
type pcmPacket struct {
	pcm          []byte // little-endian int16 PCM samples
	sampleRate   int
	channels     int
	hasSigInfo   bool    // true when the header carried a signal-quality measurement
	basebandDBFS float32 // baseband power dBFS
	noiseDBFS    float32 // noise density dBFS
}

// signalUnavailable is the threshold for the -999 sentinel the server writes
// when radiod reported no measurement for this channel.
const signalUnavailable = -998

// pcmDecoder decodes the packets of ONE websocket connection.
//
// One per connection is a requirement rather than a convenience: the version 4
// predictor is backward adaptive, deriving its filter taps from the samples
// already decoded rather than from anything on the wire. A decoder carried
// across a reconnect would decode the new stream against the old one's state
// and return plausible noise rather than an error, and for the same reason
// every packet that arrives has to be fed to it even when its samples are
// afterwards dropped.
type pcmDecoder struct {
	v4 *pcmv4.PCMv4StreamDecoder
}

func newPCMDecoder() (*pcmDecoder, error) {
	return &pcmDecoder{v4: pcmv4.NewPCMv4StreamDecoder()}, nil
}

// decode parses one binary packet into little-endian int16 PCM plus the stream
// parameters the header reports.
func (d *pcmDecoder) decode(data []byte) (pcmPacket, error) {
	// A server older than 0.1.63 clamps a version it cannot serve down to 1 and
	// answers with a zstd frame rather than refusing, so say why instead of
	// logging a bad magic on every packet for the life of the process.
	if pcmv4.IsZstdFrame(data) {
		return pcmPacket{}, fmt.Errorf(
			"server does not support audio protocol version %d (needs UberSDR 0.1.63 or later)",
			pcmv4.ProtocolVersion)
	}

	pcmLE, rate, channels, power, noise, err := d.v4.DecodePacketLE(data)
	if err != nil {
		return pcmPacket{}, err
	}

	pkt := pcmPacket{
		pcm:        pcmLE,
		sampleRate: rate,
		channels:   channels,
	}
	if power > signalUnavailable && noise > signalUnavailable {
		pkt.hasSigInfo = true
		pkt.basebandDBFS = power
		pkt.noiseDBFS = noise
	}
	return pkt, nil
}

// close releases the decoder. Version 4 holds no external resource, but the
// call site owns the decoder for the life of a connection either way.
func (d *pcmDecoder) close() {}

// downmixStereoToMono converts 2-channel S16LE PCM to mono S16LE.
// Used for wfm mode which delivers stereo 48 kHz audio.
func downmixStereoToMono(stereo []byte) []byte {
	n := len(stereo) / 4 // 2 bytes per sample × 2 channels
	mono := make([]byte, n*2)
	for i := 0; i < n; i++ {
		l := int32(int16(binary.LittleEndian.Uint16(stereo[i*4:])))
		r := int32(int16(binary.LittleEndian.Uint16(stereo[i*4+2:])))
		m := int16((l + r) / 2)
		binary.LittleEndian.PutUint16(mono[i*2:], uint16(m))
	}
	return mono
}

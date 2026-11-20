package opus

import (
	"bytes"
	"errors"
	"testing"
)

// This file pins the runtime bitrate-control entry points (Encoder.SetBitrate /
// Bitrate and the multistream counterpart). It is a default, no-cgo test: it proves
// the properties the differential gate cannot see, namely that a mid-stream rate
// change needs no encoder rebuild, leaves cross-frame state intact, and produces a
// single continuously decodable packet sequence.

// TestSetBitrateRoundTripAndDomain covers the read-back contract and the argument
// domain, including the int32-narrowing guard shared with NewEncoder.
func TestSetBitrateRoundTripAndDomain(t *testing.T) {
	e, err := NewEncoder(EncoderConfig{SampleRate: 48000, Channels: 1, Bitrate: 64000})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if got := e.Bitrate(); got != 64000 {
		t.Fatalf("Bitrate() = %d, want 64000", got)
	}

	// A concrete target round-trips.
	if err := e.SetBitrate(32000); err != nil {
		t.Fatalf("SetBitrate(32000): %v", err)
	}
	if got := e.Bitrate(); got != 32000 {
		t.Fatalf("Bitrate() after set = %d, want 32000", got)
	}

	// Zero means automatic; Bitrate() reports it as 0 rather than the OPUS_AUTO
	// sentinel.
	if err := e.SetBitrate(0); err != nil {
		t.Fatalf("SetBitrate(0): %v", err)
	}
	if got := e.Bitrate(); got != 0 {
		t.Fatalf("Bitrate() after automatic = %d, want 0", got)
	}

	// A negative value is rejected and leaves the target unchanged.
	if err := e.SetBitrate(-1); !errors.Is(err, ErrBadArg) {
		t.Fatalf("SetBitrate(-1): got %v, want ErrBadArg", err)
	}
	if got := e.Bitrate(); got != 0 {
		t.Fatalf("a rejected SetBitrate changed the target to %d", got)
	}

	// A value above the codec ceiling is clamped, not wrapped into OPUS_AUTO.
	const maxMono = 750000
	if err := e.SetBitrate(maxMono + 5000); err != nil {
		t.Fatalf("SetBitrate(above max): %v", err)
	}
	if got := e.Bitrate(); got != maxMono {
		t.Fatalf("Bitrate() after over-max = %d, want clamp to %d", got, maxMono)
	}
}

// TestSetBitrateContinuouslyDecodable is the central continuity guarantee: a long
// run of frames whose target changes repeatedly must all decode through ONE
// decoder, in order, with no error and the expected per-frame duration. Nothing
// about a rate change is allowed to break the packet stream or its framing.
func TestSetBitrateContinuouslyDecodable(t *testing.T) {
	const frameSize, channels = 960, 2
	e, err := NewEncoder(EncoderConfig{SampleRate: 48000, Channels: channels, Bitrate: 48000})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	dec, err := NewDecoder(48000, channels)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	pktBuf := make([]byte, 4000)
	out := make([]int16, frameSize*channels)

	rates := []int{48000, 24000, 12000, 96000, 64000, 16000, 48000, 0, 32000}
	const framesPerRate = 4
	for r, rate := range rates {
		if err := e.SetBitrate(rate); err != nil {
			t.Fatalf("rate step %d SetBitrate(%d): %v", r, rate, err)
		}
		for f := range framesPerRate {
			frameIndex := r*framesPerRate + f
			n, err := e.Encode(encTestPCM(frameSize, channels, 48000, frameIndex), pktBuf)
			if err != nil {
				t.Fatalf("Encode frame %d at rate %d: %v", frameIndex, rate, err)
			}
			pkt := pktBuf[:n]
			// Every packet stays a self-describing valid Opus packet of 20 ms.
			if dur, err := PacketDuration(pkt); err != nil || dur != frameSize {
				t.Fatalf("frame %d: PacketDuration=%d,%v want %d", frameIndex, dur, err, frameSize)
			}
			if nf, err := PacketFrames(pkt); err != nil || nf != 1 {
				t.Fatalf("frame %d: PacketFrames=%d,%v want 1", frameIndex, nf, err)
			}
			// The SAME decoder consumes the whole sequence: a discontinuity would
			// surface here as an error or a wrong sample count.
			got, err := dec.Decode(pkt, out)
			if err != nil {
				t.Fatalf("frame %d failed to decode after a rate change: %v", frameIndex, err)
			}
			if got != frameSize {
				t.Fatalf("frame %d decoded %d samples/channel, want %d", frameIndex, got, frameSize)
			}
		}
	}
}

// TestSetBitrateDoesNotResetState proves the "no rebuild / no state reset"
// requirement on packets rather than by inspecting private fields. Two encoders
// receive identical PCM and the same rate change; one only calls SetBitrate, the
// other calls Reset in addition. If SetBitrate reset the cross-frame state the two
// next packets would be identical; they must differ, because the reset encoder lost
// the overlap-add/ring/energy history that the other retained.
func TestSetBitrateDoesNotResetState(t *testing.T) {
	const frameSize, channels = 960, 2
	cfg := EncoderConfig{SampleRate: 48000, Channels: channels, Bitrate: 48000}

	primeAndChange := func(withReset bool) []byte {
		e, err := NewEncoder(cfg)
		if err != nil {
			t.Fatalf("NewEncoder: %v", err)
		}
		buf := make([]byte, 4000)
		// Prime several frames so cross-frame state (overlap-add, energy prediction,
		// the delay ring) is well established.
		for i := range 6 {
			if _, err := e.Encode(encTestPCM(frameSize, channels, 48000, i), buf); err != nil {
				t.Fatalf("prime Encode %d: %v", i, err)
			}
		}
		if withReset {
			e.Reset()
		}
		if err := e.SetBitrate(20000); err != nil {
			t.Fatalf("SetBitrate: %v", err)
		}
		n, err := e.Encode(encTestPCM(frameSize, channels, 48000, 6), buf)
		if err != nil {
			t.Fatalf("changed-rate Encode: %v", err)
		}
		return bytes.Clone(buf[:n])
	}

	kept := primeAndChange(false)
	reset := primeAndChange(true)
	if bytes.Equal(kept, reset) {
		t.Fatal("SetBitrate produced the same packet as SetBitrate+Reset, so it reset the " +
			"cross-frame state; a mid-stream change must preserve it")
	}
}

// TestSetBitrateTakesEffectUnderCBR gives a deterministic, byte-size-visible proof
// that the new target really reaches the rate controller. Under hard CBR a 20 ms
// packet is exactly bitrate/50/8 bytes; changing the target between frames changes
// the next packet's size accordingly, with no encoder reconstruction.
func TestSetBitrateTakesEffectUnderCBR(t *testing.T) {
	const frameSize, channels = 960, 1
	e, err := NewEncoder(EncoderConfig{
		SampleRate: 48000, Channels: channels, Bitrate: 40000, CBR: true,
	})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	buf := make([]byte, 4000)
	// Prime so the measurement is on a steady frame.
	for i := range 3 {
		if _, err := e.Encode(encTestPCM(frameSize, channels, 48000, i), buf); err != nil {
			t.Fatalf("prime: %v", err)
		}
	}
	for _, br := range []int{24000, 64000, 16000} {
		if err := e.SetBitrate(br); err != nil {
			t.Fatalf("SetBitrate(%d): %v", br, err)
		}
		n, err := e.Encode(encTestPCM(frameSize, channels, 48000, 7), buf)
		if err != nil {
			t.Fatalf("Encode at %d: %v", br, err)
		}
		if want := br / 50 / 8; n != want {
			t.Errorf("CBR at %d bps: packet %d bytes, want %d", br, n, want)
		}
	}
}

// TestMultistreamSetBitrate is a smoke test that the surround encoder exposes the
// same runtime change and keeps producing valid packets.
func TestMultistreamSetBitrate(t *testing.T) {
	const frameSize, channels, streams, coupled = 960, 2, 1, 1
	e, err := NewMultistreamEncoder(MultistreamEncoderConfig{
		SampleRate: 48000, Channels: channels, Streams: streams, CoupledStreams: coupled,
		Mapping: []byte{0, 1}, Bitrate: 64000,
	})
	if err != nil {
		t.Fatalf("NewMultistreamEncoder: %v", err)
	}
	if err := e.SetBitrate(-1); !errors.Is(err, ErrBadArg) {
		t.Fatalf("negative multistream SetBitrate: got %v, want ErrBadArg", err)
	}
	buf := make([]byte, 4000)
	for _, br := range []int{32000, 96000} {
		if err := e.SetBitrate(br); err != nil {
			t.Fatalf("SetBitrate(%d): %v", br, err)
		}
		n, err := e.Encode(encTestPCM(frameSize, channels, 48000, 1), buf)
		if err != nil {
			t.Fatalf("multistream Encode at %d: %v", br, err)
		}
		if nf, err := PacketFrames(buf[:n]); err != nil || nf < 1 {
			t.Fatalf("multistream packet at %d: frames=%d err=%v", br, nf, err)
		}
	}
}

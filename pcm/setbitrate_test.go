package pcm_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/tphakala/go-opus/opus"
	"github.com/tphakala/go-opus/pcm"
)

// TestEncoderRuntimeBitrate confirms the pcm facade (an alias of oggopus.Encoder)
// exposes the mid-stream rate-control surface and that changing the rate while a
// stream is in progress still yields a complete, decodable Ogg Opus stream.
func TestEncoderRuntimeBitrate(t *testing.T) {
	var buf bytes.Buffer
	enc, err := pcm.NewEncoder(&buf, pcm.Config{
		SampleRate: testRate, Channels: testChannels, Bitrate: 48000,
	})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if got, err := enc.Bitrate(); err != nil || got != 48000 {
		t.Fatalf("Bitrate() = %d, %v; want 48000", got, err)
	}

	// A full 20 ms frame at 24 kHz is 480 samples/channel.
	frame := toneBytes(480, testChannels, 440, testRate)
	for f := range 24 {
		if f == 8 {
			if err := enc.SetBitrate(16000); err != nil {
				t.Fatalf("SetBitrate(cut): %v", err)
			}
		}
		if f == 16 {
			if err := enc.SetBitrate(64000); err != nil {
				t.Fatalf("SetBitrate(recover): %v", err)
			}
		}
		if _, err := enc.Write(frame); err != nil {
			t.Fatalf("Write frame %d: %v", f, err)
		}
	}
	if got, _ := enc.Bitrate(); got != 64000 {
		t.Fatalf("Bitrate() after changes = %d, want 64000", got)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out, info := decodePCM(t, buf.Bytes())
	if info.OutputSampleRate != pcm.OutputSampleRate {
		t.Fatalf("OutputSampleRate = %d, want %d", info.OutputSampleRate, pcm.OutputSampleRate)
	}
	// 24 frames of 480 samples/channel at 24 kHz input -> 960 samples/channel each
	// at the 48 kHz output.
	if want := 24 * 960 * testChannels; len(out)/2 != want {
		t.Fatalf("decoded %d samples, want %d", len(out)/2, want)
	}
}

// TestEncoderRuntimeBitrateClosed ensures the facade forwards the closed-stream
// error rather than panicking.
func TestEncoderRuntimeBitrateClosed(t *testing.T) {
	var buf bytes.Buffer
	enc, err := pcm.NewEncoder(&buf, validConfig())
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := enc.SetBitrate(24000); !errors.Is(err, pcm.ErrClosed) {
		t.Fatalf("SetBitrate after Close: got %v, want ErrClosed", err)
	}

	// A negative target reaches the codec and is a bad argument.
	var buf2 bytes.Buffer
	enc2, err := pcm.NewEncoder(&buf2, validConfig())
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := enc2.SetBitrate(-1); !errors.Is(err, opus.ErrBadArg) {
		t.Fatalf("SetBitrate(-1): got %v, want opus.ErrBadArg", err)
	}
}

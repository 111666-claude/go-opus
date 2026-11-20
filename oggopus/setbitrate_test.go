package oggopus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/tphakala/go-opus/opus"
)

// This file covers the container-facing real-time rate control
// (Encoder.SetBitrate / Bitrate): that a mid-stream change keeps every page
// sequence consecutive and every granule position on the unchanged sample
// timeline, that the whole Ogg stream still decodes, and the closed /
// uninitialized error paths.

const (
	rtTestRate     = 48000
	rtTestChannels = 1
	rtFrameSamples = 960 // 20 ms at 48 kHz
	rtFrameBytes   = rtFrameSamples * rtTestChannels * 2
)

// rtPCM builds deterministic, non-silent mono PCM for nFrames 20 ms frames.
func rtPCM(nFrames int) []byte {
	pcm := make([]byte, nFrames*rtFrameBytes)
	for i := range nFrames * rtFrameSamples {
		v := 0.4*math.Sin(2*math.Pi*0.02*float64(i)) +
			0.15*math.Sin(2*math.Pi*0.073*float64(i))
		binary.LittleEndian.PutUint16(pcm[2*i:2*i+2], uint16(int16(v*20000)))
	}
	return pcm
}

// buildAdaptiveStream writes the same PCM, applying rateChange(frameIndex) before
// the frame with that index; a nil plan means a constant bitrate. It returns the
// complete Ogg Opus bytes.
func buildAdaptiveStream(t *testing.T, baseRate int, pcm []byte, rateChange func(frame int) (int, bool)) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := NewEncoder(&buf, Config{
		SampleRate: rtTestRate, Channels: rtTestChannels, Bitrate: baseRate, CBR: true,
	})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	nFrames := len(pcm) / rtFrameBytes
	for f := range nFrames {
		if rateChange != nil {
			if br, ok := rateChange(f); ok {
				if err := enc.SetBitrate(br); err != nil {
					t.Fatalf("SetBitrate at frame %d: %v", f, err)
				}
			}
		}
		if _, err := enc.Write(pcm[f*rtFrameBytes : (f+1)*rtFrameBytes]); err != nil {
			t.Fatalf("Write frame %d: %v", f, err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

// TestOggSetBitratePageAndGranuleContinuity is the Ogg-side counterpart of the
// opus continuity test. It asserts, for a stream whose rate changes twice:
//   - page sequence numbers are exactly 0,1,2,... with no jump or repeat;
//   - granule positions are monotonic and the final one equals preSkip + the exact
//     source sample count at 48 kHz, identical to a constant-rate stream carrying
//     the same PCM (so the sample timeline never moves when the rate does);
//   - every page still CRC-verifies (scanPages uses the same readPage/parsePage
//     validation) and the last page is flagged end-of-stream;
//   - the whole stream decodes without error to the exact expected sample count.
func TestOggSetBitratePageAndGranuleContinuity(t *testing.T) {
	const nFrames = 40
	pcm := rtPCM(nFrames)
	plan := func(f int) (int, bool) {
		switch f {
		case 12:
			return 16000, true // Wi-Fi -> cellular: rate cut
		case 26:
			return 72000, true // recovery
		default:
			return 0, false
		}
	}
	adaptive := buildAdaptiveStream(t, 48000, pcm, plan)
	fixed := buildAdaptiveStream(t, 48000, pcm, nil)

	aPages := scanPages(t, adaptive)
	fPages := scanPages(t, fixed)

	for i, p := range aPages {
		if p.sequence != uint32(i) {
			t.Errorf("page %d has sequence %d: page sequence jumped after a rate change", i, p.sequence)
		}
	}
	// Granule positions must be set only on packet-completing pages and never
	// regress; header pages carry granule 0.
	var prev int64
	for _, p := range aPages {
		if p.granule != granuleNone && p.granule < prev {
			t.Errorf("granule regressed from %d to %d", prev, p.granule)
		}
		if p.granule != granuleNone {
			prev = p.granule
		}
	}
	if got := aPages[len(aPages)-1].flags & flagEOS; got == 0 {
		t.Error("final page is not flagged end-of-stream")
	}

	// The sample timeline is independent of bitrate: the sequence of meaningful
	// granule positions must match the constant-rate stream exactly.
	granules := func(pages []pageInfo) []int64 {
		out := make([]int64, 0, len(pages))
		for _, p := range pages {
			if p.granule != granuleNone {
				out = append(out, p.granule)
			}
		}
		return out
	}
	if gA, gF := granules(aPages), granules(fPages); !int64sEqual(gA, gF) {
		t.Fatalf("granule timeline changed with bitrate:\nadaptive %v\nfixed    %v", gA, gF)
	}

	// Final granule = preSkip (312) + source samples at 48 kHz (nFrames*960).
	wantFinal := int64(312 + nFrames*rtFrameSamples)
	if prev != wantFinal {
		t.Errorf("final granule %d, want %d", prev, wantFinal)
	}

	// The adaptive stream must decode end to end to the exact PCM sample count.
	out, info, err := DecodeInterleaved(bytes.NewReader(adaptive))
	if err != nil {
		t.Fatalf("DecodeInterleaved on the rate-changing stream: %v", err)
	}
	if info.OutputSampleRate != OutputSampleRate {
		t.Errorf("OutputSampleRate = %d, want %d", info.OutputSampleRate, OutputSampleRate)
	}
	if got := len(out) / 2 / rtTestChannels; got != nFrames*rtFrameSamples {
		t.Errorf("decoded %d samples/channel, want %d", got, nFrames*rtFrameSamples)
	}

	// A streaming decoder must likewise consume every packet without a discontinuity.
	dec, err := NewDecoder(bytes.NewReader(adaptive))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	tmp := make([]byte, 4096)
	var decoded int
	for {
		n, err := dec.Read(tmp)
		decoded += n / 2 / rtTestChannels
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("streaming decode: %v", err)
		}
	}
	if decoded != nFrames*rtFrameSamples {
		t.Errorf("streaming decoder produced %d samples/channel, want %d", decoded, nFrames*rtFrameSamples)
	}
}

// TestOggSetBitrateLifecycle covers the closed and uninitialized guards and the
// Bitrate() read-back, including that a rejected change leaves the target alone.
func TestOggSetBitrateLifecycle(t *testing.T) {
	var buf bytes.Buffer
	enc, err := NewEncoder(&buf, Config{SampleRate: rtTestRate, Channels: rtTestChannels, Bitrate: 40000})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if got, err := enc.Bitrate(); err != nil || got != 40000 {
		t.Fatalf("Bitrate() = %d, %v; want 40000", got, err)
	}
	if err := enc.SetBitrate(28000); err != nil {
		t.Fatalf("SetBitrate: %v", err)
	}
	if got, _ := enc.Bitrate(); got != 28000 {
		t.Fatalf("Bitrate() after change = %d, want 28000", got)
	}
	// A negative rate is rejected by the codec (the seam forwards the ctl) and
	// leaves the target unchanged. It is a bad argument, not a config error, because
	// a runtime SetBitrate never goes through Config.validate.
	if err := enc.SetBitrate(-7); !errors.Is(err, opus.ErrBadArg) {
		t.Fatalf("SetBitrate(-7): got %v, want a wrapped opus.ErrBadArg", err)
	}
	if got, _ := enc.Bitrate(); got != 28000 {
		t.Fatalf("rejected SetBitrate moved the target to %d", got)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := enc.SetBitrate(12000); !errors.Is(err, ErrClosed) {
		t.Fatalf("SetBitrate after Close: got %v, want ErrClosed", err)
	}
	if _, err := enc.Bitrate(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Bitrate after Close: got %v, want ErrClosed", err)
	}

	// A zero-value / never-initialized encoder reports errUninitialized, never panics.
	var zero Encoder
	if err := zero.SetBitrate(12000); !errors.Is(err, errUninitialized) {
		t.Fatalf("zero-value SetBitrate: got %v, want errUninitialized", err)
	}
	if _, err := zero.Bitrate(); !errors.Is(err, errUninitialized) {
		t.Fatalf("zero-value Bitrate: got %v, want errUninitialized", err)
	}
}

func int64sEqual(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

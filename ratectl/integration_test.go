package ratectl_test

import (
	"bytes"
	"math"
	"testing"
	"time"

	"github.com/tphakala/go-opus/oggopus"
	"github.com/tphakala/go-opus/opus"
	"github.com/tphakala/go-opus/ratectl"
)

// This is the end-to-end layer: a real opus.Encoder and a real oggopus.Encoder are
// driven by a ratectl.Controller fed scripted "network" feedback that mimics a
// Wi-Fi -> cellular handoff (a healthy start, a congested middle, a recovery). The
// fixed-point codec does all the encoding here; ratectl only supplies integer
// targets. It verifies the applied bitrate trajectory reacts in the right
// direction, stays bounded, and that every packet / the whole Ogg stream remain
// valid and continuously decodable.

type e2eClock struct{ t time.Time }

func (c *e2eClock) now() time.Time       { return c.t }
func (c *e2eClock) step(d time.Duration) { c.t = c.t.Add(d) }

// e2ePCM is deterministic non-silent mono PCM for one 20 ms frame at 48 kHz.
func e2ePCM(seed int) []int16 {
	const n = 960
	pcm := make([]int16, n)
	for i := range n {
		v := 0.4*math.Sin(2*math.Pi*0.03*float64(i)+float64(seed)*0.5) +
			0.1*math.Sin(2*math.Pi*0.11*float64(i))
		pcm[i] = int16(v * 21000)
	}
	return pcm
}

// TestControllerDrivesRealEncoder simulates the handoff against a real CELT
// encoder and a single decoder consuming the entire packet sequence.
func TestControllerDrivesRealEncoder(t *testing.T) {
	clk := &e2eClock{}
	enc, err := opus.NewEncoder(opus.EncoderConfig{
		SampleRate: 48000, Channels: 1, Bitrate: 64000, CBR: true,
	})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	dec, err := opus.NewDecoder(48000, 1)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}

	ctl, err := ratectl.New(ratectl.Config{
		InitialBitrate:       64000,
		MinBitrate:           8000,
		MaxBitrate:           128000,
		MaxFeedbackAge:       time.Hour,
		TargetQueueDelay:     100 * time.Millisecond,
		MaxDecreasePerSecond: 250000,
		MaxIncreasePerSecond: 120000,
		Clock:                clk.now,
	})
	if err != nil {
		t.Fatalf("ratectl.New: %v", err)
	}

	// phase(f) returns the simulated condition at frame f.
	type phase struct {
		loss float64
		bw   float64
	}
	condition := func(f int) phase {
		switch {
		case f < 15:
			return phase{0.0, 0} // healthy Wi-Fi
		case f < 40:
			return phase{0.30, 16000} // cellular congestion
		default:
			return phase{0.0, 0} // recovery
		}
	}

	const frames = 60
	pktBuf := make([]byte, 4000)
	out := make([]int16, 960)
	trajectory := make([]int, 0, frames)
	congestionMin := math.MaxInt

	for f := range frames {
		// Feedback every 4 frames, stamped with the controller clock.
		if f%4 == 0 {
			c := condition(f)
			if _, err := ctl.Update(ratectl.NetworkFeedback{
				Time:               clk.now(),
				PacketLoss:         c.loss,
				QueueDelay:         0,
				AvailableBandwidth: c.bw,
			}); err != nil {
				t.Fatalf("frame %d Update: %v", f, err)
			}
		}
		applied, err := ctl.ApplyTo(enc)
		if err != nil {
			t.Fatalf("frame %d ApplyTo: %v", f, err)
		}
		if applied < 8000 || applied > 128000 {
			t.Fatalf("frame %d applied %d outside configured bounds", f, applied)
		}
		trajectory = append(trajectory, applied)
		if f >= 15 && f < 40 && applied < congestionMin {
			congestionMin = applied
		}

		n, err := enc.Encode(e2ePCM(f), pktBuf)
		if err != nil {
			t.Fatalf("frame %d Encode: %v", f, err)
		}
		// One decoder for the whole stream: any discontinuity from a rate change
		// would surface here.
		if got, err := dec.Decode(pktBuf[:n], out); err != nil || got != 960 {
			t.Fatalf("frame %d decode: got %d samples, err=%v", f, got, err)
		}
		clk.step(20 * time.Millisecond)
	}

	// Directional assertions on the trajectory, robust to exact ramp values.
	if congestionMin >= 64000 {
		t.Errorf("congestion never lowered the bitrate below the start: min %d", congestionMin)
	}
	if congestionMin > 16000 {
		t.Errorf("congestion min %d did not approach the 16000 bps bandwidth estimate", congestionMin)
	}
	// The first engaged application pins the encoder to its original fixed target;
	// healthy probing only begins to move it afterwards. (The stricter "no feedback
	// at all means never touch the encoder" guarantee is TestNoFeedbackKeepsFixed.)
	if trajectory[0] != 64000 {
		t.Errorf("first applied target %d, want the fixed 64000 start", trajectory[0])
	}
	// After recovery the final target must be climbing back, not stuck at the floor.
	if trajectory[frames-1] <= congestionMin {
		t.Errorf("bitrate did not recover after the handoff: final %d, congestion min %d",
			trajectory[frames-1], congestionMin)
	}
}

// TestControllerDrivesOggEncoder proves the container encoder satisfies the
// BitrateSetter sink and that a controller-driven, rate-changing Ogg stream still
// decodes to the exact sample count. Page/granule continuity is asserted directly
// in oggopus's own tests; here the cross-package wiring and full decode are checked.
func TestControllerDrivesOggEncoder(t *testing.T) {
	clk := &e2eClock{}
	var buf bytes.Buffer
	oenc, err := oggopus.NewEncoder(&buf, oggopus.Config{
		SampleRate: 48000, Channels: 1, Bitrate: 48000,
	})
	if err != nil {
		t.Fatalf("oggopus.NewEncoder: %v", err)
	}

	// Compile-time guarantee that the container encoder is a valid controller sink.
	var _ ratectl.BitrateSetter = oenc

	ctl, err := ratectl.New(ratectl.Config{
		InitialBitrate:       48000,
		MinBitrate:           8000,
		MaxBitrate:           96000,
		MaxFeedbackAge:       time.Hour,
		MaxDecreasePerSecond: 300000,
		Clock:                clk.now,
	})
	if err != nil {
		t.Fatalf("ratectl.New: %v", err)
	}

	const frames = 30
	frameBytes := 960 * 2
	pcm := make([]byte, 0, frames*frameBytes)
	for f := range frames {
		for _, s := range e2ePCM(f) {
			pcm = append(pcm, byte(s), byte(s>>8))
		}
	}
	for f := range frames {
		if f == 10 {
			if _, err := ctl.Update(ratectl.NetworkFeedback{
				Time: clk.now(), PacketLoss: 0.35, AvailableBandwidth: 12000,
			}); err != nil {
				t.Fatalf("Update: %v", err)
			}
		}
		if _, err := ctl.ApplyTo(oenc); err != nil {
			t.Fatalf("frame %d ApplyTo: %v", f, err)
		}
		if _, err := oenc.Write(pcm[f*frameBytes : (f+1)*frameBytes]); err != nil {
			t.Fatalf("frame %d Write: %v", f, err)
		}
		clk.step(20 * time.Millisecond)
	}
	if err := oenc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out, info, err := oggopus.DecodeInterleaved(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("DecodeInterleaved of a controller-driven stream: %v", err)
	}
	if info.OutputSampleRate != 48000 {
		t.Errorf("OutputSampleRate = %d, want 48000", info.OutputSampleRate)
	}
	if got := len(out) / 2; got != frames*960 {
		t.Errorf("decoded %d samples/channel, want %d", got, frames*960)
	}
}

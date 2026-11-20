package ratectl

import (
	"errors"
	"math"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock so every trajectory is deterministic and
// no test depends on wall-clock scheduling.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// fakeEncoder records every SetBitrate call and can be forced to error once, so
// the controller's application path and its error handling are observable without
// a real codec.
type fakeEncoder struct {
	calls    []int
	failNext bool
	err      error
}

func (f *fakeEncoder) SetBitrate(bps int) error {
	if f.failNext {
		f.failNext = false
		return f.err
	}
	f.calls = append(f.calls, bps)
	return nil
}

func (f *fakeEncoder) last() (int, bool) {
	if len(f.calls) == 0 {
		return 0, false
	}
	return f.calls[len(f.calls)-1], true
}

// baseCfg returns a valid config wired to clk with generous staleness so tests
// that pass explicit timestamps do not trip the age guard unless they mean to.
func baseCfg(clk *fakeClock) Config {
	return Config{
		InitialBitrate:       64000,
		MinBitrate:           8000,
		MaxBitrate:           128000,
		MaxFeedbackAge:       time.Hour,
		TargetQueueDelay:     100 * time.Millisecond,
		Clock:                clk.now,
		MaxIncreasePerSecond: 1_000_000, // effectively no time cap on the way up
		MaxDecreasePerSecond: 1_000_000, // or down; per-update fractions still bind
	}
}

func TestNewValidation(t *testing.T) {
	clk := &fakeClock{}
	good := baseCfg(clk)
	if _, err := New(good); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	bad := []Config{
		func() Config { c := good; c.InitialBitrate = 0; return c }(),
		func() Config { c := good; c.InitialBitrate = -1; return c }(),
		func() Config { c := good; c.MinBitrate = 0; c.MaxBitrate = -5; return c }(),
		func() Config { c := good; c.MinBitrate = 100000; c.MaxBitrate = 50000; return c }(), // inverted
		func() Config { c := good; c.MaxIncreaseFraction = 1.5; return c }(),
		func() Config { c := good; c.MaxDecreaseFraction = -0.1; return c }(),
		func() Config { c := good; c.QueueSafetyFactor = 0.5; return c }(),
		func() Config { c := good; c.LossThreshold = 0.5; c.HighLossThreshold = 0.2; return c }(),
		func() Config { c := good; c.LossThreshold = -1; return c }(),
		func() Config { c := good; c.MaxFeedbackAge = -time.Second; return c }(),
		func() Config { c := good; c.TargetQueueDelay = -time.Millisecond; return c }(),
	}
	for i, c := range bad {
		if _, err := New(c); !errors.Is(err, ErrBadConfig) {
			t.Errorf("bad config #%d: got %v, want ErrBadConfig", i, err)
		}
	}

	// InitialBitrate outside the bounds is clamped, not rejected.
	c := good
	c.InitialBitrate = 1_000_000
	ctl, err := New(c)
	if err != nil {
		t.Fatalf("clamp config: %v", err)
	}
	if got := ctl.TargetBitrate(); got != c.MaxBitrate {
		t.Errorf("InitialBitrate above Max: target %d, want clamp to %d", got, c.MaxBitrate)
	}

	// Zero-value tuning fields are filled with defaults.
	defaults, err := New(Config{InitialBitrate: 32000})
	if err != nil {
		t.Fatalf("minimal config: %v", err)
	}
	if defaults.cfg.MaxIncreaseFraction != defaultMaxIncreaseFraction ||
		defaults.cfg.MaxDecreaseFraction != defaultMaxDecreaseFraction ||
		defaults.cfg.MaxFeedbackAge != defaultMaxFeedbackAge {
		t.Errorf("defaults not applied: %+v", defaults.cfg)
	}
}

// TestNoFeedbackKeepsFixed is the core "无反馈时保持现有固定配置" guarantee: before
// any feedback is accepted the controller must never call SetBitrate, however many
// frames are produced, and the encoder keeps the bitrate it was built with.
func TestNoFeedbackKeepsFixed(t *testing.T) {
	clk := &fakeClock{}
	ctl, err := New(baseCfg(clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	enc := &fakeEncoder{}
	for i := range 10 {
		got, err := ctl.ApplyTo(enc)
		if err != nil {
			t.Fatalf("ApplyTo %d: %v", i, err)
		}
		if got != 64000 {
			t.Fatalf("ApplyTo %d returned %d before engagement, want 64000", i, got)
		}
		clk.advance(20 * time.Millisecond)
	}
	if ctl.Engaged() {
		t.Fatal("controller engaged without any accepted feedback")
	}
	if len(enc.calls) != 0 {
		t.Fatalf("SetBitrate called %v before any feedback; fixed config must be untouched", enc.calls)
	}
}

// TestZeroBandwidthNotFloor pins that a zero AvailableBandwidth means "no
// estimate", not a zero-capacity link: a healthy sample with zero bandwidth must
// not drive the target to the floor.
func TestZeroBandwidthNotFloor(t *testing.T) {
	clk := &fakeClock{}
	ctl, _ := New(baseCfg(clk))
	ok, err := ctl.Update(NetworkFeedback{PacketLoss: 0, QueueDelay: 0, AvailableBandwidth: 0})
	if err != nil || !ok {
		t.Fatalf("healthy zero-bandwidth sample: accepted=%v err=%v", ok, err)
	}
	if ctl.DesiredBitrate() <= 64000 {
		t.Fatalf("zero bandwidth collapsed the target to %d; it should probe upward, not to the floor",
			ctl.DesiredBitrate())
	}
}

// TestNonFiniteAndOutOfRangeRejected covers the "非有限指标" and domain checks: every
// malformed metric returns ErrBadFeedback and leaves the desired target at its
// initial value.
func TestNonFiniteAndOutOfRangeRejected(t *testing.T) {
	clk := &fakeClock{}
	ctl, _ := New(baseCfg(clk))
	bad := []NetworkFeedback{
		{PacketLoss: math.NaN()},
		{PacketLoss: math.Inf(1)},
		{PacketLoss: -0.01},
		{PacketLoss: 1.01},
		{QueueDelay: -time.Millisecond},
		{AvailableBandwidth: math.NaN()},
		{AvailableBandwidth: math.Inf(-1)},
		{AvailableBandwidth: -100},
	}
	for i, fb := range bad {
		ok, err := ctl.Update(fb)
		if !errors.Is(err, ErrBadFeedback) {
			t.Errorf("sample #%d: got (accepted=%v, err=%v), want ErrBadFeedback", i, ok, err)
		}
		if ctl.Engaged() {
			t.Fatalf("sample #%d engaged the controller despite being rejected", i)
		}
		if ctl.DesiredBitrate() != 64000 {
			t.Fatalf("sample #%d mutated desired to %d after rejection", i, ctl.DesiredBitrate())
		}
	}
}

// TestStaleAndOutOfOrder covers the "过期/乱序反馈" handling. A stale sample (older
// than MaxFeedbackAge) and an out-of-order/duplicate sample are both ignored
// without error and without moving the desired target.
func TestStaleAndOutOfOrder(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	cfg := baseCfg(clk)
	cfg.MaxFeedbackAge = 500 * time.Millisecond
	ctl, _ := New(cfg)

	// Stale: observed well before the controller clock, beyond MaxFeedbackAge.
	ok, err := ctl.Update(NetworkFeedback{
		Time: clk.t.Add(-2 * time.Second), PacketLoss: 0.9, AvailableBandwidth: 1000,
	})
	if err != nil || ok {
		t.Fatalf("stale sample: accepted=%v err=%v, want (false,nil)", ok, err)
	}
	if ctl.Engaged() {
		t.Fatal("a stale sample engaged the controller")
	}

	// A fresh, valid sample engages and lowers the desired target.
	first := NetworkFeedback{Time: clk.t, PacketLoss: 0.3, AvailableBandwidth: 20000}
	if ok, err := ctl.Update(first); err != nil || !ok {
		t.Fatalf("first valid sample: accepted=%v err=%v", ok, err)
	}
	desiredAfterFirst := ctl.DesiredBitrate()

	// Out-of-order: an earlier timestamp than the accepted one.
	ok, err = ctl.Update(NetworkFeedback{
		Time: clk.t.Add(-100 * time.Millisecond), PacketLoss: 0, AvailableBandwidth: 200000,
	})
	if err != nil || ok {
		t.Fatalf("out-of-order sample: accepted=%v err=%v, want (false,nil)", ok, err)
	}
	if ctl.DesiredBitrate() != desiredAfterFirst {
		t.Fatalf("out-of-order sample moved desired from %d to %d",
			desiredAfterFirst, ctl.DesiredBitrate())
	}

	// A duplicate (identical timestamp) is ignored too.
	if ok, _ := ctl.Update(first); ok {
		t.Fatal("a duplicate feedback sample was accepted")
	}
}

// TestArrivalOrderZeroTime pins the zero-Time contract: samples carrying no
// timestamp are "current, in arrival order" and must all be accepted even when the
// controller clock does not advance between them. This is the path a caller without
// a shared transport clock uses, so a frozen clock must not turn them into
// duplicates.
func TestArrivalOrderZeroTime(t *testing.T) {
	clk := &fakeClock{} // deliberately never advanced
	ctl, _ := New(baseCfg(clk))
	for i := range 5 {
		ok, err := ctl.Update(NetworkFeedback{PacketLoss: float64(i) * 0.05, AvailableBandwidth: 0})
		if err != nil || !ok {
			t.Fatalf("arrival-order sample %d: accepted=%v err=%v", i, ok, err)
		}
	}
	if ctl.LastFeedbackTime() != clk.now() {
		t.Fatalf("zero-time samples were not stamped with the controller clock")
	}
}

// TestLossBackoffTrajectory drives a scripted rise in packet loss and asserts the
// desired target reacts monotonically downward, never outside the configured
// bounds, and saturates rather than overshooting at high loss.
func TestLossBackoffTrajectory(t *testing.T) {
	clk := &fakeClock{}
	ctl, _ := New(baseCfg(clk))
	prev := ctl.DesiredBitrate()
	losses := []float64{0.0, 0.01, 0.05, 0.10, 0.25, 0.40}
	for i, loss := range losses {
		if ok, err := ctl.Update(NetworkFeedback{PacketLoss: loss, AvailableBandwidth: 0}); err != nil || !ok {
			t.Fatalf("Update loss=%v: accepted=%v err=%v", loss, ok, err)
		}
		got := ctl.DesiredBitrate()
		if got < ctl.cfg.MinBitrate || got > ctl.cfg.MaxBitrate {
			t.Fatalf("step %d desired %d outside [%d,%d]", i, got, ctl.cfg.MinBitrate, ctl.cfg.MaxBitrate)
		}
		if loss > ctl.cfg.LossThreshold && got > prev {
			t.Errorf("step %d loss=%.2f raised desired %d -> %d", i, loss, prev, got)
		}
		// A single update can never cut by more than the configured fraction.
		if drop := float64(prev-got) / float64(prev); drop > ctl.cfg.MaxDecreaseFraction+1e-9 {
			t.Errorf("step %d cut %.3f of target, exceeds MaxDecreaseFraction %.3f",
				i, drop, ctl.cfg.MaxDecreaseFraction)
		}
		prev = got
	}
	if ctl.DesiredBitrate() < ctl.cfg.MinBitrate {
		t.Fatal("target fell below MinBitrate")
	}
}

// TestQueueDelayBackoff pins the delay law: inside the tolerated bound no cut,
// above it a proportional cut bounded by MaxDecreaseFraction.
func TestQueueDelayBackoff(t *testing.T) {
	clk := &fakeClock{}
	ctl, _ := New(baseCfg(clk))
	start := ctl.DesiredBitrate()

	// At exactly the tolerated bound (100ms * 1.5) nothing happens.
	if _, err := ctl.Update(NetworkFeedback{QueueDelay: 150 * time.Millisecond, AvailableBandwidth: 0}); err != nil {
		t.Fatal(err)
	}
	if ctl.DesiredBitrate() <= start {
		t.Fatalf("delay within tolerance cut target to %d", ctl.DesiredBitrate())
	}

	// A large queue overshoot must cut, but never more than the fraction cap in one
	// step relative to the live target.
	live := ctl.TargetBitrate()
	if _, err := ctl.Update(NetworkFeedback{QueueDelay: time.Second, AvailableBandwidth: 0}); err != nil {
		t.Fatal(err)
	}
	got := ctl.DesiredBitrate()
	if got >= live {
		t.Fatalf("1s queue delay did not reduce target (live=%d desired=%d)", live, got)
	}
	if float64(live-got)/float64(live) > ctl.cfg.MaxDecreaseFraction+1e-9 {
		t.Fatalf("delay cut exceeded MaxDecreaseFraction")
	}
}

// TestBandwidthCeiling checks that an available-bandwidth estimate caps the target
// even when loss and delay are healthy (which would otherwise probe upward to
// 64000*1.25=80000). The estimate is kept above the one-step decrease floor so the
// ceiling, not the fraction cap, is what binds.
func TestBandwidthCeiling(t *testing.T) {
	clk := &fakeClock{}
	ctl, _ := New(baseCfg(clk))
	if _, err := ctl.Update(NetworkFeedback{PacketLoss: 0, QueueDelay: 0, AvailableBandwidth: 50000}); err != nil {
		t.Fatal(err)
	}
	if got := ctl.DesiredBitrate(); got > 50000 {
		t.Fatalf("desired %d exceeds the 50000 bps bandwidth estimate (healthy probing would give 80000)", got)
	}
}

// TestBurstyJitterIsSmoothed proves a single isolated loss spike cannot swing the
// rate the way a sustained condition does: the EMA plus the per-update fraction
// cap bound the one-step reaction, and healthy samples recover it.
func TestBurstyJitterIsSmoothed(t *testing.T) {
	clk := &fakeClock{}
	ctl, _ := New(baseCfg(clk))

	// Establish a healthy baseline over a few samples.
	for range 3 {
		if _, err := ctl.Update(NetworkFeedback{PacketLoss: 0, QueueDelay: 0, AvailableBandwidth: 0}); err != nil {
			t.Fatal(err)
		}
	}
	healthyDesired := ctl.DesiredBitrate()
	live := ctl.TargetBitrate()

	// One severe spike.
	if _, err := ctl.Update(NetworkFeedback{PacketLoss: 0.5, QueueDelay: 0, AvailableBandwidth: 0}); err != nil {
		t.Fatal(err)
	}
	spikeDesired := ctl.DesiredBitrate()
	if spikeDesired >= healthyDesired {
		t.Fatal("a severe loss spike did not lower the desired target at all")
	}
	if drop := float64(live-spikeDesired) / float64(live); drop > ctl.cfg.MaxDecreaseFraction+1e-9 {
		t.Fatalf("single spike cut %.3f of the target, beyond the %.3f cap", drop, ctl.cfg.MaxDecreaseFraction)
	}

	// Sustained healthy feedback recovers toward the original region.
	for range 4 {
		if _, err := ctl.Update(NetworkFeedback{PacketLoss: 0, QueueDelay: 0, AvailableBandwidth: 0}); err != nil {
			t.Fatal(err)
		}
	}
	if ctl.DesiredBitrate() <= spikeDesired {
		t.Fatal("target did not recover after the burst passed")
	}
}

// TestRampLimitAndNoOvershoot verifies the per-second change speed: a desired drop
// much larger than the ramp budget is spread over time, and the live target never
// crosses the desired value in either direction.
func TestRampLimitAndNoOvershoot(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	cfg := baseCfg(clk)
	cfg.MaxDecreasePerSecond = 20000 // 20 kbps per wall-clock second
	cfg.MaxIncreasePerSecond = 20000
	ctl, _ := New(cfg)
	enc := &fakeEncoder{}

	// Drive desired far below the 64000 starting target.
	if _, err := ctl.Update(NetworkFeedback{PacketLoss: 0.4, AvailableBandwidth: 12000}); err != nil {
		t.Fatal(err)
	}
	desired := ctl.DesiredBitrate()

	// First engaged ApplyTo pins the initial target.
	if got, err := ctl.ApplyTo(enc); err != nil || got != 64000 {
		t.Fatalf("first ApplyTo = %d, %v; want 64000", got, err)
	}
	// Advance 100ms: at most 2000 bps of downward movement.
	clk.advance(100 * time.Millisecond)
	got, err := ctl.ApplyTo(enc)
	if err != nil {
		t.Fatal(err)
	}
	if moved := 64000 - got; moved > 2000 {
		t.Fatalf("100ms moved target down by %d bps, ramp allows ~2000", moved)
	}
	if got < desired {
		t.Fatalf("ramp overshot desired: live %d < desired %d", got, desired)
	}

	// Walk the clock forward until convergence; the live target must land exactly on
	// desired and never pass it.
	for range 50 {
		clk.advance(100 * time.Millisecond)
		got, err = ctl.ApplyTo(enc)
		if err != nil {
			t.Fatal(err)
		}
		if got < desired {
			t.Fatalf("ramp overshot on the way down: %d < %d", got, desired)
		}
		if got == desired {
			break
		}
	}
	if got != desired {
		t.Fatalf("did not converge to desired %d, got %d", desired, got)
	}
}

// TestRedundantApplySkipsSetBitrate ensures that once the live target reaches the
// desired target, a steady target does not re-issue the ctl on every frame.
func TestRedundantApplySkipsSetBitrate(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	cfg := baseCfg(clk)
	cfg.MaxIncreasePerSecond = 1_000_000
	ctl, _ := New(cfg)
	enc := &fakeEncoder{}
	if _, err := ctl.Update(NetworkFeedback{PacketLoss: 0, AvailableBandwidth: 0}); err != nil {
		t.Fatal(err)
	}
	// Ramp until the live target converges to desired.
	for range 20 {
		clk.advance(20 * time.Millisecond)
		if _, err := ctl.ApplyTo(enc); err != nil {
			t.Fatal(err)
		}
		if ctl.TargetBitrate() == ctl.DesiredBitrate() {
			break
		}
	}
	if ctl.TargetBitrate() != ctl.DesiredBitrate() {
		t.Fatal("precondition: target never converged")
	}
	steadyCalls := len(enc.calls)
	// With no further feedback the desired target does not move, so additional
	// frames must not issue any more SetBitrate calls.
	for range 5 {
		clk.advance(20 * time.Millisecond)
		if _, err := ctl.ApplyTo(enc); err != nil {
			t.Fatal(err)
		}
	}
	if len(enc.calls) != steadyCalls {
		t.Fatalf("a converged steady target issued %d extra SetBitrate calls", len(enc.calls)-steadyCalls)
	}
}

// TestEncoderErrorPropagatesButRampAdvances checks that a SetBitrate failure is
// returned to the caller while the controller still advances its ramp clock, so a
// later successful frame continues from the right place.
func TestEncoderErrorPropagatesButRampAdvances(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	cfg := baseCfg(clk)
	cfg.MaxDecreasePerSecond = 100000
	ctl, _ := New(cfg)
	enc := &fakeEncoder{err: errors.New("codec rejected")}

	if _, err := ctl.Update(NetworkFeedback{PacketLoss: 0.3, AvailableBandwidth: 16000}); err != nil {
		t.Fatal(err)
	}
	if _, err := ctl.ApplyTo(&fakeEncoder{}); err != nil { // engage on a good encoder
		t.Fatal(err)
	}
	clk.advance(100 * time.Millisecond)
	enc.failNext = true
	if _, err := ctl.ApplyTo(enc); !errors.Is(err, enc.err) {
		t.Fatalf("encoder error not propagated: got %v", err)
	}
	// The ramp clock advanced despite the failure: another step keeps moving rather
	// than restarting from the old lastApply.
	before := ctl.TargetBitrate()
	clk.advance(100 * time.Millisecond)
	if _, err := ctl.ApplyTo(&fakeEncoder{}); err != nil {
		t.Fatal(err)
	}
	if ctl.TargetBitrate() == before && ctl.TargetBitrate() != ctl.DesiredBitrate() {
		t.Fatal("ramp timing froze after an encoder error")
	}
}

// TestClosedLifecycle covers "关闭后更新": after Close every mutating call returns
// ErrClosed and the encoder is never touched; Close is idempotent.
func TestClosedLifecycle(t *testing.T) {
	clk := &fakeClock{}
	ctl, _ := New(baseCfg(clk))
	enc := &fakeEncoder{}
	if err := ctl.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := ctl.Close(); err != nil {
		t.Fatalf("idempotent Close: %v", err)
	}
	if _, err := ctl.Update(NetworkFeedback{PacketLoss: 0.1}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Update after Close: got %v, want ErrClosed", err)
	}
	if _, err := ctl.ApplyTo(enc); !errors.Is(err, ErrClosed) {
		t.Fatalf("ApplyTo after Close: got %v, want ErrClosed", err)
	}
	if len(enc.calls) != 0 {
		t.Fatalf("a closed controller touched the encoder: %v", enc.calls)
	}
}

// TestRampUpNoOvershoot exercises the upward direction explicitly.
func TestRampUpNoOvershoot(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	cfg := baseCfg(clk)
	cfg.MaxIncreasePerSecond = 16000
	ctl, _ := New(cfg)
	// Healthy feedback raises desired above the 64000 start (capped at MaxBitrate).
	if _, err := ctl.Update(NetworkFeedback{PacketLoss: 0, QueueDelay: 0, AvailableBandwidth: 0}); err != nil {
		t.Fatal(err)
	}
	desired := ctl.DesiredBitrate()
	if desired <= 64000 {
		t.Fatalf("healthy path did not raise desired above start: %d", desired)
	}
	enc := &fakeEncoder{}
	if _, err := ctl.ApplyTo(enc); err != nil { // engage at start
		t.Fatal(err)
	}
	got := 64000
	for range 60 {
		clk.advance(100 * time.Millisecond)
		got, _ = ctl.ApplyTo(enc)
		if got > desired {
			t.Fatalf("upward ramp overshot: %d > desired %d", got, desired)
		}
		if got == desired {
			return
		}
	}
	t.Fatalf("did not converge upward to %d, last %d", desired, got)
}

// TestInitialBitrateClampedToFloor covers the lower-bound clamp in New: an
// InitialBitrate below MinBitrate is raised to the floor.
func TestInitialBitrateClampedToFloor(t *testing.T) {
	clk := &fakeClock{}
	cfg := baseCfg(clk)
	cfg.InitialBitrate = 1000
	cfg.MinBitrate = 8000
	ctl, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := ctl.TargetBitrate(); got != 8000 {
		t.Fatalf("InitialBitrate below floor: target %d, want clamp to 8000", got)
	}
}

// TestDisabledTimeCapStillBoundedByFraction sets both per-second ramps to a
// negative value to disable the time-based cap; the live target then reaches the
// desired target in one step, but the per-update multiplicative fraction (already
// applied to desired) still bounds how far a single feedback event can move it.
func TestDisabledTimeCapStillBoundedByFraction(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	cfg := baseCfg(clk)
	cfg.MaxIncreasePerSecond = -1 // disable the time cap in both directions
	cfg.MaxDecreasePerSecond = -1
	ctl, _ := New(cfg)
	enc := &fakeEncoder{}
	if _, err := ctl.Update(NetworkFeedback{PacketLoss: 0.4, AvailableBandwidth: 16000}); err != nil {
		t.Fatal(err)
	}
	desired := ctl.DesiredBitrate()
	if _, err := ctl.ApplyTo(enc); err != nil { // engage
		t.Fatal(err)
	}
	clk.advance(20 * time.Millisecond)
	got, err := ctl.ApplyTo(enc)
	if err != nil {
		t.Fatal(err)
	}
	// With no time cap a single 20 ms step already converges on desired, which is
	// itself no lower than the 50% one-step fraction floor of the 64000 start.
	if got != desired {
		t.Fatalf("disabled time cap: got %d, want immediate convergence to %d", got, desired)
	}
	if got < 32000 {
		t.Fatalf("a single feedback event moved below the fraction floor: %d", got)
	}
}

// TestRTTAcceptedAndNonMonotonicClock exercises the optional RTT path and the
// defensive non-monotonic-clock clamp (a negative elapsed time must not produce a
// negative ramp step).
func TestRTTAcceptedAndNonMonotonicClock(t *testing.T) {
	clk := &fakeClock{t: time.Unix(100, 0)}
	ctl, _ := New(baseCfg(clk))
	enc := &fakeEncoder{}
	// An RTT-bearing sample is accepted and engages the controller.
	if _, err := ctl.Update(NetworkFeedback{
		Time: clk.now(), PacketLoss: 0.2, RTT: 150 * time.Millisecond, AvailableBandwidth: 0,
	}); err != nil {
		t.Fatalf("Update with RTT: %v", err)
	}
	if !ctl.haveRTT || ctl.smoothedRTT != 150*time.Millisecond {
		t.Fatalf("RTT not recorded: have=%v rtt=%s", ctl.haveRTT, ctl.smoothedRTT)
	}
	if _, err := ctl.ApplyTo(enc); err != nil { // engage, sets lastApply
		t.Fatal(err)
	}
	// Move the clock backwards. The ramp must treat elapsed time as zero and keep
	// the target stable rather than compute a negative step.
	clk.t = clk.t.Add(-time.Second)
	before := ctl.TargetBitrate()
	got, err := ctl.ApplyTo(enc)
	if err != nil {
		t.Fatalf("ApplyTo with a backwards clock: %v", err)
	}
	if got != before {
		t.Fatalf("backwards clock moved target %d -> %d", before, got)
	}
}

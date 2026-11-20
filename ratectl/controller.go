package ratectl

import (
	"math"
	"time"
)

// metricEMA is the smoothing factor applied to each incoming metric. A bursty
// single sample (one isolated loss spike or one inflated delay reading) therefore
// moves the smoothed estimate only halfway, and the per-update fraction cap plus
// the per-second ramp absorb the rest, which is how "突发抖动" (bursty jitter) is
// prevented from swinging the bitrate. It is fixed rather than configurable: it is
// an internal anti-aliasing filter, while the policy knobs a caller tunes live in
// Config.
const metricEMA = 0.5

// Controller drives one encoder's target bitrate from NetworkFeedback. It is
// stateful (smoothed metrics, the live and desired targets, timing for the ramp)
// but holds no goroutine and no encoder ownership: the caller pumps Update as
// feedback arrives and ApplyTo once per frame. It is not safe for concurrent use;
// one Controller belongs to one encoder on one goroutine.
type Controller struct {
	cfg Config

	// Live target the encoder is (or will next be) held at, and the desired target
	// the most recent feedback converged on. target starts at InitialBitrate and is
	// only ever moved by ApplyTo's ramp; desired jumps per Update but is bounded by
	// the fraction caps. Both are always within [MinBitrate, MaxBitrate].
	target  int
	desired int

	// Smoothed metrics and the bookkeeping that guards their validity.
	smoothedLoss     float64
	smoothedDelayS   float64
	smoothedBW       float64
	smoothedRTT      time.Duration
	haveMetrics      bool
	haveRTT          bool
	lastFeedback     time.Time
	haveLastFeedback bool

	// Engagement / ramp bookkeeping. engaged flips on the first accepted feedback;
	// before it ApplyTo never touches the encoder, so a stream with no adaptation
	// input keeps the fixed bitrate it was built with. lastApply times the
	// per-second ramp; lastAppliedBps lets ApplyTo skip a redundant SetBitrate.
	engaged        bool
	lastApply      time.Time
	lastAppliedBps int
	closed         bool
}

// New builds a Controller for cfg, filling every zero field with its documented
// default and validating the domains. It returns ErrBadConfig (and no controller)
// for a non-positive InitialBitrate, inverted or non-positive bounds, a fraction
// outside [0,1], a queue safety factor below 1, or a negative feedback age.
//
//nolint:gocritic // Config passed by value to match the opus/oggopus flat-config constructor API; it is built once per controller, never per frame, so the copy is not on a hot path.
func New(cfg Config) (*Controller, error) {
	if cfg.InitialBitrate <= 0 {
		return nil, errf("%w: InitialBitrate %d must be positive", ErrBadConfig, cfg.InitialBitrate)
	}
	if cfg.MinBitrate == 0 {
		cfg.MinBitrate = defaultMinBitrate
	}
	if cfg.MaxBitrate == 0 {
		cfg.MaxBitrate = defaultMaxBitrate
	}
	if cfg.MinBitrate < 0 || cfg.MaxBitrate < 0 || cfg.MinBitrate > cfg.MaxBitrate {
		return nil, errf("%w: bitrate bounds must satisfy 0 < MinBitrate(%d) <= MaxBitrate(%d)",
			ErrBadConfig, cfg.MinBitrate, cfg.MaxBitrate)
	}
	if cfg.MaxIncreaseFraction == 0 {
		cfg.MaxIncreaseFraction = defaultMaxIncreaseFraction
	}
	if cfg.MaxDecreaseFraction == 0 {
		cfg.MaxDecreaseFraction = defaultMaxDecreaseFraction
	}
	if err := checkFraction("MaxIncreaseFraction", cfg.MaxIncreaseFraction); err != nil {
		return nil, err
	}
	if err := checkFraction("MaxDecreaseFraction", cfg.MaxDecreaseFraction); err != nil {
		return nil, err
	}
	if cfg.MaxIncreasePerSecond == 0 {
		cfg.MaxIncreasePerSecond = defaultMaxIncreasePerSecond
	}
	if cfg.MaxDecreasePerSecond == 0 {
		cfg.MaxDecreasePerSecond = defaultMaxDecreasePerSecond
	}
	if cfg.TargetQueueDelay == 0 {
		cfg.TargetQueueDelay = 100 * time.Millisecond
	}
	if cfg.TargetQueueDelay < 0 {
		return nil, errf("%w: TargetQueueDelay %s must be non-negative", ErrBadConfig, cfg.TargetQueueDelay)
	}
	if cfg.QueueSafetyFactor == 0 {
		cfg.QueueSafetyFactor = defaultQueueSafetyFactor
	}
	if cfg.QueueSafetyFactor < 1 {
		return nil, errf("%w: QueueSafetyFactor %v must be >= 1", ErrBadConfig, cfg.QueueSafetyFactor)
	}
	if cfg.LossThreshold == 0 {
		cfg.LossThreshold = defaultLossThreshold
	}
	if cfg.HighLossThreshold == 0 {
		cfg.HighLossThreshold = defaultHighLossThreshold
	}
	if err := checkFraction("LossThreshold", cfg.LossThreshold); err != nil {
		return nil, err
	}
	if err := checkFraction("HighLossThreshold", cfg.HighLossThreshold); err != nil {
		return nil, err
	}
	if cfg.HighLossThreshold <= cfg.LossThreshold {
		return nil, errf("%w: HighLossThreshold(%v) must exceed LossThreshold(%v)",
			ErrBadConfig, cfg.HighLossThreshold, cfg.LossThreshold)
	}
	if cfg.MaxFeedbackAge == 0 {
		cfg.MaxFeedbackAge = defaultMaxFeedbackAge
	}
	if cfg.MaxFeedbackAge < 0 {
		return nil, errf("%w: MaxFeedbackAge %s must be non-negative", ErrBadConfig, cfg.MaxFeedbackAge)
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}

	initial := clampInt(cfg.InitialBitrate, cfg.MinBitrate, cfg.MaxBitrate)
	return &Controller{
		cfg:     cfg,
		target:  initial,
		desired: initial,
	}, nil
}

// Update ingests one NetworkFeedback sample and recomputes the desired target. It
// returns true when the sample was accepted and changed the desired target.
//
// Rejection, by design, never panics and never changes existing state:
//   - A non-finite (NaN/±Inf) metric, a loss outside [0,1], a negative delay or a
//     negative bandwidth is malformed and returns ErrBadFeedback.
//   - A sample whose Time is not strictly newer than the last accepted one is
//     out-of-order (or a duplicate) and is ignored, returning (false, nil).
//   - A sample older than Config.MaxFeedbackAge relative to the controller clock is
//     stale and is ignored the same way.
//   - A zero Time is stamped with the controller clock, so callers without a
//     shared clock can still drive the controller in arrival order.
//
// A zero AvailableBandwidth means "no estimate this sample", not a zero-capacity
// link: it removes the bandwidth ceiling rather than forcing the rate to the floor.
func (c *Controller) Update(fb NetworkFeedback) (bool, error) {
	if c.closed {
		return false, ErrClosed
	}
	if err := validateFeedback(fb); err != nil {
		return false, err
	}

	now := c.cfg.Clock()
	when := fb.Time
	explicitTime := !when.IsZero()
	if !explicitTime {
		// A zero Time means "this sample is current, in arrival order". The caller
		// has no shared clock and is driving the controller sequentially, so accept
		// it unconditionally and stamp it now rather than comparing timestamps: with
		// a clock that does not advance between calls every arrival-order sample
		// would otherwise share one timestamp and be dropped as a duplicate.
		when = now
	} else {
		// Explicit timestamps get the strict ordering and freshness guard.
		if c.haveLastFeedback && !when.After(c.lastFeedback) {
			return false, nil // out-of-order or duplicate
		}
		if age := now.Sub(when); age > c.cfg.MaxFeedbackAge {
			return false, nil // stale
		}
	}

	// EMA-smooth the metrics so one burst cannot dominate. The first accepted
	// sample seeds the estimates directly instead of averaging against zero.
	if !c.haveMetrics {
		c.smoothedLoss = fb.PacketLoss
		c.smoothedDelayS = fb.QueueDelay.Seconds()
		c.smoothedBW = fb.AvailableBandwidth
		c.haveMetrics = true
	} else {
		c.smoothedLoss = ema(c.smoothedLoss, fb.PacketLoss)
		c.smoothedDelayS = ema(c.smoothedDelayS, fb.QueueDelay.Seconds())
		// A "no estimate" bandwidth (0) must not drag a real estimate toward zero;
		// hold the prior value in that case.
		if fb.AvailableBandwidth > 0 {
			c.smoothedBW = ema(c.smoothedBW, fb.AvailableBandwidth)
		}
	}
	if fb.RTT > 0 {
		if !c.haveRTT {
			c.smoothedRTT = fb.RTT
			c.haveRTT = true
		} else {
			c.smoothedRTT = time.Duration(ema(float64(c.smoothedRTT), float64(fb.RTT)))
		}
	}

	c.desired = c.computeDesired()
	c.lastFeedback = when
	c.haveLastFeedback = true
	c.engaged = true
	return true, nil
}

// computeDesired maps the smoothed metrics to a bounded desired target relative to
// the current live target, applying the loss and delay back-offs, the bandwidth
// ceiling, healthy upward probing, and the per-update multiplicative change caps.
// All fractional policy is here; the result is rounded once to an integer bps.
func (c *Controller) computeDesired() int {
	cur := float64(c.target)

	// Loss back-off: no response at or below LossThreshold, ramping linearly to the
	// full decrease fraction at HighLossThreshold.
	lossFactor := 1.0
	if c.smoothedLoss > c.cfg.LossThreshold {
		t := (c.smoothedLoss - c.cfg.LossThreshold) / (c.cfg.HighLossThreshold - c.cfg.LossThreshold)
		t = clampFloat(t, 0, 1)
		lossFactor = 1 - c.cfg.MaxDecreaseFraction*t
	}

	// Delay back-off: tolerate up to TargetQueueDelay*QueueSafetyFactor, then cut
	// proportionally to how far the queue overshoots its target, never more than the
	// configured decrease fraction.
	delayFactor := 1.0
	targetS := c.cfg.TargetQueueDelay.Seconds()
	boundS := targetS * c.cfg.QueueSafetyFactor
	if c.smoothedDelayS > boundS {
		delayFactor = targetS / c.smoothedDelayS
		delayFactor = math.Max(delayFactor, 1-c.cfg.MaxDecreaseFraction)
	}

	healthy := lossFactor == 1 && delayFactor == 1
	var candidate float64
	if healthy {
		// AIMD probe: multiplicative increase while the path shows no pressure.
		candidate = cur * (1 + c.cfg.MaxIncreaseFraction)
	} else {
		candidate = cur * lossFactor * delayFactor
	}

	// Bandwidth ceiling. A zero/absent estimate imposes no cap.
	if c.smoothedBW > 0 {
		candidate = math.Min(candidate, c.smoothedBW)
	}

	// Per-update multiplicative change caps, then the hard configured bounds.
	lo := cur * (1 - c.cfg.MaxDecreaseFraction)
	hi := cur * (1 + c.cfg.MaxIncreaseFraction)
	candidate = clampFloat(candidate, lo, hi)
	candidate = clampFloat(candidate, float64(c.cfg.MinBitrate), float64(c.cfg.MaxBitrate))
	return int(math.Round(candidate))
}

// ApplyTo moves the live target toward the desired target by at most the
// configured per-second ramp for the elapsed wall-clock time, and applies the
// resulting target to enc via SetBitrate. It returns the target in effect after
// the call. It is meant to run once per encoded frame, between Encode calls.
//
// Before the first accepted feedback the controller is not engaged: ApplyTo does
// NOT call SetBitrate and returns InitialBitrate, so an encoder that never gets
// feedback keeps exactly the fixed bitrate it was constructed with. After Close it
// returns ErrClosed and leaves the encoder untouched. A SetBitrate error from the
// encoder is returned and the controller still advances its ramp timing, so a
// transiently rejected target does not freeze the trajectory.
func (c *Controller) ApplyTo(enc BitrateSetter) (int, error) {
	if c.closed {
		return c.target, ErrClosed
	}
	if !c.engaged {
		return c.target, nil
	}

	now := c.cfg.Clock()
	if c.lastApply.IsZero() {
		// First engaged application: adopt the initial target as the ramp origin and
		// apply it once so the encoder is pinned to a known value before ramping.
		c.lastApply = now
		c.target = clampInt(c.target, c.cfg.MinBitrate, c.cfg.MaxBitrate)
		if err := enc.SetBitrate(c.target); err != nil {
			return c.target, err
		}
		c.lastAppliedBps = c.target
		return c.target, nil
	}

	dt := now.Sub(c.lastApply).Seconds()
	c.lastApply = now
	if dt < 0 {
		dt = 0 // a non-monotonic injected clock must not produce a negative ramp
	}
	if c.target != c.desired {
		c.target = ramp(c.target, c.desired, dt, c.cfg.MaxIncreasePerSecond, c.cfg.MaxDecreasePerSecond)
	}
	if c.target == c.lastAppliedBps {
		return c.target, nil // already at this value; don't issue a redundant ctl
	}
	if err := enc.SetBitrate(c.target); err != nil {
		return c.target, err
	}
	c.lastAppliedBps = c.target
	return c.target, nil
}

// ramp moves current toward desired, never overshooting it, limiting the move to
// rateUp/rateDown bits-per-second multiplied by the elapsed seconds. A non-positive
// rate disables that direction's time cap (the per-update fraction still bounded
// desired in computeDesired).
func ramp(current, desired int, dt, rateUp, rateDown float64) int {
	switch {
	case desired > current:
		step := desired - current
		if rateUp > 0 {
			if capStep := int(math.Round(rateUp * dt)); capStep < step {
				step = capStep
			}
		}
		return current + step
	case desired < current:
		step := current - desired
		if rateDown > 0 {
			if capStep := int(math.Round(rateDown * dt)); capStep < step {
				step = capStep
			}
		}
		return current - step
	default:
		return current
	}
}

// Close marks the controller closed. Afterwards Update and ApplyTo return
// ErrClosed and never mutate an encoder; Close is idempotent. It does not close the
// encoder, whose lifecycle the caller owns.
func (c *Controller) Close() error {
	c.closed = true
	return nil
}

// TargetBitrate reports the live target most recently applied (or InitialBitrate
// before engagement). It is always within [MinBitrate, MaxBitrate].
func (c *Controller) TargetBitrate() int { return c.target }

// DesiredBitrate reports the target the latest accepted feedback converged on,
// which the live target ramps toward. Before the first feedback it equals
// InitialBitrate.
func (c *Controller) DesiredBitrate() int { return c.desired }

// Engaged reports whether at least one feedback sample has been accepted. While
// false, ApplyTo never touches the encoder.
func (c *Controller) Engaged() bool { return c.engaged }

// LastFeedbackTime reports the timestamp of the most recent accepted feedback, or
// the zero time when none has been accepted.
func (c *Controller) LastFeedbackTime() time.Time { return c.lastFeedback }

// --- small helpers -----------------------------------------------------------

func ema(prev, next float64) float64 { return prev + metricEMA*(next-prev) }

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func checkFraction(name string, v float64) error {
	if math.IsNaN(v) || v < 0 || v > 1 {
		return errf("%w: %s %v must be a finite fraction in [0,1]", ErrBadConfig, name, v)
	}
	return nil
}

// validateFeedback enforces finiteness and the hard domains before any value can
// touch controller state. Bandwidth==0 is explicitly allowed (means "no estimate").
func validateFeedback(fb NetworkFeedback) error {
	if math.IsNaN(fb.PacketLoss) || math.IsInf(fb.PacketLoss, 0) ||
		fb.PacketLoss < 0 || fb.PacketLoss > 1 {
		return errf("%w: PacketLoss %v must be a finite fraction in [0,1]", ErrBadFeedback, fb.PacketLoss)
	}
	// time.Duration is an int64 and so cannot be NaN/Inf; only the sign is
	// meaningful here.
	if fb.QueueDelay < 0 {
		return errf("%w: QueueDelay %s must be non-negative", ErrBadFeedback, fb.QueueDelay)
	}
	if math.IsNaN(fb.AvailableBandwidth) || math.IsInf(fb.AvailableBandwidth, 0) ||
		fb.AvailableBandwidth < 0 {
		return errf("%w: AvailableBandwidth %v must be finite and non-negative",
			ErrBadFeedback, fb.AvailableBandwidth)
	}
	// time.Duration is an int64 and so cannot carry NaN/Inf; an RTT <= 0 simply
	// means "no RTT this sample" and is handled as such by Update.
	return nil
}

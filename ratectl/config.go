package ratectl

import (
	"time"
)

// Default tuning constants. They are chosen for a conversational Opus stream that
// must react within a few feedback intervals to a handoff but must not flutter on
// a single noisy sample. Every one is overridable through Config; the defaults
// reproduce the behaviour the package tests pin.
const (
	// defaultMinBitrate is the lowest target the controller will ever request,
	// matching the floor opus_set_bitrate enforces for a mono stream. A stereo
	// caller that wants the codec's own floor raises it.
	defaultMinBitrate = 500
	// defaultMaxBitrate is the ceiling for a single stream (opus's
	// 750000*channels mono limit). Multistream/stereo callers raise it.
	defaultMaxBitrate = 750000
	// defaultMaxIncreaseFraction is the AIMD increase cap per feedback update: the
	// target may grow by at most 25% of itself in one step when the path looks
	// healthy.
	defaultMaxIncreaseFraction = 0.25
	// defaultMaxDecreaseFraction is the multiplicative-decrease cap per update: the
	// target falls by at most 50% in one step, so a single severe report cannot
	// collapse it straight to the floor.
	defaultMaxDecreaseFraction = 0.5
	// defaultMaxIncreasePerSecond / defaultMaxDecreasePerSecond bound the realised
	// ramp in bits-per-second per wall-clock second, applied in ApplyTo. They make
	// the on-wire trajectory independent of how frequently frames are produced.
	defaultMaxIncreasePerSecond = 100000.0
	defaultMaxDecreasePerSecond = 300000.0
	// defaultLossThreshold is the packet-loss fraction below which loss is treated
	// as noise (2%). Above it the controller backs off multiplicatively.
	defaultLossThreshold = 0.02
	// defaultHighLossThreshold is the fraction at/above which the response saturates
	// at MaxDecreaseFraction (25%).
	defaultHighLossThreshold = 0.25
	// defaultQueueSafetyFactor scales the configured target queueing delay into the
	// "still acceptable" bound: up to 1.5x target delay no delay-driven cut applies.
	defaultQueueSafetyFactor = 1.5
	// defaultMaxFeedbackAge is how long a feedback sample stays authoritative. A
	// sample older than this relative to the controller's clock is stale and
	// ignored rather than acted on.
	defaultMaxFeedbackAge = time.Second
	// defaultRttEstimate is used only when the very first feedback sample already
	// reports an absolute queueing delay but no RTT is available; it is replaced by
	// the running RTT estimate as soon as samples arrive.
	defaultRttEstimate = 200 * time.Millisecond
)

// Config configures a Controller. Every field has a documented zero-value default,
// so a literal setting only InitialBitrate (and optionally the bounds) is
// complete. Bitrates are integers in bits per second; the fractional fields and
// ramp speeds use float64 because they are control policy, not codec arithmetic.
type Config struct {
	// InitialBitrate is the target the controller holds (and ApplyTo enforces)
	// before the first usable feedback sample. It must be positive; New clamps it
	// into [MinBitrate, MaxBitrate]. Required: there is no automatic-rate default
	// because the controller's job is to own a concrete target.
	InitialBitrate int

	// MinBitrate and MaxBitrate bound every target the controller requests. Zero
	// values select defaultMinBitrate and defaultMaxBitrate. MinBitrate must be
	// positive and MinBitrate <= MaxBitrate. The encoder additionally clamps to its
	// own [500, 750000*channels] range, so for stereo set MaxBitrate to at most
	// 1500000.
	MinBitrate int
	MaxBitrate int

	// MaxIncreaseFraction and MaxDecreaseFraction cap the multiplicative change a
	// single feedback update may make, as a fraction of the current target in
	// [0,1]. Zero selects the defaults (0.25 up, 0.50 down). A decrease fraction of
	// 0 would freeze the downward response, which is almost never intended; use the
	// per-second ramp instead to slow a response.
	MaxIncreaseFraction float64
	MaxDecreaseFraction float64

	// MaxIncreasePerSecond and MaxDecreasePerSecond bound the realised ramp in
	// bits-per-second per wall-clock second, smoothing over the frame cadence. Zero
	// selects the defaults. A value <= 0 after defaulting disables the
	// corresponding time-based cap (the per-update fraction still applies).
	MaxIncreasePerSecond float64
	MaxDecreasePerSecond float64

	// TargetQueueDelay is the one-way queueing delay the controller tries to keep
	// the path at or below. A measured delay up to TargetQueueDelay*QueueSafetyFactor
	// causes no delay-driven reduction; above it the target is cut proportionally.
	// Zero selects 100ms, a typical conversational budget.
	TargetQueueDelay time.Duration
	// QueueSafetyFactor multiplies TargetQueueDelay for the no-action bound; zero
	// selects defaultQueueSafetyFactor. It must be >= 1 when set.
	QueueSafetyFactor float64

	// LossThreshold and HighLossThreshold are the packet-loss fractions (in [0,1])
	// at which the multiplicative back-off starts and saturates, respectively. Zero
	// selects defaultLossThreshold (0.02) and defaultHighLossThreshold (0.25).
	LossThreshold     float64
	HighLossThreshold float64

	// MaxFeedbackAge is the maximum age, relative to the controller clock, at which
	// a feedback sample is still authoritative. An older sample is treated as stale
	// and ignored. Zero selects defaultMaxFeedbackAge.
	MaxFeedbackAge time.Duration

	// Clock supplies the current time. It is an injection point for deterministic
	// tests; nil uses time.Now. Production code leaves it nil. A custom Clock must be
	// monotonic for the per-second ramp to be meaningful.
	Clock func() time.Time
}

// NetworkFeedback is one sample of transport-side network conditions, the input to
// Controller.Update. All fields describe conditions observed at Time: the
// controller treats an older Time than the last accepted sample as out-of-order
// and ignores it.
type NetworkFeedback struct {
	// Time is when these conditions were observed. The zero value is accepted and
	// means "now, in arrival order": the controller stamps it with its own clock and
	// always accepts it in call sequence (so a caller without a shared clock can
	// pump samples even when its clock does not advance between calls). Only samples
	// carrying an explicit, non-zero Time are checked for staleness and ordering.
	Time time.Time

	// PacketLoss is the observed packet-loss rate as a fraction in [0,1] (0.05 is
	// 5%). Values outside [0,1], NaN and Inf are rejected.
	PacketLoss float64

	// QueueDelay is the measured one-way queueing (buffering) delay above the
	// path's propagation baseline, never negative. A negative, NaN or Inf value is
	// rejected. Callers that only have an RTT may pass rtt - baselineRtt.
	QueueDelay time.Duration

	// AvailableBandwidth is the transport's estimate of currently available
	// capacity in bits per second, or 0 if the transport provides none. A zero
	// value means "no estimate" and simply removes the bandwidth ceiling for this
	// sample; it is NOT treated as a zero-capacity link. A negative, NaN or Inf
	// value is rejected.
	AvailableBandwidth float64

	// RTT is the measured round-trip time, used to refine the queue-delay baseline.
	// It is optional; a zero or negative value means "no RTT this sample" and the
	// controller keeps its prior estimate. NaN or Inf are rejected.
	RTT time.Duration
}

// BitrateSetter is the sink the controller applies its target to. *opus.Encoder,
// *opus.MultistreamEncoder and *oggopus.Encoder all satisfy it. The implementation
// must change the encoder's target without rebuilding it or resetting codec
// state, so the stream stays continuously decodable.
type BitrateSetter interface {
	SetBitrate(bps int) error
}

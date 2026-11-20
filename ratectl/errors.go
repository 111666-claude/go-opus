package ratectl

import "errors"

// Sentinel errors returned by the controller, testable with errors.Is. They are
// deliberately distinct: a bad Config or a malformed feedback sample are caller
// programming/measurement errors, while ErrClosed is a lifecycle verdict.
var (
	// ErrBadConfig reports a Config field outside its domain (a zero or negative
	// bitrate where a positive value is required, inverted bounds, or a fraction
	// outside [0,1]). New returns it; no controller is created.
	ErrBadConfig = errors.New("ratectl: bad configuration")
	// ErrBadFeedback reports a NetworkFeedback sample whose metrics are not usable:
	// a non-finite (NaN/±Inf) value, a loss fraction outside [0,1], a negative
	// queueing delay or a negative bandwidth estimate. The sample is rejected and
	// the controller's state is left unchanged.
	ErrBadFeedback = errors.New("ratectl: bad network feedback")
	// ErrClosed reports an Update or ApplyTo call on a Controller after Close. The
	// call is a no-op and never mutates the encoder.
	ErrClosed = errors.New("ratectl: controller is closed")
)

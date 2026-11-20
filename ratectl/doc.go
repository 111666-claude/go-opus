// Package ratectl adds real-time, feedback-driven bitrate adaptation on top of a
// go-opus encoder, for the live-calling case where a stream moves between networks
// (Wi-Fi to cellular) and a fixed bitrate either congests and distorts or starves
// an idle link. It is a CONTROL-PLANE package: it never touches a PCM sample or a
// packet byte. It consumes coarse network feedback (packet loss, queueing delay,
// an available-bandwidth estimate), decides a target bitrate under configured
// bounds and change-rate limits, and pushes that target into an encoder through
// the small BitrateSetter interface. The encoder then changes only the rate of its
// next packet; cross-frame codec state is untouched, so the stream stays
// continuously decodable and an Ogg container's page sequence and sample timeline
// never jump (see oggopus.Encoder.SetBitrate).
//
// # Why a separate package
//
// The codec packages (opus, internal/...) are fixed-point and kept bit-exact with
// libopus; adaptation policy is neither fixed-point nor part of RFC 6716, so it
// does not belong in them. Keeping the control law here means the codec contract
// is unchanged and the adaptation is independently testable against scripted
// feedback without encoding a frame. The codec path still contains no floating
// point; the controller uses float64 only to combine its fractional inputs and
// rounds once, to an integer bits-per-second target it hands to the encoder.
//
// # Model
//
// A Controller is configured once (bounds, ramp speed, loss/delay sensitivity) and
// then driven by two calls: Update ingests one NetworkFeedback sample as it
// arrives from RTCP/transport feedback, and ApplyTo (typically once per encoded
// frame) moves the live target toward the most recent decision by at most the
// configured per-second ramp and applies it to an encoder. With no feedback the
// controller never touches the encoder, so a stream that receives no adaptation
// input keeps exactly its configured fixed bitrate. Stale or out-of-order feedback
// is ignored, non-finite and out-of-range metrics are rejected before they can
// corrupt the state, and a closed controller reports ErrClosed rather than
// mutating anything.
package ratectl

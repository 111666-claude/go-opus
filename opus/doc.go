// Package opus is the public API for the go-opus native Go Opus codec: a raw,
// buffer-based Encoder and Decoder (one PCM frame in, one packet out, and the
// reverse) plus packet-inspection helpers. It is the layer the differential
// conformance gates test.
//
// The encoder is a fixed-point CELT-only encoder (8, 12, 16, 24 and 48 kHz, mono
// or stereo); CELT is the only mode currently planned, so SILK-only and hybrid
// encoding are not implemented. The decoder is the full RFC 6716 decoder and
// handles all three modes. The container layer lives in the sibling oggopus
// package.
//
// Encoder.SetBitrate changes the target rate between frames without rebuilding the
// encoder or resetting codec state, so a live stream can adapt to network
// conditions while staying continuously decodable; the ratectl package provides
// the feedback-driven control law on top of it.
package opus

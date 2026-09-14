package protocol

import "testing"

func TestSSEDecoderDefaultsAndDirectConstruction(t *testing.T) {
	t.Parallel()
	limits := DefaultSSELimits()
	if limits.MaxEventBytes <= limits.Stream.MaxFrameBytes {
		t.Fatalf("MaxEventBytes = %d, MaxFrameBytes = %d", limits.MaxEventBytes, limits.Stream.MaxFrameBytes)
	}
	decoder := NewSSEDecoder(limits)
	frames, err := decoder.Feed([]byte(": keepalive\n\n"))
	if err != nil || len(frames) != 0 {
		t.Fatalf("comment feed = %#v, %v", frames, err)
	}
}

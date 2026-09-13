package http_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/valksor/naatre/protocol"
	transporthttp "github.com/valksor/naatre/transport/http"
)

func TestSSEDecoderHandlesEveryChunkBoundaryAndSplitUTF8(t *testing.T) {
	t.Parallel()
	limits := transporthttp.DefaultSSELimits()
	frame := protocol.StreamFrame{
		Type: protocol.StreamData, Stream: "stream-safe", Sequence: 2,
		Position: 1, HasPosition: true, EventID: "application-event", Cursor: "opaque-cursor",
		Data: []byte(`{"text":"tēriņš"}`),
	}
	encoded, err := transporthttp.EncodeSSE(frame, limits)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte("event: naatre.data\n")) || !bytes.Contains(encoded, []byte("id: opaque-cursor\n")) || bytes.Contains(encoded, []byte("event: application-event")) {
		t.Fatalf("SSE envelope = %q", encoded)
	}
	decoder := transporthttp.NewSSEDecoder(limits)
	var decoded []protocol.StreamFrame
	for _, octet := range encoded {
		frames, err := decoder.Feed([]byte{octet})
		if err != nil {
			t.Fatal(err)
		}
		decoded = append(decoded, frames...)
	}
	if err := decoder.Finish(); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || decoded[0].Stream != frame.Stream || decoded[0].Cursor != frame.Cursor || string(decoded[0].Data) != string(frame.Data) {
		t.Fatalf("decoded SSE frames = %#v", decoded)
	}
}

func TestSSEDecoderSupportsCommentsMultilineDataAndCRLF(t *testing.T) {
	t.Parallel()
	input := []byte(": transport heartbeat\r\nevent: naatre.data\r\nid: replay-cursor\r\ndata: {\"type\":\"data\",\r\ndata: \"stream\":\"s\",\"sequence\":2,\"position\":1,\"cursor\":\"replay-cursor\",\"data\":true}\r\n\r\n")
	decoder := transporthttp.NewSSEDecoder(transporthttp.DefaultSSELimits())
	frames, err := decoder.Feed(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || frames[0].Position != 1 || string(frames[0].Data) != "true" {
		t.Fatalf("multiline frames = %#v", frames)
	}
	if err := decoder.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestSSEDecoderAcceptsCRDelimitersAndBoundsCommentsPerEvent(t *testing.T) {
	t.Parallel()
	limits := transporthttp.DefaultSSELimits()
	limits.MaxEventBytes = 96
	decoder := transporthttp.NewSSEDecoder(limits)
	input := []byte(": 0123456789012345678901234567890123456789\r\r: 0123456789012345678901234567890123456789\r\r")
	frames, err := decoder.Feed(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 0 {
		t.Fatalf("comment frames = %#v", frames)
	}
	if err := decoder.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestSSEDecoderRejectsControlInjectionMismatchAndTruncation(t *testing.T) {
	t.Parallel()
	limits := transporthttp.DefaultSSELimits()
	invalid := [][]byte{
		[]byte("event: naatre.complete\ndata: {\"type\":\"data\",\"stream\":\"s\",\"sequence\":1,\"data\":true}\n\n"),
		[]byte("event: naatre.data\nevent: naatre.complete\ndata: {}\n\n"),
		[]byte("event: naatre.data\nid: bad\x00cursor\ndata: {}\n\n"),
		[]byte("event: forged\ndata: {}\n\n"),
	}
	for _, input := range invalid {
		decoder := transporthttp.NewSSEDecoder(limits)
		if _, err := decoder.Feed(input); err == nil {
			t.Fatalf("invalid SSE accepted: %q", input)
		}
	}
	decoder := transporthttp.NewSSEDecoder(limits)
	if _, err := decoder.Feed([]byte("event: naatre.data\ndata: {")); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Finish(); !errors.Is(err, transporthttp.ErrSSETruncated) {
		t.Fatalf("truncated SSE error = %v", err)
	}
}

func TestSSEDecoderBoundsBufferedEvent(t *testing.T) {
	t.Parallel()
	limits := transporthttp.DefaultSSELimits()
	limits.MaxEventBytes = 32
	decoder := transporthttp.NewSSEDecoder(limits)
	if _, err := decoder.Feed([]byte("data: 0123456789012345678901234567890123456789")); !errors.Is(err, transporthttp.ErrSSELimit) {
		t.Fatalf("SSE limit error = %v", err)
	}
}

func TestSSEDecoderReturnsCompletedFramesBeforeLaterChunkError(t *testing.T) {
	t.Parallel()
	valid := "event: naatre.complete\ndata: {\"type\":\"complete\",\"stream\":\"s\",\"sequence\":1}\n\n"
	invalid := "event: naatre.data\ndata: {}\n\n"
	decoder := transporthttp.NewSSEDecoder(transporthttp.DefaultSSELimits())
	frames, err := decoder.Feed([]byte(valid + invalid))
	if !errors.Is(err, transporthttp.ErrInvalidSSE) || len(frames) != 1 || frames[0].Type != protocol.StreamComplete {
		t.Fatalf("frames/error = %#v/%v", frames, err)
	}
}

func TestSSEDecoderRejectsInvalidUTF8Comments(t *testing.T) {
	t.Parallel()
	decoder := transporthttp.NewSSEDecoder(transporthttp.DefaultSSELimits())
	if _, err := decoder.Feed([]byte{':', ' ', 0xff, '\n', '\n'}); !errors.Is(err, transporthttp.ErrInvalidSSE) {
		t.Fatalf("invalid UTF-8 comment error = %v", err)
	}
}

func TestSSEDecoderRetainsPartiallyConfiguredStreamLimits(t *testing.T) {
	t.Parallel()
	limits := transporthttp.SSELimits{Stream: protocol.StreamLimits{MaxDataBytes: 4}}
	decoder := transporthttp.NewSSEDecoder(limits)
	input := []byte("event: naatre.data\ndata: {\"type\":\"data\",\"stream\":\"s\",\"sequence\":1,\"data\":false}\n\n")
	if _, err := decoder.Feed(input); !errors.Is(err, transporthttp.ErrInvalidSSE) {
		t.Fatalf("partial nested stream limit error = %v", err)
	}
}

func TestSSEDefaultEventLimitCoversResolvedFrameAndDuplicatedCursor(t *testing.T) {
	t.Parallel()
	stream := protocol.DefaultStreamLimits()
	stream.MaxFrameBytes = 4096
	stream.MaxDataBytes = 3500
	stream.MaxIdentifierBytes = 512
	frame := protocol.StreamFrame{
		Type: protocol.StreamData, Stream: "s", Sequence: 2, Position: 1, HasPosition: true,
		Cursor: strings.Repeat("c", stream.MaxIdentifierBytes*4),
	}
	low, high := 0, stream.MaxDataBytes-2
	for low < high {
		mid := (low + high + 1) / 2
		frame.Data = []byte(`"` + strings.Repeat("x", mid) + `"`)
		if _, err := protocol.MarshalStreamFrame(frame, stream); err == nil {
			low = mid
		} else {
			high = mid - 1
		}
	}
	frame.Data = []byte(`"` + strings.Repeat("x", low) + `"`)
	encoded, err := transporthttp.EncodeSSE(frame, transporthttp.SSELimits{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) <= stream.MaxFrameBytes+2048 {
		t.Fatalf("boundary event length = %d, expected to exceed old cap %d", len(encoded), stream.MaxFrameBytes+2048)
	}
	decoder := transporthttp.NewSSEDecoder(transporthttp.SSELimits{Stream: stream})
	frames, err := decoder.Feed(encoded)
	if err != nil || len(frames) != 1 {
		t.Fatalf("boundary decode = %#v, %v", frames, err)
	}
}

func TestSSECommentOnlyEOFIsNotALogicalTruncation(t *testing.T) {
	t.Parallel()
	decoder := transporthttp.NewSSEDecoder(transporthttp.DefaultSSELimits())
	if _, err := decoder.Feed([]byte(": heartbeat\n")); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Finish(); err != nil {
		t.Fatalf("complete comment EOF = %v", err)
	}
	partial := transporthttp.NewSSEDecoder(transporthttp.DefaultSSELimits())
	if _, err := partial.Feed([]byte(": heartbeat")); err != nil {
		t.Fatal(err)
	}
	if err := partial.Finish(); !errors.Is(err, transporthttp.ErrSSETruncated) {
		t.Fatalf("partial comment EOF = %v", err)
	}
}

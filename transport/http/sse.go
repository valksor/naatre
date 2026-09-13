package http

import (
	"bytes"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/valksor/naatre/protocol"
)

var (
	// ErrSSETruncated reports an EOF before the blank line terminating an event.
	ErrSSETruncated = errors.New("truncated SSE event")
	// ErrSSELimit reports that one buffered SSE event exceeded its byte budget.
	ErrSSELimit = errors.New("SSE event exceeds limit")
	// ErrInvalidSSE reports malformed or inconsistent SSE control fields.
	ErrInvalidSSE = errors.New("invalid SSE event")
)

// SSELimits bounds transport buffering and the logical frame nested in it.
// MaxEventBytes counts buffered SSE octets with CRLF treated as one line
// delimiter, matching the decoder's normalization before any allocation.
type SSELimits struct {
	Stream        protocol.StreamLimits
	MaxEventBytes int
}

// DefaultSSELimits returns conservative limits suitable for untrusted peers.
func DefaultSSELimits() SSELimits {
	stream := protocol.DefaultStreamLimits()
	return SSELimits{Stream: stream, MaxEventBytes: defaultSSEEventBytes(stream)}
}

// EncodeSSE encodes one logical frame as one self-contained SSE event.
func EncodeSSE(frame protocol.StreamFrame, limits SSELimits) ([]byte, error) {
	limits = resolveSSELimits(limits)
	payload, err := protocol.MarshalStreamFrame(frame, limits.Stream)
	if err != nil {
		return nil, err
	}

	var encoded bytes.Buffer
	fmt.Fprintf(&encoded, "event: naatre.%s\n", frame.Type)
	if frame.Cursor != "" {
		fmt.Fprintf(&encoded, "id: %s\n", frame.Cursor)
	}
	encoded.WriteString("data: ")
	encoded.Write(payload)
	encoded.WriteString("\n\n")
	if encoded.Len() > limits.MaxEventBytes {
		return nil, ErrSSELimit
	}
	return encoded.Bytes(), nil
}

// SSEDecoder incrementally decodes an SSE byte stream into logical frames.
// It is not safe for concurrent use.
type SSEDecoder struct {
	limits     SSELimits
	line       []byte
	eventBytes int
	event      string
	id         string
	data       [][]byte
	haveEvent  bool
	haveID     bool
	haveData   bool
	skipLF     bool
	err        error
}

// NewSSEDecoder creates a bounded incremental decoder.
func NewSSEDecoder(limits SSELimits) *SSEDecoder {
	return &SSEDecoder{limits: resolveSSELimits(limits)}
}

// Feed consumes an arbitrary transport chunk. Complete events are returned in
// wire order; incomplete UTF-8 and lines remain buffered for the next call.
func (d *SSEDecoder) Feed(chunk []byte) ([]protocol.StreamFrame, error) {
	if d.err != nil {
		return nil, d.err
	}
	var frames []protocol.StreamFrame
	for _, octet := range chunk {
		if d.skipLF && octet == '\n' {
			d.skipLF = false
			continue
		}
		d.skipLF = false
		d.eventBytes++
		if d.eventBytes > d.limits.MaxEventBytes {
			d.err = ErrSSELimit
			return frames, d.err
		}
		if octet != '\n' && octet != '\r' {
			d.line = append(d.line, octet)
			continue
		}
		line := d.line
		if octet == '\r' {
			d.skipLF = true
		}
		d.line = d.line[:0]
		frame, complete, err := d.consumeLine(line)
		if err != nil {
			d.err = err
			return frames, err
		}
		if complete {
			frames = append(frames, frame)
		}
		if len(line) == 0 {
			d.eventBytes = 0
		}
	}
	return frames, nil
}

// Finish validates that EOF occurred between events rather than within one.
func (d *SSEDecoder) Finish() error {
	if d.err != nil {
		return d.err
	}
	if len(d.line) != 0 || d.haveEvent || d.haveID || d.haveData {
		return ErrSSETruncated
	}
	return nil
}

func (d *SSEDecoder) consumeLine(line []byte) (protocol.StreamFrame, bool, error) {
	if !utf8.Valid(line) {
		return protocol.StreamFrame{}, false, ErrInvalidSSE
	}
	if len(line) == 0 {
		if !d.haveEvent && !d.haveID && !d.haveData {
			return protocol.StreamFrame{}, false, nil
		}
		return d.finishEvent()
	}
	if line[0] == ':' {
		return protocol.StreamFrame{}, false, nil
	}

	field, value, found := bytes.Cut(line, []byte{':'})
	if !found {
		return protocol.StreamFrame{}, false, ErrInvalidSSE
	}
	if len(value) != 0 && value[0] == ' ' {
		value = value[1:]
	}
	switch string(field) {
	case "event":
		if d.haveEvent || len(value) == 0 || bytes.IndexByte(value, 0) >= 0 {
			return protocol.StreamFrame{}, false, ErrInvalidSSE
		}
		d.event, d.haveEvent = string(value), true
	case "id":
		if d.haveID || bytes.IndexByte(value, 0) >= 0 {
			return protocol.StreamFrame{}, false, ErrInvalidSSE
		}
		d.id, d.haveID = string(value), true
	case "data":
		d.data = append(d.data, append([]byte(nil), value...))
		d.haveData = true
	default:
		return protocol.StreamFrame{}, false, ErrInvalidSSE
	}
	return protocol.StreamFrame{}, false, nil
}

func (d *SSEDecoder) finishEvent() (protocol.StreamFrame, bool, error) {
	if !d.haveEvent || !d.haveData {
		return protocol.StreamFrame{}, false, ErrInvalidSSE
	}
	prefix := "naatre."
	if len(d.event) <= len(prefix) || d.event[:len(prefix)] != prefix {
		return protocol.StreamFrame{}, false, ErrInvalidSSE
	}
	payload := bytes.Join(d.data, []byte{'\n'})
	frame, err := protocol.DecodeStreamFrame(payload, d.limits.Stream)
	if err != nil || d.event != prefix+string(frame.Type) || d.id != frame.Cursor || d.haveID != (frame.Cursor != "") {
		return protocol.StreamFrame{}, false, ErrInvalidSSE
	}
	d.resetEvent()
	return frame, true, nil
}

func (d *SSEDecoder) resetEvent() {
	d.event = ""
	d.id = ""
	d.data = d.data[:0]
	d.haveEvent = false
	d.haveID = false
	d.haveData = false
}

func resolveSSELimits(limits SSELimits) SSELimits {
	limits.Stream = protocol.ResolveStreamLimits(limits.Stream)
	if limits.MaxEventBytes <= 0 {
		limits.MaxEventBytes = defaultSSEEventBytes(limits.Stream)
	}
	return limits
}

func defaultSSEEventBytes(stream protocol.StreamLimits) int {
	const fixed = len("event: naatre.") + len("history-unavailable") + len("\nid: ") + len("\ndata: ") + len("\n\n")
	maximum := int(^uint(0) >> 1)
	if stream.MaxFrameBytes > maximum-fixed {
		return maximum
	}
	if stream.MaxIdentifierBytes > (maximum-fixed-stream.MaxFrameBytes)/4 {
		return maximum
	}
	return stream.MaxFrameBytes + stream.MaxIdentifierBytes*4 + fixed
}

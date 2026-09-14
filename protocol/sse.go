package protocol

import (
	"bytes"
	"errors"
	"unicode/utf8"
)

var (
	ErrSSETruncated = errors.New("truncated SSE event")
	ErrSSELimit     = errors.New("SSE event exceeds limit")
	ErrInvalidSSE   = errors.New("invalid SSE event")
)

// SSELimits bounds transport buffering and the logical frame nested in it.
type SSELimits struct {
	Stream        StreamLimits
	MaxEventBytes int
}

func DefaultSSELimits() SSELimits {
	stream := DefaultStreamLimits()
	return SSELimits{Stream: stream, MaxEventBytes: defaultSSEEventBytes(stream)}
}

func ResolveSSELimits(limits SSELimits) SSELimits {
	limits.Stream = ResolveStreamLimits(limits.Stream)
	if limits.MaxEventBytes <= 0 {
		limits.MaxEventBytes = defaultSSEEventBytes(limits.Stream)
	}
	return limits
}

// SSEDecoder incrementally decodes the normative SSE binding into logical
// stream frames. It is not safe for concurrent use.
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

func NewSSEDecoder(limits SSELimits) *SSEDecoder {
	return &SSEDecoder{limits: ResolveSSELimits(limits)}
}

func (d *SSEDecoder) Feed(chunk []byte) ([]StreamFrame, error) {
	if d.err != nil {
		return nil, d.err
	}
	var frames []StreamFrame
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

func (d *SSEDecoder) Finish() error {
	if d.err != nil {
		return d.err
	}
	if len(d.line) != 0 || d.haveEvent || d.haveID || d.haveData {
		return ErrSSETruncated
	}
	return nil
}

func (d *SSEDecoder) consumeLine(line []byte) (StreamFrame, bool, error) {
	if !utf8.Valid(line) {
		return StreamFrame{}, false, ErrInvalidSSE
	}
	if len(line) == 0 {
		if !d.haveEvent && !d.haveID && !d.haveData {
			return StreamFrame{}, false, nil
		}
		return d.finishEvent()
	}
	if line[0] == ':' {
		return StreamFrame{}, false, nil
	}
	field, value, found := bytes.Cut(line, []byte{':'})
	if !found {
		return StreamFrame{}, false, ErrInvalidSSE
	}
	if len(value) != 0 && value[0] == ' ' {
		value = value[1:]
	}
	switch string(field) {
	case "event":
		if d.haveEvent || len(value) == 0 || bytes.IndexByte(value, 0) >= 0 {
			return StreamFrame{}, false, ErrInvalidSSE
		}
		d.event, d.haveEvent = string(value), true
	case "id":
		if d.haveID || bytes.IndexByte(value, 0) >= 0 {
			return StreamFrame{}, false, ErrInvalidSSE
		}
		d.id, d.haveID = string(value), true
	case "data":
		d.data = append(d.data, bytes.Clone(value))
		d.haveData = true
	default:
		return StreamFrame{}, false, ErrInvalidSSE
	}
	return StreamFrame{}, false, nil
}

func (d *SSEDecoder) finishEvent() (StreamFrame, bool, error) {
	if !d.haveEvent || !d.haveData {
		return StreamFrame{}, false, ErrInvalidSSE
	}
	const prefix = "naatre."
	if len(d.event) <= len(prefix) || d.event[:len(prefix)] != prefix {
		return StreamFrame{}, false, ErrInvalidSSE
	}
	payload := bytes.Join(d.data, []byte{'\n'})
	frame, err := DecodeStreamFrame(payload, d.limits.Stream)
	if err != nil || d.event != prefix+string(frame.Type) || d.id != frame.Cursor || d.haveID != (frame.Cursor != "") {
		return StreamFrame{}, false, ErrInvalidSSE
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

func defaultSSEEventBytes(stream StreamLimits) int {
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

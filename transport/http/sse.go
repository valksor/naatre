package http

import (
	"bytes"
	"fmt"

	"github.com/valksor/naatre/protocol"
)

var (
	ErrSSETruncated = protocol.ErrSSETruncated
	ErrSSELimit     = protocol.ErrSSELimit
	ErrInvalidSSE   = protocol.ErrInvalidSSE
)

type SSELimits = protocol.SSELimits
type SSEDecoder = protocol.SSEDecoder

func DefaultSSELimits() SSELimits { return protocol.DefaultSSELimits() }

func NewSSEDecoder(limits SSELimits) *SSEDecoder { return protocol.NewSSEDecoder(limits) }

// EncodeSSE encodes one logical frame as one self-contained SSE event.
func EncodeSSE(frame protocol.StreamFrame, limits SSELimits) ([]byte, error) {
	limits = protocol.ResolveSSELimits(limits)
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

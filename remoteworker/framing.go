package remoteworker

import (
	"encoding/binary"
	"errors"
	"io"
)

const (
	FrameData        byte = 0
	FrameEnd         byte = 1
	frameHeaderBytes      = 5
)

var (
	ErrFrameLimit = errors.New("remote-worker frame exceeds configured limit")
	ErrFrameFlags = errors.New("remote-worker frame uses unsupported flags")
)

type Frame struct {
	Flags   byte
	Payload []byte
}

func WriteFrame(writer io.Writer, flags byte, payload []byte, maximum uint32) error {
	if writer == nil || (flags != FrameData && flags != FrameEnd) {
		return ErrFrameFlags
	}
	if len(payload) == 0 || uint64(len(payload)) > uint64(maximum) {
		return ErrFrameLimit
	}
	var header [frameHeaderBytes]byte
	header[0] = flags
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if err := writeFramePart(writer, header[:]); err != nil {
		return err
	}
	return writeFramePart(writer, payload)
}

func writeFramePart(writer io.Writer, value []byte) error {
	for len(value) != 0 {
		written, err := writer.Write(value)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(value) {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}

func ReadFrame(reader io.Reader, maximum uint32) (Frame, error) {
	if reader == nil {
		return Frame{}, io.ErrUnexpectedEOF
	}
	var header [frameHeaderBytes]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return Frame{}, err
	}
	if header[0] != FrameData && header[0] != FrameEnd {
		return Frame{}, ErrFrameFlags
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size == 0 || size > maximum {
		return Frame{}, ErrFrameLimit
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return Frame{}, err
	}
	return Frame{Flags: header[0], Payload: payload}, nil
}

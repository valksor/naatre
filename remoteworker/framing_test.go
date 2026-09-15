package remoteworker

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type shortWriter struct {
	buffer bytes.Buffer
	limit  int
}

func (w *shortWriter) Write(value []byte) (int, error) {
	if w.limit == 0 {
		return 0, nil
	}
	return w.buffer.Write(value[:min(len(value), w.limit)])
}

func TestLengthDelimitedFrameRoundTrip(t *testing.T) {
	t.Parallel()
	var buffer bytes.Buffer
	payload := []byte(`{"protocol":"naatre.remote-worker.v1","kind":"register"}`)
	if err := WriteFrame(&buffer, FrameData, payload, 1024); err != nil {
		t.Fatal(err)
	}
	frame, err := ReadFrame(&buffer, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Flags != FrameData || !bytes.Equal(frame.Payload, payload) {
		t.Fatalf("frame = %#v", frame)
	}
}

func TestLengthDelimitedFrameLimits(t *testing.T) {
	t.Parallel()
	var buffer bytes.Buffer
	if err := WriteFrame(&buffer, FrameData, []byte("oversized"), 4); !errors.Is(err, ErrFrameLimit) {
		t.Fatalf("WriteFrame = %v", err)
	}
	buffer.Write([]byte{0, 0, 0, 0, 9})
	if _, err := ReadFrame(&buffer, 4); !errors.Is(err, ErrFrameLimit) {
		t.Fatalf("ReadFrame = %v", err)
	}
}

func TestLengthDelimitedFrameHandlesShortWrites(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"kind":"invoke"}`)
	w := &shortWriter{limit: 2}
	if err := WriteFrame(w, FrameData, payload, 1024); err != nil {
		t.Fatal(err)
	}
	frame, err := ReadFrame(&w.buffer, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frame.Payload, payload) {
		t.Fatalf("payload = %q", frame.Payload)
	}

	if err := WriteFrame(&shortWriter{}, FrameData, payload, 1024); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("WriteFrame zero write = %v", err)
	}
}

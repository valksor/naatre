package remoteworker

import (
	"errors"
	"sync"
)

type CreditWindow struct {
	mu     sync.Mutex
	frames uint64
	bytes  uint64
}

func NewCreditWindow(frames, bytes uint64) *CreditWindow {
	return &CreditWindow{frames: frames, bytes: bytes}
}

func (w *CreditWindow) Grant(frames, bytes uint64) error {
	if w == nil || frames == 0 || bytes == 0 {
		return errors.New("remote stream credit must be positive")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if ^uint64(0)-w.frames < frames || ^uint64(0)-w.bytes < bytes {
		return errors.New("remote stream credit overflow")
	}
	w.frames += frames
	w.bytes += bytes
	return nil
}

func (w *CreditWindow) Consume(bytes uint64) error {
	if w == nil || bytes == 0 {
		return ErrBackpressure
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.frames == 0 || w.bytes < bytes {
		return ErrBackpressure
	}
	w.frames--
	w.bytes -= bytes
	return nil
}

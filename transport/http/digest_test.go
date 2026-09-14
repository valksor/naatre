package http_test

import (
	"bytes"
	"errors"
	"io"
	"math"
	"strings"
	"testing"

	transporthttp "github.com/valksor/naatre/transport/http"
)

func TestDigestFieldRoundTripAndStrictRejections(t *testing.T) {
	t.Parallel()
	content := []byte("Wikipedia")
	field, err := transporthttp.FormatDigestField(content, transporthttp.SHA512, transporthttp.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	want := "sha-512=:2D2LbhjDhDShE3dawYeaOnOCIrHdCwa9e3tuALmbaaUyzX8G9EYh5iBH6f9xB+ZVvCknlZo6muP7K9R+B+hjwA==:, sha-256=:04s4ot1HbgRcKZ6O5dZGaDRFbZe9WSpxdGtCOmoF84Y=:"
	if field != want {
		t.Fatalf("digest field = %q, want %q", field, want)
	}
	if _, err := transporthttp.ParseDigestFields([]string{field}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		fields []string
		match  error
	}{
		{name: "missing", match: transporthttp.ErrDigestRequired},
		{name: "field lines", fields: []string{field, field}, match: transporthttp.ErrDuplicateDigestField},
		{name: "unknown", fields: []string{"md5=:AA==:"}, match: transporthttp.ErrUnsupportedDigestAlgorithm},
		{name: "duplicate member", fields: []string{"sha-256=:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=:, sha-256=:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=:"}, match: transporthttp.ErrDuplicateDigestAlgorithm},
		{name: "wrong type", fields: []string{"sha-256=\"value\""}, match: transporthttp.ErrMalformedDigest},
		{name: "parameters", fields: []string{"sha-256=:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=:;x=1"}, match: transporthttp.ErrMalformedDigest},
		{name: "wrong length", fields: []string{"sha-256=:AA==:"}, match: transporthttp.ErrMalformedDigest},
		{name: "oversized field", fields: []string{strings.Repeat("x", transporthttp.MaximumDigestFieldBytes+1)}, match: transporthttp.ErrMalformedDigest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := transporthttp.ParseDigestFields(test.fields); !errors.Is(err, test.match) {
				t.Fatalf("ParseDigestFields error = %v, want %v", err, test.match)
			}
		})
	}
}

func TestWantDigestNegotiation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		fields []string
		want   transporthttp.DigestAlgorithm
		match  error
	}{
		{name: "default", want: transporthttp.SHA256},
		{name: "stronger weight", fields: []string{"sha-256=5, sha-512=10"}, want: transporthttp.SHA512},
		{name: "equal server preference", fields: []string{"sha-256=7, sha-512=7"}, want: transporthttp.SHA512},
		{name: "all refused", fields: []string{"sha-256=0, sha-512=0"}, match: transporthttp.ErrNoAcceptableDigest},
		{name: "unknown", fields: []string{"md5=9, sha-256=1"}, match: transporthttp.ErrUnsupportedDigestAlgorithm},
		{name: "invalid weight", fields: []string{"sha-256=11"}, match: transporthttp.ErrMalformedDigest},
		{name: "duplicate lines", fields: []string{"sha-256=5", "sha-512=9"}, match: transporthttp.ErrDuplicateDigestField},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual, err := transporthttp.NegotiateDigestAlgorithm(test.fields)
			if !errors.Is(err, test.match) || actual != test.want {
				t.Fatalf("NegotiateDigestAlgorithm = %q, %v; want %q, %v", actual, err, test.want, test.match)
			}
		})
	}
}

func TestVerifyDigestToIsBoundedAndGatesFinalization(t *testing.T) {
	t.Parallel()
	const correct = "sha-256=:04s4ot1HbgRcKZ6O5dZGaDRFbZe9WSpxdGtCOmoF84Y=:"
	var stage bytes.Buffer
	n, err := transporthttp.VerifyDigestTo(&stage, strings.NewReader("Wikipedia"), []string{correct}, transporthttp.VerifyOptions{MaximumBytes: 9, ExpectedLength: 9})
	if err != nil || n != 9 || stage.String() != "Wikipedia" {
		t.Fatalf("verified stream = %q, %d, %v", stage.String(), n, err)
	}

	finalized := false
	stage.Reset()
	_, err = transporthttp.VerifyDigestTo(&stage, strings.NewReader("WikipediA"), []string{correct}, transporthttp.VerifyOptions{MaximumBytes: 9, ExpectedLength: 9})
	if err == nil {
		finalized = true
	}
	if !errors.Is(err, transporthttp.ErrDigestMismatch) || finalized {
		t.Fatalf("mismatch = %v, finalized = %v", err, finalized)
	}

	stage.Reset()
	_, err = transporthttp.VerifyDigestTo(&stage, strings.NewReader("Wikipedia"), []string{correct}, transporthttp.VerifyOptions{MaximumBytes: 4, ExpectedLength: 9})
	if !errors.Is(err, transporthttp.ErrDigestLimitExceeded) || stage.Len() > 5 {
		t.Fatalf("bounded stream = %d bytes, %v", stage.Len(), err)
	}

	stage.Reset()
	_, err = transporthttp.VerifyDigestTo(&stage, strings.NewReader("Wiki"), []string{correct}, transporthttp.VerifyOptions{MaximumBytes: 9, ExpectedLength: 9})
	if !errors.Is(err, transporthttp.ErrDigestTruncated) {
		t.Fatalf("truncated stream error = %v", err)
	}

	stage.Reset()
	overlength := &countingReader{Reader: strings.NewReader("Wikipedia")}
	n, err = transporthttp.VerifyDigestTo(&stage, overlength, []string{correct}, transporthttp.VerifyOptions{MaximumBytes: 9, ExpectedLength: 1})
	if !errors.Is(err, transporthttp.ErrDigestLengthMismatch) || n != 2 || overlength.read != 2 || stage.Len() != 2 {
		t.Fatalf("overlength stream = %d bytes read, %d staged, %d reported, %v", overlength.read, stage.Len(), n, err)
	}

	stage.Reset()
	impossible := &countingReader{Reader: strings.NewReader("Wikipedia")}
	n, err = transporthttp.VerifyDigestTo(&stage, impossible, []string{correct}, transporthttp.VerifyOptions{MaximumBytes: 4, ExpectedLength: 9})
	if !errors.Is(err, transporthttp.ErrDigestLimitExceeded) || n != 0 || impossible.read != 0 || stage.Len() != 0 {
		t.Fatalf("impossible length = %d bytes read, %d staged, %d reported, %v", impossible.read, stage.Len(), n, err)
	}

	streamErr := errors.New("stream read failed")
	stage.Reset()
	n, err = transporthttp.VerifyDigestTo(&stage, &terminalErrorReader{payload: []byte("Wiki"), err: streamErr}, []string{correct}, transporthttp.VerifyOptions{MaximumBytes: 9, ExpectedLength: 9})
	if !errors.Is(err, streamErr) || errors.Is(err, transporthttp.ErrDigestTruncated) || n != 4 {
		t.Fatalf("stream read error = %d, %v", n, err)
	}

	writeErr := errors.New("staging write failed")
	pipeReader, pipeWriter := io.Pipe()
	if err := pipeReader.CloseWithError(writeErr); err != nil {
		t.Fatal(err)
	}
	n, err = transporthttp.VerifyDigestTo(pipeWriter, strings.NewReader("Wikipedia"), []string{correct}, transporthttp.VerifyOptions{MaximumBytes: 9, ExpectedLength: 9})
	_ = pipeWriter.Close()
	if !errors.Is(err, writeErr) || errors.Is(err, transporthttp.ErrDigestTruncated) || n != 0 {
		t.Fatalf("staging write error = %d, %v", n, err)
	}

	_, err = transporthttp.VerifyDigestTo(&stage, strings.NewReader(""), []string{correct}, transporthttp.VerifyOptions{MaximumBytes: math.MaxInt64, ExpectedLength: -1})
	if !errors.Is(err, transporthttp.ErrMalformedDigest) {
		t.Fatalf("overflowing limit error = %v", err)
	}
}

type countingReader struct {
	io.Reader
	read int
}

func (r *countingReader) Read(payload []byte) (int, error) {
	n, err := r.Reader.Read(payload)
	r.read += n
	return n, err
}

type terminalErrorReader struct {
	payload []byte
	err     error
	done    bool
}

func (r *terminalErrorReader) Read(payload []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(payload, r.payload), r.err
}

func TestDigestTrailerCapabilityIsExplicit(t *testing.T) {
	t.Parallel()
	if transporthttp.TrailerDigestVerificationSupported {
		t.Fatal("core digest profile must not claim trailer verification")
	}
}

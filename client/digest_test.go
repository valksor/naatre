package client

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/valksor/naatre/protocol/httpdigest"
)

const digestResponseEnvelope = `{"requestId":"digest-profile","data":{"ok":true},"capabilities":[],"extensions":{}}`

func TestExecuteVerifiesHTTPDigestBeforeSDKSuccess(t *testing.T) {
	t.Parallel()
	representation := []byte(digestResponseEnvelope)
	tests := []struct {
		name     string
		encoding string
		content  []byte
	}{
		{name: "identity", encoding: "identity", content: representation},
		{name: "gzip", encoding: "gzip", content: gzipClientDigestBytes(t, representation)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requestBody, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				if _, err := httpdigest.VerifyDigestTo(io.Discard, bytes.NewReader(requestBody), request.Header.Values("Content-Digest"), httpdigest.VerifyOptions{MaximumBytes: int64(len(requestBody)) + 1, ExpectedLength: int64(len(requestBody))}); err != nil {
					return nil, err
				}
				if request.Header.Get("Want-Content-Digest") != preferredDigestAlgorithms || request.Header.Get("Want-Repr-Digest") != preferredDigestAlgorithms {
					return nil, errors.New("digest negotiation headers missing")
				}
				contentDigest, _ := httpdigest.FormatDigestField(test.content, httpdigest.SHA512)
				reprDigest, _ := httpdigest.FormatDigestField(representation, httpdigest.SHA512)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Type":     []string{ResponseMediaType},
						"Content-Encoding": []string{test.encoding},
						"Content-Digest":   []string{contentDigest},
						"Repr-Digest":      []string{reprDigest},
					},
					ContentLength: int64(len(test.content)),
					Body:          io.NopCloser(bytes.NewReader(test.content)),
				}, nil
			})
			client, err := New(Config{
				Endpoint:   "https://example.test/v1/execute",
				HTTPClient: &http.Client{Transport: transport},
				HTTPDigest: &DigestConfig{},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Execute(context.Background(), validRequest(t))
			if err != nil {
				t.Fatalf("Execute: %v cause=%v", err, errors.Unwrap(err))
			}
			if result.Integrity == nil || result.Integrity.Profile != httpdigest.DigestProfile || result.Integrity.ContentAlgorithm != httpdigest.SHA512 || result.Integrity.RepresentationBytes != int64(len(representation)) {
				t.Fatalf("integrity = %#v", result.Integrity)
			}
		})
	}
}

func TestExecuteDigestFailuresAreStableAndGateBodyUse(t *testing.T) {
	t.Parallel()
	body := []byte(digestResponseEnvelope)
	sha512, err := httpdigest.FormatDigestField(body, httpdigest.SHA512)
	if err != nil {
		t.Fatal(err)
	}
	sha256, err := httpdigest.FormatDigestField(body, httpdigest.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	conflicting := sha512 + ", sha-256=:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=:"

	tests := []struct {
		name          string
		content       []byte
		contentDigest []string
		reprDigest    []string
		trailer       http.Header
		contentLength int64
		bodyError     error
		cancelOnError bool
		code          string
		wantReads     int
	}{
		{name: "malformed", content: body, contentDigest: []string{"sha-512=bad"}, reprDigest: []string{sha512}, contentLength: int64(len(body)), code: httpdigest.CodeMalformedDigest},
		{name: "late", content: body, contentDigest: []string{sha512}, reprDigest: []string{sha512}, trailer: http.Header{"Content-Digest": []string{sha512}}, contentLength: int64(len(body)), code: httpdigest.CodeDigestTrailerUnsupported},
		{name: "missing", content: body, reprDigest: []string{sha512}, contentLength: int64(len(body)), code: httpdigest.CodeDigestRequired},
		{name: "downgraded", content: body, contentDigest: []string{sha256}, reprDigest: []string{sha512}, contentLength: int64(len(body)), code: httpdigest.CodeDigestDowngrade},
		{name: "conflicting", content: body, contentDigest: []string{conflicting}, reprDigest: []string{sha512}, contentLength: int64(len(body)), code: httpdigest.CodeDigestMismatch, wantReads: len(body)},
		{name: "intermediary-altered", content: append(append([]byte(nil), body...), ' '), contentDigest: []string{sha512}, reprDigest: []string{sha512}, contentLength: int64(len(body) + 1), code: httpdigest.CodeDigestMismatch, wantReads: len(body) + 1},
		{name: "incomplete", content: body, contentDigest: []string{sha512}, reprDigest: []string{sha512}, contentLength: int64(len(body) + 3), code: httpdigest.CodeDigestTruncated, wantReads: len(body)},
		{name: "canceled", contentDigest: []string{sha512}, reprDigest: []string{sha512}, contentLength: -1, bodyError: context.Canceled, code: httpdigest.CodeDigestCanceled},
		{name: "canceled-with-io-error", contentDigest: []string{sha512}, reprDigest: []string{sha512}, contentLength: -1, bodyError: errors.New("read failed"), cancelOnError: true, code: httpdigest.CodeDigestCanceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			var cancel context.CancelFunc
			if test.cancelOnError {
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
			}
			bodyReader := &digestReadCloser{reader: bytes.NewReader(test.content), terminal: test.bodyError, cancel: cancel}
			transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
				header := http.Header{
					"Content-Type":   []string{ResponseMediaType},
					"Content-Digest": test.contentDigest,
					"Repr-Digest":    test.reprDigest,
				}
				return &http.Response{StatusCode: http.StatusOK, Header: header, Trailer: test.trailer, ContentLength: test.contentLength, Body: bodyReader}, nil
			})
			client, err := New(Config{Endpoint: "https://example.test/v1/execute", HTTPClient: &http.Client{Transport: transport}, HTTPDigest: &DigestConfig{}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Execute(ctx, validRequest(t))
			assertClientCode(t, err, test.code)
			if bodyReader.read != test.wantReads {
				t.Fatalf("body bytes read = %d, want %d", bodyReader.read, test.wantReads)
			}
			if strings.Contains(err.Error(), digestResponseEnvelope) || strings.Contains(err.Error(), sha512) {
				t.Fatalf("public error leaked protected input: %v", err)
			}
		})
	}
}

func TestExecuteVerifiesRangeRepresentationFromExplicitSource(t *testing.T) {
	t.Parallel()
	content := []byte(digestResponseEnvelope)
	representation := append(append([]byte(nil), content...), ' ')
	contentDigest, _ := httpdigest.FormatDigestField(content, httpdigest.SHA512)
	reprDigest, _ := httpdigest.FormatDigestField(representation, httpdigest.SHA512)
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusPartialContent,
			Header: http.Header{
				"Content-Type":   []string{ResponseMediaType},
				"Content-Range":  []string{"bytes 0-" + strconv.Itoa(len(content)-1) + "/" + strconv.Itoa(len(representation))},
				"Content-Digest": []string{contentDigest},
				"Repr-Digest":    []string{reprDigest},
			},
			ContentLength: int64(len(content)), Body: io.NopCloser(bytes.NewReader(content)),
		}, nil
	})
	client, err := New(Config{
		Endpoint: "https://example.test/v1/execute", HTTPClient: &http.Client{Transport: transport},
		HTTPDigest: &DigestConfig{RangeRepresentation: func(context.Context, *http.Response) (io.ReadCloser, int64, error) {
			return io.NopCloser(bytes.NewReader(representation)), int64(len(representation)), nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Execute(context.Background(), validRequest(t))
	if err != nil {
		t.Fatalf("Execute: %v cause=%v", err, errors.Unwrap(err))
	}
	if result.Integrity == nil || !result.Integrity.Range || result.Integrity.RepresentationBytes != int64(len(representation)) {
		t.Fatalf("integrity = %#v", result.Integrity)
	}
}

type digestReadCloser struct {
	reader   *bytes.Reader
	terminal error
	cancel   context.CancelFunc
	read     int
}

func (reader *digestReadCloser) Read(payload []byte) (int, error) {
	n, err := reader.reader.Read(payload)
	reader.read += n
	if err == io.EOF && reader.terminal != nil {
		if reader.cancel != nil {
			reader.cancel()
		}
		return n, reader.terminal
	}
	return n, err
}

func (*digestReadCloser) Close() error { return nil }

func gzipClientDigestBytes(t *testing.T, content []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

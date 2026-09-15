package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/valksor/naatre/protocol/httpdigest"
)

const preferredDigestAlgorithms = "sha-512=10, sha-256=1"

func (c *Client) applyRequestDigest(request *http.Request, body []byte) error {
	if c.digest == nil {
		return nil
	}
	contentDigest, err := httpdigest.FormatDigestField(body, httpdigest.SHA256)
	if err != nil {
		return clientError(httpdigest.ErrorCode(err), 0, nil)
	}
	request.Header.Set("Content-Digest", contentDigest)
	request.Header.Set("Repr-Digest", contentDigest)
	request.Header.Set("Want-Content-Digest", c.digest.WantContentDigest)
	request.Header.Set("Want-Repr-Digest", c.digest.WantReprDigest)
	return nil
}

func resolveDigestConfig(config *DigestConfig) (*DigestConfig, httpdigest.DigestAlgorithm, httpdigest.DigestAlgorithm, error) {
	if config == nil {
		return nil, "", "", nil
	}
	resolved := *config
	if resolved.WantContentDigest == "" {
		resolved.WantContentDigest = preferredDigestAlgorithms
	}
	if resolved.WantReprDigest == "" {
		resolved.WantReprDigest = preferredDigestAlgorithms
	}
	contentAlgorithm, err := httpdigest.NegotiateDigestAlgorithm([]string{resolved.WantContentDigest})
	if err != nil {
		return nil, "", "", err
	}
	reprAlgorithm, err := httpdigest.NegotiateDigestAlgorithm([]string{resolved.WantReprDigest})
	if err != nil {
		return nil, "", "", err
	}
	return &resolved, contentAlgorithm, reprAlgorithm, nil
}

func (c *Client) validateDigestHeaders(response *http.Response) error {
	if c.digest == nil {
		return nil
	}
	if err := httpdigest.RejectTrailers(response.Header, response.Trailer); err != nil {
		return err
	}
	if err := httpdigest.ValidateContentEncoding(response.Header); err != nil {
		return err
	}
	if err := httpdigest.RequireAlgorithm(response.Header.Values("Content-Digest"), c.contentAlgorithm); err != nil {
		return err
	}
	return httpdigest.RequireAlgorithm(response.Header.Values("Repr-Digest"), c.reprAlgorithm)
}

func (c *Client) verifyResponseDigests(ctx context.Context, response *http.Response, content, representation []byte) (*DigestVerification, error) {
	if c.digest == nil {
		return nil, nil
	}
	if err := httpdigest.RejectTrailers(response.Header, response.Trailer); err != nil {
		return nil, err
	}
	if err := httpdigest.ValidateResponseRange(response.StatusCode, response.Header, int64(len(content))); err != nil {
		return nil, err
	}
	expectedLength := response.ContentLength
	if _, err := httpdigest.VerifyDigestTo(io.Discard, bytes.NewReader(content), response.Header.Values("Content-Digest"), httpdigest.VerifyOptions{
		MaximumBytes: c.maxCompressedBytes, ExpectedLength: expectedLength,
	}); err != nil {
		return nil, normalizeClientDigestError(ctx, err)
	}
	representationBytes := representation
	if response.StatusCode == http.StatusPartialContent {
		if c.digest.RangeRepresentation == nil {
			return nil, httpdigest.ErrDigestRepresentationUnavailable
		}
		source, length, err := c.digest.RangeRepresentation(ctx, response)
		if err != nil || source == nil {
			return nil, httpdigest.ErrDigestRepresentationUnavailable
		}
		defer func() { _ = source.Close() }()
		var staged bytes.Buffer
		_, err = httpdigest.VerifyDigestTo(&staged, source, response.Header.Values("Repr-Digest"), httpdigest.VerifyOptions{
			MaximumBytes: c.maxDecompressedBytes, ExpectedLength: length,
		})
		if err != nil {
			return nil, normalizeClientDigestError(ctx, err)
		}
		representationBytes = staged.Bytes()
	} else if _, err := httpdigest.VerifyDigestTo(io.Discard, bytes.NewReader(representation), response.Header.Values("Repr-Digest"), httpdigest.VerifyOptions{
		MaximumBytes: c.maxDecompressedBytes, ExpectedLength: int64(len(representation)),
	}); err != nil {
		return nil, normalizeClientDigestError(ctx, err)
	}
	return &DigestVerification{
		Profile: httpdigest.DigestProfile, ContentAlgorithm: c.contentAlgorithm, RepresentationAlgorithm: c.reprAlgorithm,
		ContentBytes: int64(len(content)), RepresentationBytes: int64(len(representationBytes)), Range: response.StatusCode == http.StatusPartialContent,
	}, nil
}

func normalizeClientDigestError(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return httpdigest.ErrDigestCanceled
	}
	return err
}

func digestCodeForReadError(ctx context.Context, err error) string {
	switch {
	case ctx.Err() != nil, errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return httpdigest.CodeDigestCanceled
	case errors.Is(err, errResponseLimit):
		return httpdigest.CodeDigestLimitExceeded
	default:
		return httpdigest.CodeDigestIO
	}
}

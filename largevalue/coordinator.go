package largevalue

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/valksor/naatre/runtime"
	transporthttp "github.com/valksor/naatre/transport/http"
)

type Coordinator struct{ config Config }

func NewCoordinator(config Config) (*Coordinator, error) {
	switch {
	case config.Store == nil:
		return nil, errors.New("large value store is nil")
	case config.Authorizer == nil:
		return nil, errors.New("large value authorizer is nil")
	case config.Now == nil || config.NewID == nil:
		return nil, errors.New("large value clock and ID generator are required")
	case len(config.SigningKey) < 32:
		return nil, errors.New("large value signing key must contain at least 32 bytes")
	case config.MaximumTTL <= 0 || config.MaximumBytes <= DefaultSmallByteLimit || config.MaximumEncodedBytes <= 0:
		return nil, errors.New("large value limits must be positive")
	case config.MaximumExpansion <= 0 || config.CleanupTimeout <= 0:
		return nil, errors.New("large value expansion and cleanup limits must be positive")
	case len(config.AllowedMediaTypes) == 0 || len(config.AllowedProfiles) == 0:
		return nil, errors.New("large value media types and profiles are required")
	default:
		config.SigningKey = append([]byte(nil), config.SigningKey...)
		config.AllowedMediaTypes = append([]string(nil), config.AllowedMediaTypes...)
		config.AllowedProfiles = append([]TransferProfile(nil), config.AllowedProfiles...)
		return &Coordinator{config: config}, nil
	}
}

func (c *Coordinator) Issue(ctx context.Context, request IssueRequest) (Capability, error) {
	principal, ok := runtime.PrincipalFromContext(ctx)
	if !ok || principal.Subject == "" || request.TTL <= 0 || request.TTL > c.config.MaximumTTL ||
		(request.Direction != Upload && request.Direction != Download) || !slices.Contains(c.config.AllowedProfiles, request.Profile) {
		return Capability{}, ErrUnavailable
	}
	if request.Binding.Principal != principal.Subject || request.Binding.Tenant != principal.Tenant ||
		request.Binding.AuthorizationRevision != principal.AuthorizationRevision || request.Binding.OperationDigest == "" {
		return Capability{}, ErrUnavailable
	}
	if err := validateMetadata(request.Metadata, c.config); err != nil {
		return Capability{}, err
	}
	methods, err := validateMethods(request.Direction, request.Methods)
	if err != nil || validateTransferURL(request.TransferURL) != nil {
		return Capability{}, ErrInvalidMetadata
	}
	if request.TransferURL != "" {
		if c.config.Egress == nil {
			return Capability{}, ErrEgressDenied
		}
		if _, err := c.config.Egress.ValidateHop(ctx, request.TransferURL); err != nil {
			return Capability{}, err
		}
	}
	allowed, err := c.config.Authorizer.AuthorizeLargeValue(ctx, AuthorizationRequest{
		Action: AuthorizeIssue, Principal: principal, Direction: request.Direction, Binding: request.Binding, Metadata: request.Metadata,
	})
	if err != nil || !allowed {
		return Capability{}, ErrUnavailable
	}
	id, err := c.config.NewID()
	if err != nil || strings.TrimSpace(id) == "" {
		return Capability{}, ErrUnavailable
	}
	expires := c.config.Now().UTC().Add(request.TTL)
	record := Record{ID: id, Direction: request.Direction, Profile: request.Profile, Binding: request.Binding,
		Metadata: request.Metadata, Methods: methods, TransferURL: request.TransferURL, ExpiresAt: expires,
		SingleUse: request.SingleUse, Revision: 1}
	if err := c.config.Store.Create(ctx, record); err != nil {
		return Capability{}, ErrUnavailable
	}
	reference, err := c.sign(record)
	if err != nil {
		return Capability{}, ErrUnavailable
	}
	return Capability{Reference: reference, Direction: request.Direction, Profile: request.Profile, Methods: methods,
		TransferURL: request.TransferURL, ExpiresAt: expires}, nil
}

func (c *Coordinator) AcceptUpload(ctx context.Context, reference string, source io.Reader) (err error) {
	return c.finalize(ctx, reference, source, Upload, AuthorizeUpload)
}

// FinalizeDownload verifies an application-provided download source before a
// protected handler can consume it or an adapter can serve it.
func (c *Coordinator) FinalizeDownload(ctx context.Context, reference string, source io.Reader) error {
	return c.finalize(ctx, reference, source, Download, AuthorizeFinalize)
}

func (c *Coordinator) finalize(ctx context.Context, reference string, source io.Reader, direction Direction, action AuthorizationAction) (err error) {
	record, principal, err := c.authorizedRecord(ctx, reference, action)
	if err != nil || record.Direction != direction || record.Finalized || source == nil {
		return ErrUnavailable
	}
	stage, err := c.config.Store.Begin(ctx, record.ID, record.Revision)
	if err != nil {
		return ErrTransferFailed
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.config.CleanupTimeout)
		defer cancel()
		if abortErr := stage.Abort(cleanup); abortErr != nil && err == nil {
			err = fmt.Errorf("%w: staging cleanup failed", ErrTransferFailed)
		}
	}()

	representation, closeRepresentation, err := c.representationReader(source, record.Metadata)
	if err != nil {
		return ErrTransferFailed
	}
	written, err := transporthttp.VerifyDigestTo(stage, representation, []string{record.Metadata.RepresentationDigest}, transporthttp.VerifyOptions{
		MaximumBytes: c.config.MaximumBytes, ExpectedLength: record.Metadata.Length,
	})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrTransferFailed, err)
	}
	if closeRepresentation != nil {
		if err := closeRepresentation(); err != nil {
			return fmt.Errorf("%w: %w", ErrTransferFailed, err)
		}
	}
	if err := stage.Close(); err != nil {
		return ErrTransferFailed
	}
	if c.config.Scanner != nil {
		result, scanErr := c.config.Scanner.ScanLargeValue(ctx, record.Metadata, func() (ReadCloser, error) { return stage.Open(ctx) })
		declared, _, _ := strings.Cut(record.Metadata.MediaType, ";")
		detected, _, _ := strings.Cut(result.DetectedMediaType, ";")
		if scanErr != nil || !result.Clean || (detected != "" && detected != declared) {
			return fmt.Errorf("%w: content scan rejected", ErrTransferFailed)
		}
	}
	allowed, err := c.config.Authorizer.AuthorizeLargeValue(ctx, AuthorizationRequest{
		Action: action, Principal: principal, Direction: record.Direction, Binding: record.Binding, Metadata: record.Metadata, RecordID: record.ID,
	})
	if err != nil || !allowed {
		return ErrUnavailable
	}
	if err := stage.Commit(ctx, written); err != nil {
		return ErrTransferFailed
	}
	committed = true
	return nil
}

func (c *Coordinator) Consume(ctx context.Context, reference string, handler func(context.Context, Record, io.Reader) error) error {
	record, _, err := c.authorizedRecord(ctx, reference, AuthorizeConsume)
	if err != nil || !record.Finalized || handler == nil {
		return ErrUnavailable
	}
	if record.SingleUse {
		claimed, ok, claimErr := c.config.Store.Claim(ctx, record.ID, record.Revision)
		if claimErr != nil || !ok {
			return ErrUnavailable
		}
		record = claimed
	}
	reader, err := c.config.Store.Open(ctx, record.ID)
	if err != nil {
		return ErrUnavailable
	}
	handlerErr := handler(ctx, record, reader)
	closeErr := reader.Close()
	if handlerErr != nil {
		return handlerErr
	}
	if closeErr != nil {
		return ErrTransferFailed
	}
	return nil
}

// Inspect authorizes and resolves a capability without opening its content.
// When method is non-empty it also enforces the method signed into the
// capability. Adapter packages use it to gate staging and every remote
// redirect hop. The returned record excludes the bearer reference.
func (c *Coordinator) Inspect(ctx context.Context, reference string, action AuthorizationAction, method string) (Record, error) {
	record, _, err := c.authorizedRecord(ctx, reference, action)
	if err != nil || (method != "" && !slices.Contains(record.Methods, method)) {
		return Record{}, ErrUnavailable
	}
	return record, nil
}

func (c *Coordinator) Revoke(ctx context.Context, reference string) error {
	record, _, err := c.authorizedRecord(ctx, reference, AuthorizeRevoke)
	if err != nil {
		return err
	}
	if revoked, err := c.config.Store.Revoke(ctx, record.ID, record.Revision); err != nil || !revoked {
		return ErrUnavailable
	}
	return nil
}

func (c *Coordinator) DeleteExpired(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, errors.New("cleanup limit must be positive")
	}
	return c.config.Store.DeleteExpired(ctx, c.config.Now().UTC(), limit)
}

type capabilityClaims struct {
	Version               int       `json:"v"`
	ID                    string    `json:"id"`
	Direction             Direction `json:"direction"`
	Principal             string    `json:"principal"`
	Tenant                string    `json:"tenant,omitempty"`
	AuthorizationRevision string    `json:"authorizationRevision"`
	Expires               int64     `json:"expires"`
	Methods               []string  `json:"methods"`
	RecordDigest          string    `json:"recordDigest"`
}

func (c *Coordinator) sign(record Record) (string, error) {
	claims := capabilityClaims{Version: 1, ID: record.ID, Direction: record.Direction, Principal: record.Binding.Principal,
		Tenant: record.Binding.Tenant, AuthorizationRevision: record.Binding.AuthorizationRevision, Expires: record.ExpiresAt.Unix(), Methods: record.Methods,
		RecordDigest: recordDigest(record)}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, c.config.SigningKey)
	_, _ = mac.Write([]byte("naatre:large-value-capability:v1\n" + encoded))
	return "v1." + encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (c *Coordinator) authorizedRecord(ctx context.Context, reference string, action AuthorizationAction) (Record, runtime.Principal, error) {
	claims, err := c.verify(reference)
	if err != nil {
		return Record{}, runtime.Principal{}, ErrUnavailable
	}
	principal, ok := runtime.PrincipalFromContext(ctx)
	if !ok || principal.Subject != claims.Principal || principal.Tenant != claims.Tenant || principal.AuthorizationRevision != claims.AuthorizationRevision {
		return Record{}, runtime.Principal{}, ErrUnavailable
	}
	record, err := c.config.Store.Load(ctx, claims.ID)
	if err != nil || record.Revoked || record.Consumed || !record.ExpiresAt.After(c.config.Now().UTC()) ||
		record.Direction != claims.Direction || record.Binding.Principal != claims.Principal || record.Binding.Tenant != claims.Tenant ||
		record.Binding.AuthorizationRevision != claims.AuthorizationRevision || record.ExpiresAt.Unix() != claims.Expires || !slices.Equal(record.Methods, claims.Methods) {
		return Record{}, runtime.Principal{}, ErrUnavailable
	}
	if !hmac.Equal([]byte(recordDigest(record)), []byte(claims.RecordDigest)) {
		return Record{}, runtime.Principal{}, ErrUnavailable
	}
	allowed, err := c.config.Authorizer.AuthorizeLargeValue(ctx, AuthorizationRequest{Action: action, Principal: principal,
		Direction: record.Direction, Binding: record.Binding, Metadata: record.Metadata, RecordID: record.ID})
	if err != nil || !allowed {
		return Record{}, runtime.Principal{}, ErrUnavailable
	}
	return record, principal, nil
}

func (c *Coordinator) verify(reference string) (capabilityClaims, error) {
	parts := strings.Split(reference, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return capabilityClaims{}, ErrInvalidCapability
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return capabilityClaims{}, ErrInvalidCapability
	}
	mac := hmac.New(sha256.New, c.config.SigningKey)
	_, _ = mac.Write([]byte("naatre:large-value-capability:v1\n" + parts[1]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return capabilityClaims{}, ErrInvalidCapability
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return capabilityClaims{}, ErrInvalidCapability
	}
	var claims capabilityClaims
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&claims); err != nil || claims.Version != 1 || claims.ID == "" || claims.Principal == "" || claims.Expires <= 0 || claims.RecordDigest == "" {
		return capabilityClaims{}, ErrInvalidCapability
	}
	return claims, nil
}

func recordDigest(record Record) string {
	payload, _ := json.Marshal(struct {
		ID          string
		Direction   Direction
		Profile     TransferProfile
		Binding     Binding
		Metadata    Metadata
		Methods     []string
		TransferURL string
		Expires     int64
		SingleUse   bool
	}{record.ID, record.Direction, record.Profile, record.Binding, record.Metadata, record.Methods,
		record.TransferURL, record.ExpiresAt.Unix(), record.SingleUse})
	digest := sha256.Sum256(payload)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func (c *Coordinator) representationReader(source io.Reader, metadata Metadata) (io.Reader, func() error, error) {
	limited := &encodedReader{reader: source, maximum: c.config.MaximumEncodedBytes, expected: metadata.EncodedLength}
	if metadata.ContentCoding == "" || metadata.ContentCoding == "identity" {
		return limited, nil, nil
	}
	buffered := bufio.NewReader(limited)
	reader, err := gzip.NewReader(buffered)
	if err != nil {
		return nil, nil, err
	}
	reader.Multistream(false)
	return reader, func() error {
		if err := reader.Close(); err != nil {
			return err
		}
		if _, err := buffered.ReadByte(); err != io.EOF {
			return ErrTransferFailed
		}
		return limited.complete()
	}, nil
}

type encodedReader struct {
	reader   io.Reader
	maximum  int64
	expected int64
	read     int64
}

func (r *encodedReader) Read(buffer []byte) (int, error) {
	remaining := r.maximum - r.read
	if remaining < int64(len(buffer)) {
		buffer = buffer[:max(remaining+1, 0)]
	}
	n, err := r.reader.Read(buffer)
	r.read += int64(n)
	if r.read > r.maximum {
		return n, transporthttp.ErrDigestLimitExceeded
	}
	return n, err
}

func (r *encodedReader) complete() error {
	if r.read < r.expected {
		return transporthttp.ErrDigestTruncated
	}
	if r.read > r.expected {
		return transporthttp.ErrDigestLengthMismatch
	}
	return nil
}

func validateMethods(direction Direction, methods []string) ([]string, error) {
	result := append([]string(nil), methods...)
	if len(result) == 0 {
		if direction == Upload {
			result = []string{"PUT"}
		} else {
			result = []string{"GET", "HEAD"}
		}
	}
	slices.Sort(result)
	result = slices.Compact(result)
	for _, method := range result {
		if (direction == Upload && method != "POST" && method != "PUT" && method != "PATCH") ||
			(direction == Download && method != "GET" && method != "HEAD") {
			return nil, ErrInvalidMetadata
		}
	}
	return result, nil
}

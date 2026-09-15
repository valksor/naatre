// Package largevalue defines the core.large-value-1 contract for payloads
// whose bytes travel outside the canonical operation document.
package largevalue

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/valksor/naatre/runtime"
	transporthttp "github.com/valksor/naatre/transport/http"
)

const (
	Profile                 = "core.large-value-1"
	DefaultSmallByteLimit   = 64 << 10
	DefaultMaximumFilename  = 255
	DefaultMaximumRedirects = 5
)

var (
	ErrUnavailable       = errors.New("payload reference unavailable")
	ErrInvalidMetadata   = errors.New("invalid payload metadata")
	ErrInvalidCapability = errors.New("invalid payload capability")
	ErrTransferFailed    = errors.New("payload transfer failed")
	ErrRangeUnsatisfied  = errors.New("payload range unsatisfied")
	ErrEgressDenied      = errors.New("payload egress denied")
)

type Direction string

const (
	Upload   Direction = "upload"
	Download Direction = "download"
)

type TransferProfile string

const (
	DirectProfile      TransferProfile = "direct"
	PresignedProfile   TransferProfile = "presigned"
	MultipartProfile   TransferProfile = "multipart"
	ResumableProfile   TransferProfile = "resumable"
	ApplicationProfile TransferProfile = "application"
)

type CleanupOwner string

const (
	CleanupCoordinator CleanupOwner = "coordinator"
	CleanupStore       CleanupOwner = "store"
	CleanupApplication CleanupOwner = "application"
)

type SlotDeclaration struct {
	Name              string          `json:"name"`
	Direction         Direction       `json:"direction"`
	Required          bool            `json:"required"`
	Profile           TransferProfile `json:"profile"`
	AllowedMediaTypes []string        `json:"allowedMediaTypes"`
	MaximumLength     int64           `json:"maximumLength"`
}

type Metadata struct {
	Slot                 string `json:"slot"`
	Filename             string `json:"filename,omitempty"`
	MediaType            string `json:"mediaType"`
	Length               int64  `json:"length"`
	RepresentationDigest string `json:"representationDigest"`
	ContentCoding        string `json:"contentCoding"`
	EncodedLength        int64  `json:"encodedLength"`
}

type ResolvedReference struct {
	Slot      string `json:"slot"`
	Reference string `json:"reference"`
	MediaType string `json:"mediaType"`
	Length    int64  `json:"length"`
	Digest    string `json:"digest"`
}

type Binding struct {
	Principal             string
	Tenant                string
	AuthorizationRevision string
	OperationDigest       string
}

type Capability struct {
	Reference   string
	Direction   Direction
	Profile     TransferProfile
	Methods     []string
	TransferURL string
	ExpiresAt   time.Time
}

// String prevents bearer material from being emitted by ordinary formatting.
func (Capability) String() string { return "payload-capability-redacted" }

type Record struct {
	ID          string
	Direction   Direction
	Profile     TransferProfile
	Binding     Binding
	Metadata    Metadata
	Methods     []string
	TransferURL string
	ExpiresAt   time.Time
	SingleUse   bool
	Consumed    bool
	Revoked     bool
	Finalized   bool
	Revision    uint64
}

type AuthorizationAction string

const (
	AuthorizeIssue    AuthorizationAction = "issue"
	AuthorizeUpload   AuthorizationAction = "upload"
	AuthorizeFinalize AuthorizationAction = "finalize"
	AuthorizeConsume  AuthorizationAction = "consume"
	AuthorizeRevoke   AuthorizationAction = "revoke"
)

type AuthorizationRequest struct {
	Action    AuthorizationAction
	Principal runtime.Principal
	Direction Direction
	Binding   Binding
	Metadata  Metadata
	RecordID  string
}

type Authorizer interface {
	AuthorizeLargeValue(context.Context, AuthorizationRequest) (bool, error)
}

type AuthorizerFunc func(context.Context, AuthorizationRequest) (bool, error)

func (f AuthorizerFunc) AuthorizeLargeValue(ctx context.Context, request AuthorizationRequest) (bool, error) {
	return f(ctx, request)
}

type ScanResult struct {
	Clean             bool
	DetectedMediaType string
}

type Scanner interface {
	ScanLargeValue(context.Context, Metadata, func() (ReadCloser, error)) (ScanResult, error)
}

type ScannerFunc func(context.Context, Metadata, func() (ReadCloser, error)) (ScanResult, error)

func (f ScannerFunc) ScanLargeValue(ctx context.Context, metadata Metadata, open func() (ReadCloser, error)) (ScanResult, error) {
	return f(ctx, metadata, open)
}

type ReadCloser interface {
	Read([]byte) (int, error)
	Close() error
}

type Stage interface {
	Write([]byte) (int, error)
	Close() error
	Open(context.Context) (ReadCloser, error)
	Commit(context.Context, int64) error
	Abort(context.Context) error
}

type Store interface {
	Create(context.Context, Record) error
	Load(context.Context, string) (Record, error)
	Begin(context.Context, string, uint64) (Stage, error)
	Open(context.Context, string) (ReadCloser, error)
	Claim(context.Context, string, uint64) (Record, bool, error)
	Revoke(context.Context, string, uint64) (bool, error)
	DeleteExpired(context.Context, time.Time, int) (int, error)
}

type IssueRequest struct {
	Direction   Direction
	Profile     TransferProfile
	Binding     Binding
	Metadata    Metadata
	Methods     []string
	TransferURL string
	TTL         time.Duration
	SingleUse   bool
}

type Config struct {
	Store               Store
	Authorizer          Authorizer
	Scanner             Scanner
	Now                 func() time.Time
	NewID               func() (string, error)
	SigningKey          []byte
	MaximumTTL          time.Duration
	MaximumBytes        int64
	MaximumEncodedBytes int64
	MaximumExpansion    int64
	AllowedMediaTypes   []string
	AllowedProfiles     []TransferProfile
	CleanupTimeout      time.Duration
	Egress              *EgressPolicy
}

func validateMetadata(metadata Metadata, config Config) error {
	if strings.TrimSpace(metadata.Slot) == "" || metadata.Length < 0 || metadata.Length > config.MaximumBytes ||
		metadata.EncodedLength < 0 || metadata.EncodedLength > config.MaximumEncodedBytes {
		return ErrInvalidMetadata
	}
	if metadata.ContentCoding == "" {
		metadata.ContentCoding = "identity"
	}
	if metadata.ContentCoding != "identity" && metadata.ContentCoding != "gzip" {
		return fmt.Errorf("%w: unsupported content coding", ErrInvalidMetadata)
	}
	if metadata.ContentCoding == "identity" && metadata.EncodedLength != metadata.Length {
		return fmt.Errorf("%w: identity lengths differ", ErrInvalidMetadata)
	}
	if metadata.ContentCoding == "gzip" {
		if metadata.EncodedLength == 0 {
			return fmt.Errorf("%w: expansion ratio exceeds policy", ErrInvalidMetadata)
		}
		quotient, remainder := metadata.Length/metadata.EncodedLength, metadata.Length%metadata.EncodedLength
		if quotient > config.MaximumExpansion || (quotient == config.MaximumExpansion && remainder > 0) {
			return fmt.Errorf("%w: expansion ratio exceeds policy", ErrInvalidMetadata)
		}
	}
	mediaType, _, err := mime.ParseMediaType(metadata.MediaType)
	if err != nil || mediaType != strings.ToLower(mediaType) || !slices.Contains(config.AllowedMediaTypes, mediaType) {
		return fmt.Errorf("%w: media type denied", ErrInvalidMetadata)
	}
	if err := validateFilename(metadata.Filename); err != nil {
		return err
	}
	if strings.TrimSpace(metadata.RepresentationDigest) == "" {
		return fmt.Errorf("%w: representation digest required", ErrInvalidMetadata)
	}
	if _, err := transporthttp.ParseDigestFields([]string{metadata.RepresentationDigest}); err != nil {
		return fmt.Errorf("%w: malformed representation digest", ErrInvalidMetadata)
	}
	return nil
}

func validateFilename(name string) error {
	if name == "" {
		return nil
	}
	if !utf8.ValidString(name) || len(name) > DefaultMaximumFilename || name == "." || name == ".." ||
		strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return fmt.Errorf("%w: unsafe filename", ErrInvalidMetadata)
	}
	for _, char := range name {
		if unicode.IsControl(char) {
			return fmt.Errorf("%w: unsafe filename", ErrInvalidMetadata)
		}
	}
	return nil
}

func validateTransferURL(raw string) error {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ErrInvalidMetadata
	}
	return nil
}

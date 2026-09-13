package runtime

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/valksor/naatre/protocol"
)

var (
	// ErrInvalidCursor is intentionally opaque. Callers must not be able to
	// distinguish a bad signature from expired, rotated, or mismatched scope.
	ErrInvalidCursor      = errors.New("invalid collection cursor")
	ErrInvalidPage        = errors.New("invalid collection page request")
	ErrUnstableCollection = errors.New("collection positions are not strictly ordered")
)

type CursorDirection string

const (
	CursorForward  CursorDirection = "forward"
	CursorBackward CursorDirection = "backward"
)

type PaginationConsistency string

const (
	LivePagination     PaginationConsistency = "live"
	SnapshotPagination PaginationConsistency = "snapshot"
)

type CursorScopeOptions struct {
	Tenant         string
	Authorization  string
	SnapshotPolicy PaginationConsistency
	Snapshot       string
}

// CursorScope binds a cursor to the query that created it. Filter and sort
// documents are canonicalized before hashing, so object member order does not
// affect identity.
type CursorScope struct {
	valid         bool
	collection    string
	filters       string
	sort          string
	tenant        string
	authorization string
	policy        PaginationConsistency
	snapshot      string
}

func NewCursorScope(collection string, filters, sort []byte, options CursorScopeOptions) (CursorScope, error) {
	if collection == "" {
		return CursorScope{}, errors.New("cursor collection identity is required")
	}
	if options.SnapshotPolicy == "" {
		options.SnapshotPolicy = LivePagination
	}
	if options.SnapshotPolicy != LivePagination && options.SnapshotPolicy != SnapshotPagination {
		return CursorScope{}, fmt.Errorf("unsupported pagination consistency %q", options.SnapshotPolicy)
	}
	if (options.SnapshotPolicy == SnapshotPagination) != (options.Snapshot != "") {
		return CursorScope{}, errors.New("snapshot pagination requires exactly one snapshot identity")
	}
	canonicalFilters, err := protocol.CanonicalizeJSON(filters, protocol.DefaultLimits())
	if err != nil {
		return CursorScope{}, fmt.Errorf("canonicalize cursor filters: %w", err)
	}
	canonicalSort, err := protocol.CanonicalizeJSON(sort, protocol.DefaultLimits())
	if err != nil {
		return CursorScope{}, fmt.Errorf("canonicalize cursor sort: %w", err)
	}
	return CursorScope{
		valid: true, collection: collection, filters: cursorScopeDigest(canonicalFilters), sort: cursorScopeDigest(canonicalSort),
		tenant: options.Tenant, authorization: options.Authorization, policy: options.SnapshotPolicy, snapshot: options.Snapshot,
	}, nil
}

func cursorScopeDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

type CursorPosition struct {
	SortKey    string `json:"sortKey"`
	TieBreaker string `json:"tieBreaker"`
}

type CursorCodecConfig struct {
	ActiveKeyID string
	Keys        map[string][]byte
	TTL         time.Duration
	Now         func() time.Time
	MaxPageSize uint64
}

type CursorCodec struct {
	cursorSecrets
	maxPageSize uint64
}

func NewCursorCodec(config CursorCodecConfig) (*CursorCodec, error) {
	if config.MaxPageSize == 0 || config.MaxPageSize > uint64(^uint(0)>>1) {
		return nil, errors.New("cursor codec requires an active key, positive TTL, and positive maximum page size")
	}
	secrets, status := newCursorSecrets(config.ActiveKeyID, config.Keys, config.TTL, config.Now)
	switch status {
	case cursorSecretsValid:
	case cursorSecretsInvalidConfig:
		return nil, errors.New("cursor codec requires an active key, positive TTL, and positive maximum page size")
	case cursorSecretsInvalidKey:
		return nil, errors.New("cursor keys require an ID and at least 32 bytes")
	case cursorSecretsMissingActiveKey:
		return nil, errors.New("active cursor key is not present")
	}
	return &CursorCodec{cursorSecrets: secrets, maxPageSize: config.MaxPageSize}, nil
}

type cursorPayload struct {
	Version       uint8                 `json:"v"`
	KeyID         string                `json:"kid"`
	Expires       int64                 `json:"exp"`
	Collection    string                `json:"collection"`
	Filters       string                `json:"filters"`
	Sort          string                `json:"sort"`
	Direction     CursorDirection       `json:"direction"`
	Tenant        string                `json:"tenant,omitempty"`
	Authorization string                `json:"authorization,omitempty"`
	Policy        PaginationConsistency `json:"policy"`
	Snapshot      string                `json:"snapshot,omitempty"`
	Position      CursorPosition        `json:"position"`
}

func (c *CursorCodec) Encode(scope CursorScope, direction CursorDirection, position CursorPosition) (string, error) {
	if c == nil || !scope.valid || !validCursorDirection(direction) || position.TieBreaker == "" {
		return "", ErrInvalidCursor
	}
	payload := cursorPayload{
		Version: 1, KeyID: c.activeKeyID, Expires: c.now().Add(c.ttl).Unix(), Collection: scope.collection,
		Filters: scope.filters, Sort: scope.sort, Direction: direction, Tenant: scope.tenant,
		Authorization: scope.authorization, Policy: scope.policy, Snapshot: scope.snapshot, Position: position,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode cursor payload: %w", err)
	}
	signature := cursorSignature(c.keys[c.activeKeyID], encoded)
	return base64.RawURLEncoding.EncodeToString(encoded) + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (c *CursorCodec) Decode(cursor string, scope CursorScope, direction CursorDirection) (CursorPosition, error) {
	payload, signature, ok := splitCursor(cursor)
	if c == nil || !scope.valid || !ok || !validCursorDirection(direction) {
		return CursorPosition{}, ErrInvalidCursor
	}
	var decoded cursorPayload
	reader := json.NewDecoder(bytes.NewReader(payload))
	reader.DisallowUnknownFields()
	if err := reader.Decode(&decoded); err != nil {
		return CursorPosition{}, ErrInvalidCursor
	}
	if err := ensureCursorEOF(reader); err != nil {
		return CursorPosition{}, ErrInvalidCursor
	}
	key, exists := c.keys[decoded.KeyID]
	if !exists || !hmac.Equal(signature, cursorSignature(key, payload)) {
		return CursorPosition{}, ErrInvalidCursor
	}
	if decoded.Version != 1 || decoded.Expires <= c.now().Unix() || decoded.Direction != direction || decoded.Position.TieBreaker == "" {
		return CursorPosition{}, ErrInvalidCursor
	}
	if decoded.Collection != scope.collection || decoded.Filters != scope.filters || decoded.Sort != scope.sort ||
		decoded.Tenant != scope.tenant || decoded.Authorization != scope.authorization || decoded.Policy != scope.policy || decoded.Snapshot != scope.snapshot {
		return CursorPosition{}, ErrInvalidCursor
	}
	return decoded.Position, nil
}

func splitCursor(cursor string) ([]byte, []byte, bool) {
	if len(cursor) == 0 || len(cursor) > 8192 || strings.Count(cursor, ".") != 1 {
		return nil, nil, false
	}
	parts := strings.SplitN(cursor, ".", 2)
	payload, payloadErr := base64.RawURLEncoding.DecodeString(parts[0])
	signature, signatureErr := base64.RawURLEncoding.DecodeString(parts[1])
	return payload, signature, payloadErr == nil && signatureErr == nil && len(signature) == sha256.Size
}

func ensureCursorEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return errors.New("cursor payload contains trailing data")
}

func cursorSignature(key, payload []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("naatre:cursor:v1\n"))
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}

func validCursorDirection(direction CursorDirection) bool {
	return direction == CursorForward || direction == CursorBackward
}

type PageEntry[T any] struct {
	Item         T
	Position     CursorPosition
	EdgeMetadata map[string]any
}

type PageRequest struct {
	First             *uint64
	After             string
	Last              *uint64
	Before            string
	Scope             CursorScope
	IncludeTotalCount bool
}

type PageInfo struct {
	HasNextPage     bool   `json:"hasNextPage"`
	HasPreviousPage bool   `json:"hasPreviousPage"`
	StartCursor     string `json:"startCursor,omitempty"`
	EndCursor       string `json:"endCursor,omitempty"`
}

type PageEdge[T any] struct {
	Cursor   string         `json:"cursor"`
	Item     T              `json:"item"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type CollectionPage[T any] struct {
	Items      []T           `json:"items"`
	PageInfo   PageInfo      `json:"pageInfo"`
	TotalCount *uint64       `json:"totalCount,omitempty"`
	Edges      []PageEdge[T] `json:"edges,omitempty"`
}

func Paginate[T any](codec *CursorCodec, entries []PageEntry[T], request PageRequest) (CollectionPage[T], error) {
	direction, size, err := validatePageRequest(codec, request)
	if err != nil {
		return CollectionPage[T]{}, err
	}
	if !strictlyOrdered(entries) {
		return CollectionPage[T]{}, ErrUnstableCollection
	}
	start, end, err := pageEntryBounds(codec, entries, request, direction)
	if err != nil {
		return CollectionPage[T]{}, err
	}
	available := uint64(end - start)
	if size < available {
		if direction == CursorForward {
			end = start + int(size)
		} else {
			start = end - int(size)
		}
	}
	return buildCollectionPage(codec, entries, request, direction, start, end)
}

func validatePageRequest(codec *CursorCodec, request PageRequest) (CursorDirection, uint64, error) {
	if codec == nil || !request.Scope.valid || (request.First == nil) == (request.Last == nil) {
		return "", 0, ErrInvalidPage
	}
	if request.First != nil {
		if request.Before != "" || *request.First == 0 || *request.First > codec.maxPageSize {
			return "", 0, ErrInvalidPage
		}
		return CursorForward, *request.First, nil
	}
	if request.After != "" || *request.Last == 0 || *request.Last > codec.maxPageSize {
		return "", 0, ErrInvalidPage
	}
	return CursorBackward, *request.Last, nil
}

func strictlyOrdered[T any](entries []PageEntry[T]) bool {
	for index := 1; index < len(entries); index++ {
		if compareCursorPosition(entries[index-1].Position, entries[index].Position) >= 0 {
			return false
		}
	}
	return true
}

func compareCursorPosition(left, right CursorPosition) int {
	if compared := strings.Compare(left.SortKey, right.SortKey); compared != 0 {
		return compared
	}
	return strings.Compare(left.TieBreaker, right.TieBreaker)
}

func pageEntryBounds[T any](codec *CursorCodec, entries []PageEntry[T], request PageRequest, direction CursorDirection) (int, int, error) {
	start, end := 0, len(entries)
	if request.After != "" {
		position, err := codec.Decode(request.After, request.Scope, direction)
		if err != nil {
			return 0, 0, err
		}
		start = firstPosition(entries, position, true)
	}
	if request.Before != "" {
		position, err := codec.Decode(request.Before, request.Scope, direction)
		if err != nil {
			return 0, 0, err
		}
		end = firstPosition(entries, position, false)
	}
	return start, end, nil
}

func firstPosition[T any](entries []PageEntry[T], position CursorPosition, after bool) int {
	left, right := 0, len(entries)
	for left < right {
		middle := left + (right-left)/2
		compared := compareCursorPosition(entries[middle].Position, position)
		if compared < 0 || (after && compared == 0) {
			left = middle + 1
		} else {
			right = middle
		}
	}
	return left
}

func buildCollectionPage[T any](codec *CursorCodec, entries []PageEntry[T], request PageRequest, direction CursorDirection, start, end int) (CollectionPage[T], error) {
	page := CollectionPage[T]{Items: make([]T, 0, end-start)}
	page.PageInfo.HasPreviousPage = start > 0
	page.PageInfo.HasNextPage = end < len(entries)
	includeEdges := false
	for _, entry := range entries[start:end] {
		page.Items = append(page.Items, entry.Item)
		includeEdges = includeEdges || entry.EdgeMetadata != nil
	}
	if request.IncludeTotalCount {
		count := uint64(len(entries))
		page.TotalCount = &count
	}
	if start == end {
		return page, nil
	}
	var err error
	page.PageInfo.StartCursor, err = codec.Encode(request.Scope, direction, entries[start].Position)
	if err != nil {
		return CollectionPage[T]{}, err
	}
	page.PageInfo.EndCursor, err = codec.Encode(request.Scope, direction, entries[end-1].Position)
	if err != nil {
		return CollectionPage[T]{}, err
	}
	if includeEdges {
		page.Edges = make([]PageEdge[T], 0, end-start)
		for _, entry := range entries[start:end] {
			cursor, encodeErr := codec.Encode(request.Scope, direction, entry.Position)
			if encodeErr != nil {
				return CollectionPage[T]{}, encodeErr
			}
			page.Edges = append(page.Edges, PageEdge[T]{Cursor: cursor, Item: entry.Item, Metadata: entry.EdgeMetadata})
		}
	}
	return page, nil
}

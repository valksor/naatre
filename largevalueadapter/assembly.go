package largevalueadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/valksor/naatre/largevalue"
	transporthttp "github.com/valksor/naatre/transport/http"
)

const (
	manifestName         = "manifest.json"
	sessionPrefix        = "session-"
	maximumManifestBytes = 1 << 20
)

type AssemblyConfig struct {
	Coordinator    *largevalue.Coordinator
	Root           string
	Now            func() time.Time
	NewID          func() (string, error)
	Limits         Limits
	CleanupTimeout time.Duration
	OrphanTTL      time.Duration
}

type AssemblySession struct {
	ID        string
	Validator string
	Profile   largevalue.TransferProfile
	ExpiresAt time.Time
}

type Chunk struct {
	Number    int
	Offset    int64
	Length    int64
	Validator string
	Digest    string
}

// Assembler stores verified chunks in an application-selected directory. The
// assembly store owns completed chunks until core finalization succeeds or a
// bounded abort/expiry sweep removes them.
type Assembler struct {
	coordinator *largevalue.Coordinator
	root        string
	now         func() time.Time
	newID       func() (string, error)
	limits      Limits
	cleanup     cleanupPolicy
	orphanTTL   time.Duration
	mu          sync.Mutex
	active      map[string]bool
}

type assemblyManifest struct {
	Version              int                        `json:"version"`
	SessionID            string                     `json:"sessionId"`
	RecordID             string                     `json:"recordId"`
	Profile              largevalue.TransferProfile `json:"profile"`
	TotalLength          int64                      `json:"totalLength"`
	RepresentationDigest string                     `json:"representationDigest"`
	Validator            string                     `json:"validator"`
	ExpiresAt            time.Time                  `json:"expiresAt"`
	Parts                []assemblyPart             `json:"parts"`
}

type assemblyPart struct {
	Number int    `json:"number"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	Digest string `json:"digest"`
}

func NewAssembler(config AssemblyConfig) (*Assembler, error) {
	if config.Limits == (Limits{}) {
		config.Limits = DefaultLimits()
	}
	if config.Coordinator == nil || config.Now == nil || config.NewID == nil || !validLimits(config.Limits) ||
		config.CleanupTimeout <= 0 || config.OrphanTTL <= 0 || strings.TrimSpace(config.Root) == "" {
		return nil, publicError(CodeInvalidConfig, "assembly adapter configuration is invalid", nil)
	}
	root, err := filepath.Abs(config.Root)
	if err != nil || root == string(filepath.Separator) {
		return nil, publicError(CodeInvalidConfig, "assembly adapter configuration is invalid", nil)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, publicError(CodeCleanupFailed, "large value assembly storage is unavailable", nil)
	}
	return &Assembler{coordinator: config.Coordinator, root: root, now: config.Now, newID: config.NewID, limits: config.Limits,
		cleanup: cleanupPolicy{timeout: config.CleanupTimeout}, orphanTTL: config.OrphanTTL, active: make(map[string]bool)}, nil
}

// Begin authorizes an upload capability and creates a bearer-free durable
// session manifest for multipart or resumable chunks.
func (a *Assembler) Begin(ctx context.Context, reference string, ttl time.Duration) (AssemblySession, error) {
	if err := contextError(ctx); err != nil {
		return AssemblySession{}, err
	}
	record, err := a.coordinator.Inspect(ctx, reference, largevalue.AuthorizeUpload, "")
	if err != nil || record.Direction != largevalue.Upload || (record.Profile != largevalue.MultipartProfile && record.Profile != largevalue.ResumableProfile) ||
		record.Metadata.Length > a.limits.MaximumTransferBytes || ttl <= 0 {
		return AssemblySession{}, publicError(CodeUnavailable, "large value transfer is unavailable", largevalue.ErrUnavailable)
	}
	if _, err := transporthttp.ParseDigestFields([]string{record.Metadata.RepresentationDigest}); err != nil {
		return AssemblySession{}, publicError(CodeInvalidRequest, "large value transfer request is invalid", nil)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	count, err := a.sessionCount()
	if err != nil {
		return AssemblySession{}, publicError(CodeCleanupFailed, "large value assembly storage is unavailable", nil)
	}
	if count >= a.limits.MaximumSessions {
		return AssemblySession{}, publicError(CodeResourceExhausted, "large value assembly limit is exhausted", nil)
	}
	id, err := a.newID()
	if err != nil || !validIdentifier(id, a.limits.MaximumIdentifier) {
		return AssemblySession{}, publicError(CodeUnavailable, "large value transfer is unavailable", nil)
	}
	validatorID, err := a.newID()
	if err != nil || !validIdentifier(validatorID, a.limits.MaximumIdentifier) {
		return AssemblySession{}, publicError(CodeUnavailable, "large value transfer is unavailable", nil)
	}
	validatorBytes := sha256.Sum256([]byte("naatre:large-value-assembly:v1\n" + validatorID))
	validator := `"` + hex.EncodeToString(validatorBytes[:]) + `"`
	expires := a.now().UTC().Add(ttl)
	if record.ExpiresAt.Before(expires) {
		expires = record.ExpiresAt
	}
	manifest := assemblyManifest{Version: 1, SessionID: id, RecordID: record.ID, Profile: record.Profile,
		TotalLength: record.Metadata.Length, RepresentationDigest: record.Metadata.RepresentationDigest, Validator: validator, ExpiresAt: expires}
	directory := a.sessionPath(id)
	if err := os.Mkdir(directory, 0o700); err != nil {
		return AssemblySession{}, publicError(CodeConflict, "large value assembly session is unavailable", nil)
	}
	if err := a.writeManifest(directory, manifest); err != nil {
		_ = os.RemoveAll(directory)
		return AssemblySession{}, publicError(CodeCleanupFailed, "large value assembly storage is unavailable", nil)
	}
	return AssemblySession{ID: id, Validator: validator, Profile: record.Profile, ExpiresAt: expires}, nil
}

// WriteChunk streams one exact-length, exact-digest chunk to an untrusted
// temporary file. A failed or cancelled write removes that temporary data;
// previously accepted chunks remain store-owned and expiry-bounded.
func (a *Assembler) WriteChunk(ctx context.Context, reference, sessionID string, chunk Chunk, source io.Reader) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if source == nil || !validIdentifier(sessionID, a.limits.MaximumIdentifier) {
		return publicError(CodeInvalidRequest, "large value chunk request is invalid", nil)
	}
	directory, manifest, err := a.prepareChunk(ctx, reference, sessionID, chunk)
	if err != nil {
		return err
	}
	defer a.finishActive(sessionID)
	part := assemblyPart{Number: chunk.Number, Offset: chunk.Offset, Length: chunk.Length, Digest: chunk.Digest}
	if err := a.writeChunkFile(ctx, directory, chunk, source); err != nil {
		return err
	}
	manifest.Parts = append(manifest.Parts, part)
	if err := a.writeManifest(directory, manifest); err != nil {
		_ = os.Remove(filepath.Join(directory, partFilename(chunk.Number)))
		return publicError(CodeTransferFailed, "large value chunk transfer failed", nil)
	}
	return nil
}

func (a *Assembler) prepareChunk(ctx context.Context, reference, sessionID string, chunk Chunk) (string, assemblyManifest, error) {
	record, err := a.coordinator.Inspect(ctx, reference, largevalue.AuthorizeUpload, "")
	if err != nil {
		return "", assemblyManifest{}, publicError(CodeUnavailable, "large value assembly session is unavailable", largevalue.ErrUnavailable)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	directory := a.sessionPath(sessionID)
	manifest, err := a.readManifest(directory)
	if err != nil || manifest.SessionID != sessionID || !manifest.ExpiresAt.After(a.now().UTC()) {
		return "", assemblyManifest{}, publicError(CodeUnavailable, "large value assembly session is unavailable", nil)
	}
	if a.active[sessionID] {
		return "", assemblyManifest{}, publicError(CodeConflict, "large value assembly session is busy", nil)
	}
	if record.ID != manifest.RecordID || record.Profile != manifest.Profile || record.Metadata.Length != manifest.TotalLength ||
		record.Metadata.RepresentationDigest != manifest.RepresentationDigest {
		return "", assemblyManifest{}, publicError(CodeUnavailable, "large value assembly session is unavailable", largevalue.ErrUnavailable)
	}
	if err := a.validateChunk(manifest, chunk); err != nil {
		return "", assemblyManifest{}, err
	}
	a.active[sessionID] = true
	return directory, manifest, nil
}

func (a *Assembler) writeChunkFile(ctx context.Context, directory string, chunk Chunk, source io.Reader) error {
	temporary, err := os.CreateTemp(directory, ".chunk-")
	if err != nil {
		return publicError(CodeTransferFailed, "large value chunk transfer failed", nil)
	}
	temporaryName := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return publicError(CodeTransferFailed, "large value chunk transfer failed", nil)
	}
	_, err = transporthttp.VerifyDigestTo(temporary, &contextReader{ctx: ctx, reader: source}, []string{chunk.Digest}, transporthttp.VerifyOptions{
		MaximumBytes: a.limits.MaximumChunkBytes, ExpectedLength: chunk.Length,
	})
	if err != nil {
		return transferError(ctx, largevalue.ErrTransferFailed)
	}
	if err := temporary.Sync(); err != nil {
		return publicError(CodeTransferFailed, "large value chunk transfer failed", nil)
	}
	if err := temporary.Close(); err != nil {
		return publicError(CodeTransferFailed, "large value chunk transfer failed", nil)
	}
	finalName := filepath.Join(directory, partFilename(chunk.Number))
	if err := os.Rename(temporaryName, finalName); err != nil {
		return publicError(CodeConflict, "large value chunk conflicts with existing assembly data", nil)
	}
	keep = true
	return nil
}

func (a *Assembler) validateChunk(manifest assemblyManifest, chunk Chunk) error {
	if chunk.Number <= 0 || chunk.Number > a.limits.MaximumParts || chunk.Offset < 0 || chunk.Length <= 0 ||
		chunk.Length > a.limits.MaximumChunkBytes || chunk.Offset > manifest.TotalLength-chunk.Length ||
		chunk.Validator != manifest.Validator || len(manifest.Parts) >= a.limits.MaximumParts {
		return publicError(CodeInvalidRequest, "large value chunk request is invalid", nil)
	}
	if _, err := transporthttp.ParseDigestFields([]string{chunk.Digest}); err != nil {
		return publicError(CodeInvalidRequest, "large value chunk request is invalid", nil)
	}
	for _, existing := range manifest.Parts {
		if existing.Number == chunk.Number || rangesOverlap(existing.Offset, existing.Length, chunk.Offset, chunk.Length) {
			return publicError(CodeConflict, "large value chunk conflicts with existing assembly data", nil)
		}
	}
	return nil
}

// Complete requires a gap-free assembly, streams its files in offset order
// through the core final digest gate, and then deterministically deletes all
// assembly data regardless of the finalization outcome.
func (a *Assembler) Complete(ctx context.Context, reference, sessionID string) (err error) {
	if contextErr := contextError(ctx); contextErr != nil {
		return contextErr
	}
	if !validIdentifier(sessionID, a.limits.MaximumIdentifier) {
		return publicError(CodeInvalidRequest, "large value assembly request is invalid", nil)
	}
	directory, parts, err := a.prepareCompletion(ctx, reference, sessionID)
	if err != nil {
		return err
	}
	reader := &assemblyReader{paths: make([]string, len(parts))}
	for index, part := range parts {
		reader.paths[index] = filepath.Join(directory, partFilename(part.Number))
	}
	defer func() {
		cleanup, cancel := a.cleanup.context(ctx)
		defer cancel()
		if cleanupErr := removeDirectory(cleanup, directory); cleanupErr != nil && err == nil {
			err = publicError(CodeCleanupFailed, "large value cleanup failed", nil)
		}
		a.finishActive(sessionID)
	}()
	if err := a.verifyParts(ctx, directory, parts); err != nil {
		return err
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil && err == nil {
			err = publicError(CodeCleanupFailed, "large value cleanup failed", nil)
		}
	}()
	if err := a.coordinator.AcceptUpload(ctx, reference, reader); err != nil {
		return transferError(ctx, err)
	}
	return nil
}

func (a *Assembler) prepareCompletion(ctx context.Context, reference, sessionID string) (string, []assemblyPart, error) {
	record, err := a.coordinator.Inspect(ctx, reference, largevalue.AuthorizeUpload, "")
	if err != nil {
		return "", nil, publicError(CodeUnavailable, "large value assembly session is unavailable", largevalue.ErrUnavailable)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	directory := a.sessionPath(sessionID)
	manifest, err := a.readManifest(directory)
	if err != nil || manifest.SessionID != sessionID || !manifest.ExpiresAt.After(a.now().UTC()) {
		return "", nil, publicError(CodeUnavailable, "large value assembly session is unavailable", nil)
	}
	if a.active[sessionID] {
		return "", nil, publicError(CodeConflict, "large value assembly session is busy", nil)
	}
	if record.ID != manifest.RecordID || record.Profile != manifest.Profile || record.Metadata.Length != manifest.TotalLength ||
		record.Metadata.RepresentationDigest != manifest.RepresentationDigest {
		return "", nil, publicError(CodeUnavailable, "large value assembly session is unavailable", largevalue.ErrUnavailable)
	}
	parts := sortedParts(manifest.Parts)
	if err := validateCompleteAssembly(manifest, parts); err != nil {
		return "", nil, err
	}
	a.active[sessionID] = true
	return directory, parts, nil
}

func sortedParts(parts []assemblyPart) []assemblyPart {
	result := slices.Clone(parts)
	slices.SortFunc(result, func(left, right assemblyPart) int {
		if left.Offset < right.Offset {
			return -1
		}
		if left.Offset > right.Offset {
			return 1
		}
		return left.Number - right.Number
	})
	return result
}

func (a *Assembler) verifyParts(ctx context.Context, directory string, parts []assemblyPart) error {
	for _, part := range parts {
		file, err := os.Open(filepath.Join(directory, partFilename(part.Number)))
		if err != nil {
			return publicError(CodeIntegrityFailed, "large value assembly integrity verification failed", nil)
		}
		_, verifyErr := transporthttp.VerifyDigestTo(io.Discard, &contextReader{ctx: ctx, reader: file}, []string{part.Digest}, transporthttp.VerifyOptions{
			MaximumBytes: a.limits.MaximumChunkBytes, ExpectedLength: part.Length,
		})
		closeErr := file.Close()
		if verifyErr != nil || closeErr != nil {
			return transferError(ctx, largevalue.ErrTransferFailed)
		}
	}
	return nil
}

// Abort removes an assembly under a fresh bounded context. Authorization is
// rechecked using the principal retained by context.WithoutCancel.
func (a *Assembler) Abort(ctx context.Context, reference, sessionID string) error {
	if !validIdentifier(sessionID, a.limits.MaximumIdentifier) {
		return publicError(CodeInvalidRequest, "large value assembly request is invalid", nil)
	}
	cleanup, cancel := a.cleanup.context(ctx)
	defer cancel()
	record, err := a.coordinator.Inspect(cleanup, reference, largevalue.AuthorizeUpload, "")
	if err != nil {
		return publicError(CodeUnavailable, "large value assembly session is unavailable", largevalue.ErrUnavailable)
	}
	a.mu.Lock()
	manifest, err := a.readManifest(a.sessionPath(sessionID))
	if err != nil || manifest.SessionID != sessionID {
		a.mu.Unlock()
		return publicError(CodeUnavailable, "large value assembly session is unavailable", nil)
	}
	if a.active[sessionID] {
		a.mu.Unlock()
		return publicError(CodeConflict, "large value assembly session is busy", nil)
	}
	if record.ID != manifest.RecordID {
		a.mu.Unlock()
		return publicError(CodeUnavailable, "large value assembly session is unavailable", largevalue.ErrUnavailable)
	}
	a.active[sessionID] = true
	a.mu.Unlock()
	defer a.finishActive(sessionID)
	if err := removeDirectory(cleanup, a.sessionPath(sessionID)); err != nil {
		return publicError(CodeCleanupFailed, "large value cleanup failed", nil)
	}
	return nil
}

// SweepExpired removes a deterministic bounded page of expired manifests and
// old malformed/crash-orphaned session directories.
func (a *Assembler) SweepExpired(ctx context.Context, limit int) (int, error) {
	if err := contextError(ctx); err != nil {
		return 0, err
	}
	if limit <= 0 || limit > a.limits.MaximumCleanupBatch {
		return 0, publicError(CodeInvalidRequest, "large value cleanup request is invalid", nil)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	entries, err := os.ReadDir(a.root)
	if err != nil {
		return 0, publicError(CodeCleanupFailed, "large value cleanup failed", nil)
	}
	now := a.now().UTC()
	removed := 0
	for _, entry := range entries {
		if removed == limit || !strings.HasPrefix(entry.Name(), sessionPrefix) {
			continue
		}
		if err := contextError(ctx); err != nil {
			return removed, err
		}
		directory := filepath.Join(a.root, entry.Name())
		manifest, manifestErr := a.readManifest(directory)
		if manifestErr == nil && a.active[manifest.SessionID] {
			continue
		}
		expired := manifestErr == nil && !manifest.ExpiresAt.After(now)
		if manifestErr != nil {
			info, infoErr := entry.Info()
			expired = infoErr == nil && !info.ModTime().Add(a.orphanTTL).After(now)
		}
		if expired {
			if err := removeDirectory(ctx, directory); err != nil {
				return removed, publicError(CodeCleanupFailed, "large value cleanup failed", nil)
			}
			removed++
		}
	}
	return removed, nil
}

func validateCompleteAssembly(manifest assemblyManifest, parts []assemblyPart) error {
	if len(parts) == 0 {
		return publicError(CodeConflict, "large value assembly is incomplete", nil)
	}
	nextOffset := int64(0)
	for index, part := range parts {
		if part.Offset != nextOffset || (manifest.Profile == largevalue.MultipartProfile && part.Number != index+1) {
			return publicError(CodeConflict, "large value assembly is incomplete", nil)
		}
		nextOffset += part.Length
	}
	if nextOffset != manifest.TotalLength {
		return publicError(CodeConflict, "large value assembly is incomplete", nil)
	}
	return nil
}

func (a *Assembler) sessionCount() (int, error) {
	entries, err := os.ReadDir(a.root)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), sessionPrefix) {
			count++
		}
	}
	return count, nil
}

func (a *Assembler) sessionPath(id string) string {
	digest := sha256.Sum256([]byte(id))
	return filepath.Join(a.root, sessionPrefix+hex.EncodeToString(digest[:]))
}

func (a *Assembler) readManifest(directory string) (assemblyManifest, error) {
	file, err := os.Open(filepath.Join(directory, manifestName))
	if err != nil {
		return assemblyManifest{}, err
	}
	defer func() {
		_ = file.Close()
	}()
	info, err := file.Stat()
	if err != nil || info.Size() > maximumManifestBytes {
		return assemblyManifest{}, errors.New("invalid manifest")
	}
	decoder := json.NewDecoder(io.LimitReader(file, maximumManifestBytes+1))
	decoder.DisallowUnknownFields()
	var manifest assemblyManifest
	if err := decoder.Decode(&manifest); err != nil || manifest.Version != 1 || manifest.SessionID == "" || manifest.RecordID == "" ||
		manifest.TotalLength < 0 || manifest.Validator == "" || manifest.RepresentationDigest == "" || manifest.ExpiresAt.IsZero() || len(manifest.Parts) > a.limits.MaximumParts {
		return assemblyManifest{}, errors.New("invalid manifest")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return assemblyManifest{}, errors.New("trailing manifest data")
	}
	return manifest, nil
}

func (a *Assembler) finishActive(sessionID string) {
	a.mu.Lock()
	delete(a.active, sessionID)
	a.mu.Unlock()
}

func (a *Assembler) writeManifest(directory string, manifest assemblyManifest) error {
	temporary, err := os.CreateTemp(directory, ".manifest-")
	if err != nil {
		return err
	}
	name := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(name)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(manifest); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(directory, manifestName)); err != nil {
		return err
	}
	keep = true
	return nil
}

func partFilename(number int) string {
	return "part-" + hex.EncodeToString([]byte{byte(number >> 24), byte(number >> 16), byte(number >> 8), byte(number)})
}

func rangesOverlap(leftOffset, leftLength, rightOffset, rightLength int64) bool {
	return leftOffset < rightOffset+rightLength && rightOffset < leftOffset+leftLength
}

func removeDirectory(ctx context.Context, directory string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.RemoveAll(directory)
}

type assemblyReader struct {
	paths   []string
	index   int
	current *os.File
}

func (r *assemblyReader) Read(buffer []byte) (int, error) {
	for {
		if r.current == nil {
			if r.index >= len(r.paths) {
				return 0, io.EOF
			}
			file, err := os.Open(r.paths[r.index])
			if err != nil {
				return 0, err
			}
			r.current = file
			r.index++
		}
		n, err := r.current.Read(buffer)
		if err == io.EOF {
			closeErr := r.current.Close()
			r.current = nil
			if n > 0 {
				return n, nil
			}
			if closeErr != nil {
				return 0, closeErr
			}
			continue
		}
		return n, err
	}
}

func (r *assemblyReader) Close() error {
	if r.current == nil {
		return nil
	}
	err := r.current.Close()
	r.current = nil
	return err
}

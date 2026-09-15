package largevalueadapter

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valksor/naatre/largevalue"
	transporthttp "github.com/valksor/naatre/transport/http"
)

func TestAssemblerStreamsOutOfOrderMultipartAndCleansAfterFinalization(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	coordinator := testCoordinator(t, store, &now)
	root := t.TempDir()
	assembler := testAssembler(t, coordinator, root, &now, DefaultLimits())
	content := bytes.Repeat([]byte("multipart-stream-"), 8000)
	capability := issueCapability(t, coordinator, largevalue.Upload, largevalue.MultipartProfile, content, nil)
	session, err := assembler.Begin(testPrincipalContext(), capability.Reference, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	boundaries := [][2]int{{0, 45000}, {45000, 90000}, {90000, len(content)}}
	for _, index := range []int{1, 0, 2} {
		start, end := boundaries[index][0], boundaries[index][1]
		part := content[start:end]
		digest, _ := transporthttp.FormatDigestField(part)
		err := assembler.WriteChunk(testPrincipalContext(), capability.Reference, session.ID, Chunk{
			Number: index + 1, Offset: int64(start), Length: int64(len(part)), Validator: session.Validator, Digest: digest,
		}, &limitedReader{reader: bytes.NewReader(part), maximum: 47})
		if err != nil {
			t.Fatalf("part %d: %v", index+1, err)
		}
	}
	if err := assembler.Complete(testPrincipalContext(), capability.Reference, session.ID); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 || !bytes.Equal(store.content["object-1"], content) || store.stages["object-1"].maximumWrite > 32*1024 {
		t.Fatalf("multipart finalization left entries=%d or buffered content", len(entries))
	}
}

func TestAssemblerRejectsGapOverlapValidatorDigestAndCleansCancelledChunk(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	coordinator := testCoordinator(t, store, &now)
	root := t.TempDir()
	assembler := testAssembler(t, coordinator, root, &now, DefaultLimits())
	content := bytes.Repeat([]byte("resumable-"), 9000)
	capability := issueCapability(t, coordinator, largevalue.Upload, largevalue.ResumableProfile, content, nil)
	session, err := assembler.Begin(testPrincipalContext(), capability.Reference, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	first := content[:30000]
	firstDigest, _ := transporthttp.FormatDigestField(first)
	if err := assembler.WriteChunk(testPrincipalContext(), capability.Reference, session.ID, Chunk{Number: 1, Offset: 0, Length: int64(len(first)), Validator: session.Validator, Digest: firstDigest}, bytes.NewReader(first)); err != nil {
		t.Fatal(err)
	}
	if err := assembler.WriteChunk(testPrincipalContext(), capability.Reference, session.ID, Chunk{Number: 2, Offset: 20000, Length: 10000, Validator: session.Validator, Digest: firstDigest}, bytes.NewReader(first[:10000])); ErrorCode(err) != CodeConflict {
		t.Fatalf("overlap = %v", err)
	}
	second := content[40000:]
	secondDigest, _ := transporthttp.FormatDigestField(second)
	if err := assembler.WriteChunk(testPrincipalContext(), capability.Reference, session.ID, Chunk{Number: 2, Offset: 40000, Length: int64(len(second)), Validator: session.Validator, Digest: secondDigest}, bytes.NewReader(second)); err != nil {
		t.Fatal(err)
	}
	if err := assembler.Complete(testPrincipalContext(), capability.Reference, session.ID); ErrorCode(err) != CodeConflict {
		t.Fatalf("gap completion = %v", err)
	}

	gapped := content[30000:40000]
	gapDigest, _ := transporthttp.FormatDigestField(gapped)
	if err := assembler.WriteChunk(testPrincipalContext(), capability.Reference, session.ID, Chunk{Number: 3, Offset: 30000, Length: int64(len(gapped)), Validator: "wrong", Digest: gapDigest}, bytes.NewReader(gapped)); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("validator change = %v", err)
	}
	ctx, cancel := context.WithCancel(testPrincipalContext())
	cancelSource := &cancellingReader{cancel: cancel}
	if err := assembler.WriteChunk(ctx, capability.Reference, session.ID, Chunk{Number: 3, Offset: 30000, Length: int64(len(gapped)), Validator: session.Validator, Digest: gapDigest}, cancelSource); ErrorCode(err) != CodeCancelled || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled chunk = %v", err)
	}
	directory := assembler.sessionPath(session.ID)
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".chunk-") {
			t.Fatalf("cancelled temporary chunk remains: %s", entry.Name())
		}
	}
	badDigest := "sha-256=:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=:"
	if err := assembler.WriteChunk(testPrincipalContext(), capability.Reference, session.ID, Chunk{Number: 3, Offset: 30000, Length: int64(len(gapped)), Validator: session.Validator, Digest: badDigest}, bytes.NewReader(gapped)); ErrorCode(err) != CodeIntegrityFailed {
		t.Fatalf("digest mismatch = %v", err)
	}
}

func TestAssemblerBoundsSessionsAndSweepsExpiredAndCrashOrphanData(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	coordinator := testCoordinator(t, store, &now)
	root := t.TempDir()
	limits := DefaultLimits()
	limits.MaximumSessions = 1
	assembler := testAssembler(t, coordinator, root, &now, limits)
	content := bytes.Repeat([]byte("x"), 70<<10)
	firstCapability := issueCapability(t, coordinator, largevalue.Upload, largevalue.ResumableProfile, content, nil)
	first, err := assembler.Begin(testPrincipalContext(), firstCapability.Reference, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	secondCapability := issueCapability(t, coordinator, largevalue.Upload, largevalue.ResumableProfile, content, nil)
	if _, err := assembler.Begin(testPrincipalContext(), secondCapability.Reference, time.Minute); ErrorCode(err) != CodeResourceExhausted {
		t.Fatalf("session limit = %v", err)
	}
	now = now.Add(2 * time.Minute)
	removed, err := assembler.SweepExpired(testPrincipalContext(), 1)
	if err != nil || removed != 1 {
		t.Fatalf("expired sweep = %d, %v", removed, err)
	}
	if _, err := os.Stat(assembler.sessionPath(first.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired session remains: %v", err)
	}

	orphan := filepath.Join(root, sessionPrefix+strings.Repeat("a", 64))
	if err := os.Mkdir(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	removed, err = assembler.SweepExpired(testPrincipalContext(), 1)
	if err != nil || removed != 1 {
		t.Fatalf("orphan sweep = %d, %v", removed, err)
	}
}

func testAssembler(t *testing.T, coordinator *largevalue.Coordinator, root string, now *time.Time, limits Limits) *Assembler {
	t.Helper()
	next := 0
	assembler, err := NewAssembler(AssemblyConfig{Coordinator: coordinator, Root: root, Now: func() time.Time { return *now },
		NewID: func() (string, error) { next++; return "assembly-id-" + string(rune('a'+next)), nil }, Limits: limits,
		CleanupTimeout: time.Second, OrphanTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return assembler
}

type cancellingReader struct {
	cancel context.CancelFunc
	done   bool
}

func (r *cancellingReader) Read([]byte) (int, error) {
	if !r.done {
		r.done = true
		r.cancel()
	}
	return 0, context.Canceled
}

var _ io.Reader = (*cancellingReader)(nil)

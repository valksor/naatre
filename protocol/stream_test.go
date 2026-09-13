package protocol_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/valksor/naatre/protocol"
)

func TestStreamReceiverAppliesDeterministicPatchesAndErrors(t *testing.T) {
	t.Parallel()
	receiver, err := protocol.NewStreamReceiver("stream-safe", protocol.DefaultStreamLimits())
	if err != nil {
		t.Fatal(err)
	}
	frames := []protocol.StreamFrame{
		{Type: protocol.StreamOpen, Stream: "stream-safe", Sequence: 1, SchemaRevision: "schema-r1"},
		{Type: protocol.StreamData, Stream: "stream-safe", Sequence: 2, Position: 1, HasPosition: true, Data: json.RawMessage(`{"items":[{"name":"first"}]}`)},
		{Type: protocol.StreamPatch, Stream: "stream-safe", Sequence: 3, Position: 2, HasPosition: true, Path: []any{"items", uint64(0), "name"}, Data: json.RawMessage(`"updated"`)},
		{Type: protocol.StreamError, Stream: "stream-safe", Sequence: 4, Path: []any{"items", uint64(0)}, Error: &protocol.StreamFrameError{Code: "ITEM_FAILED", Message: "item refresh failed"}},
	}
	for _, frame := range frames {
		accepted, err := receiver.Accept(frame)
		if err != nil || accepted.Duplicate {
			t.Fatalf("accept %#v = %#v, %v", frame, accepted, err)
		}
	}
	duplicate, err := receiver.Accept(frames[len(frames)-1])
	if err != nil || !duplicate.Duplicate {
		t.Fatalf("exact duplicate = %#v, %v", duplicate, err)
	}
	if _, err := receiver.Accept(protocol.StreamFrame{Type: protocol.StreamPatch, Stream: "stream-safe", Sequence: 6, Position: 3, HasPosition: true, Path: []any{"items"}, Data: json.RawMessage(`[]`)}); !errors.Is(err, protocol.ErrStreamSequenceGap) {
		t.Fatalf("gap error = %v", err)
	}
	if _, err := receiver.Accept(protocol.StreamFrame{Type: protocol.StreamComplete, Stream: "stream-safe", Sequence: 5}); err != nil {
		t.Fatal(err)
	}
	if err := receiver.Finish(); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(receiver.Snapshot(), &result); err != nil {
		t.Fatal(err)
	}
	items := result["items"].([]any)
	if got := items[0].(map[string]any)["name"]; got != "updated" {
		t.Fatalf("patched name = %#v", got)
	}
	if got := receiver.Errors(); len(got) != 1 || got[0].Code != "ITEM_FAILED" {
		t.Fatalf("stream errors = %#v", got)
	}
	if _, err := receiver.Accept(protocol.StreamFrame{Type: protocol.StreamKeepalive, Stream: "stream-safe"}); !errors.Is(err, protocol.ErrStreamClosed) {
		t.Fatalf("post-terminal error = %v", err)
	}
}

func TestStreamReceiverRejectsConflictsPatchBeforeParentAndTruncation(t *testing.T) {
	t.Parallel()
	receiver, err := protocol.NewStreamReceiver("stream-safe", protocol.DefaultStreamLimits())
	if err != nil {
		t.Fatal(err)
	}
	open := protocol.StreamFrame{Type: protocol.StreamOpen, Stream: "stream-safe", Sequence: 1, SchemaRevision: "schema-r1"}
	if _, err := receiver.Accept(open); err != nil {
		t.Fatal(err)
	}
	conflict := open
	conflict.SchemaRevision = "schema-r2"
	if _, err := receiver.Accept(conflict); !errors.Is(err, protocol.ErrStreamDuplicateConflict) {
		t.Fatalf("duplicate conflict error = %v", err)
	}
	if _, err := receiver.Accept(protocol.StreamFrame{Type: protocol.StreamPatch, Stream: "stream-safe", Sequence: 2, Position: 1, HasPosition: true, Path: []any{"missing", "child"}, Data: json.RawMessage(`true`)}); !errors.Is(err, protocol.ErrStreamPatchOrder) {
		t.Fatalf("patch-before-parent error = %v", err)
	}
	if err := receiver.Finish(); !errors.Is(err, protocol.ErrStreamTruncated) {
		t.Fatalf("unterminated stream error = %v", err)
	}
}

func TestStreamReceiverRequiresByteEquivalentDuplicates(t *testing.T) {
	t.Parallel()
	receiver, err := protocol.NewStreamReceiver("stream-safe", protocol.DefaultStreamLimits())
	if err != nil {
		t.Fatal(err)
	}
	frames := []protocol.StreamFrame{
		{Type: protocol.StreamOpen, Stream: "stream-safe", Sequence: 1, SchemaRevision: "schema-r1"},
		{Type: protocol.StreamData, Stream: "stream-safe", Sequence: 2, Data: json.RawMessage(`{"first":1,"second":2}`)},
	}
	for _, frame := range frames {
		if _, err := receiver.Accept(frame); err != nil {
			t.Fatal(err)
		}
	}
	semanticallyEqual := frames[1]
	semanticallyEqual.Data = json.RawMessage(`{"second":2,"first":1}`)
	if _, err := receiver.Accept(semanticallyEqual); !errors.Is(err, protocol.ErrStreamDuplicateConflict) {
		t.Fatalf("non-byte-equivalent duplicate error = %v", err)
	}
}

func TestStreamFrameCodecIsStrictAndRejectsControlInjection(t *testing.T) {
	t.Parallel()
	limits := protocol.DefaultStreamLimits()
	valid := protocol.StreamFrame{Type: protocol.StreamData, Stream: "opaque-stream", Sequence: 8, Position: 13, HasPosition: true, EventID: "application-event", Data: json.RawMessage(`{"text":"tēriņš"}`)}
	encoded, err := protocol.MarshalStreamFrame(valid, limits)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := protocol.DecodeStreamFrame(encoded, limits)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Stream != valid.Stream || decoded.Sequence != valid.Sequence || decoded.Position != valid.Position || !decoded.HasPosition || string(decoded.Data) != string(valid.Data) {
		t.Fatalf("decoded frame = %#v", decoded)
	}

	invalid := [][]byte{
		[]byte(`{"type":"data","type":"complete","stream":"s","sequence":1,"data":true}`),
		[]byte(`{"type":"data","stream":"s","sequence":1,"data":true,"unknown":true}`),
		[]byte("{\"type\":\"data\",\"stream\":\"forged\\nstream\",\"sequence\":1,\"data\":true}"),
		[]byte(`{"type":"keepalive","stream":"s","sequence":1}`),
	}
	for _, input := range invalid {
		if _, err := protocol.DecodeStreamFrame(input, limits); err == nil {
			t.Fatalf("invalid frame accepted: %s", input)
		}
	}
}

func TestStreamFramePositionAndTerminalRules(t *testing.T) {
	t.Parallel()
	limits := protocol.DefaultStreamLimits()
	cases := []protocol.StreamFrame{
		{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"},
		{Type: protocol.StreamData, Stream: "s", Sequence: 1, Data: json.RawMessage(`true`)},
		{Type: protocol.StreamPatch, Stream: "s", Sequence: 1, Position: 1, HasPosition: true, Path: []any{"field"}, Data: json.RawMessage(`true`)},
		{Type: protocol.StreamError, Stream: "s", Sequence: 1, Final: true, Error: &protocol.StreamFrameError{Code: "INTERNAL", Message: "failed"}},
		{Type: protocol.StreamComplete, Stream: "s", Sequence: 1},
		{Type: protocol.StreamKeepalive, Stream: "s"},
		{Type: protocol.StreamResume, Stream: "s", Sequence: 1, Cursor: "opaque-cursor"},
		{Type: protocol.StreamHistoryUnavailable, Stream: "s", Sequence: 1, Recovery: protocol.StreamRecoveryRefetch},
	}
	for _, frame := range cases {
		if err := frame.Validate(limits); err != nil {
			t.Errorf("valid %q frame: %v", frame.Type, err)
		}
	}
	invalid := cases[1]
	invalid.HasPosition = true
	invalid.Position = 0
	if err := invalid.Validate(limits); err == nil {
		t.Fatal("zero application position accepted")
	}
	invalid = cases[3]
	invalid.Final = false
	invalid.Path = nil
	if err := invalid.Validate(limits); err == nil {
		t.Fatal("pathless non-terminal error accepted")
	}
	invalid = cases[1]
	invalid.Cursor = "cursor-without-position"
	if err := invalid.Validate(limits); err == nil {
		t.Fatal("cursor without replay position accepted")
	}
	invalid = cases[4]
	invalid.SchemaRevision = "schema-r1"
	if err := invalid.Validate(limits); err == nil {
		t.Fatal("schema revision on complete accepted")
	}
	invalid = cases[1]
	invalid.Path = []any{"field"}
	if err := invalid.Validate(limits); err == nil {
		t.Fatal("pathful data frame accepted")
	}
	invalid = cases[1]
	invalid.Stream = "s\u0085forged"
	if err := invalid.Validate(limits); err == nil {
		t.Fatal("Unicode control in stream identifier accepted")
	}
	invalid = cases[3]
	invalid.Error.Message = string(make([]byte, limits.MaxErrorMessageBytes+1))
	if err := invalid.Validate(limits); err == nil {
		t.Fatal("oversized error message accepted")
	}
}

func TestStreamReceiverRejectsNonMonotonicReplayPosition(t *testing.T) {
	t.Parallel()
	receiver, err := protocol.NewStreamReceiver("s", protocol.DefaultStreamLimits())
	if err != nil {
		t.Fatal(err)
	}
	frames := []protocol.StreamFrame{
		{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"},
		{Type: protocol.StreamData, Stream: "s", Sequence: 2, Position: 2, HasPosition: true, Data: json.RawMessage(`true`)},
		{Type: protocol.StreamPatch, Stream: "s", Sequence: 3, Position: 1, HasPosition: true, Path: []any{"field"}, Data: json.RawMessage(`true`)},
	}
	for _, frame := range frames[:2] {
		if _, err := receiver.Accept(frame); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := receiver.Accept(frames[2]); !errors.Is(err, protocol.ErrStreamPositionOrder) {
		t.Fatalf("position order error = %v", err)
	}
}

func TestStreamReceiverBoundsAggregateStateIssuesAndDuplicateWindow(t *testing.T) {
	t.Parallel()
	limits := protocol.DefaultStreamLimits()
	limits.MaxStateBytes = 4
	limits.MaxIssues = 1
	limits.MaxTrackedSequences = 2
	receiver, err := protocol.NewStreamReceiver("s", limits)
	if err != nil {
		t.Fatal(err)
	}
	frames := []protocol.StreamFrame{
		{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"},
		{Type: protocol.StreamData, Stream: "s", Sequence: 2, Data: json.RawMessage(`true`)},
		{Type: protocol.StreamError, Stream: "s", Sequence: 3, Path: []any{"first"}, Error: &protocol.StreamFrameError{Code: "FAILED", Message: "first"}},
	}
	for _, frame := range frames {
		if _, err := receiver.Accept(frame); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := receiver.Accept(protocol.StreamFrame{Type: protocol.StreamError, Stream: "s", Sequence: 4, Path: []any{"second"}, Error: &protocol.StreamFrameError{Code: "FAILED", Message: "second"}}); !errors.Is(err, protocol.ErrStreamLimit) {
		t.Fatalf("issue limit error = %v", err)
	}
	if _, err := receiver.Accept(protocol.StreamFrame{Type: protocol.StreamPatch, Stream: "s", Sequence: 4, Path: []any{"field"}, Position: 1, HasPosition: true, Data: json.RawMessage(`false`)}); !errors.Is(err, protocol.ErrStreamLimit) {
		t.Fatalf("state limit error = %v", err)
	}
	if _, err := receiver.Accept(frames[0]); !errors.Is(err, protocol.ErrStreamDuplicateExpired) {
		t.Fatalf("expired duplicate-window error = %v", err)
	}
}

func TestStreamReceiverBoundsEncodedAssembledState(t *testing.T) {
	t.Parallel()
	limits := protocol.DefaultStreamLimits()
	limits.MaxStateBytes = 9
	receiver, err := protocol.NewStreamReceiver("s", limits)
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range []protocol.StreamFrame{
		{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"},
		{Type: protocol.StreamData, Stream: "s", Sequence: 2, Data: json.RawMessage(`{}`)},
	} {
		if _, err := receiver.Accept(frame); err != nil {
			t.Fatal(err)
		}
	}
	patch := protocol.StreamFrame{
		Type: protocol.StreamPatch, Stream: "s", Sequence: 3, Position: 1, HasPosition: true,
		Path: []any{"long"}, Data: json.RawMessage(`0`),
	}
	if _, err := receiver.Accept(patch); !errors.Is(err, protocol.ErrStreamLimit) {
		t.Fatalf("assembled state limit error = %v", err)
	}
	if got := string(receiver.Snapshot()); got != `{}` {
		t.Fatalf("rejected patch mutated state: %s", got)
	}
}

func TestStreamProfileNegotiationRejectsVersionSkew(t *testing.T) {
	t.Parallel()
	if err := protocol.NegotiateStreamProfile(protocol.StreamProfileVersion); err != nil {
		t.Fatal(err)
	}
	if err := protocol.NegotiateStreamProfile("2"); !errors.Is(err, protocol.ErrUnsupportedStreamVersion) {
		t.Fatalf("version skew error = %v", err)
	}
}

func TestStreamSourceAdvertisementIsStrictAndPortable(t *testing.T) {
	t.Parallel()
	advertisement := protocol.StreamSourceAdvertisement{
		ProfileVersion: protocol.StreamProfileVersion, Replay: protocol.StreamReplayBounded,
		Consistency: protocol.StreamSnapshotStable, RetentionPolicy: "process-memory",
		MaxReplayEvents: 128, MaxReplayBytes: 1 << 20, DisclosesEarliestPosition: true,
		HistoryRecovery: protocol.StreamRecoveryRefetch,
	}
	encoded, err := protocol.MarshalStreamSourceAdvertisement(advertisement, protocol.DefaultStreamLimits())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := protocol.DecodeStreamSourceAdvertisement(encoded, protocol.DefaultStreamLimits())
	if err != nil || decoded != advertisement {
		t.Fatalf("advertisement round trip = %#v/%v", decoded, err)
	}
	invalid := [][]byte{
		[]byte(`{"profileVersion":"1","replay":"bounded","consistency":"snapshot-stable","retentionPolicy":"process-memory","maxReplayEvents":0,"maxReplayBytes":1024,"disclosesEarliestPosition":true,"historyRecovery":"refetch"}`),
		[]byte(`{"profileVersion":"1","replay":"none","consistency":"unknown","retentionPolicy":"none","maxReplayEvents":0,"maxReplayBytes":0,"disclosesEarliestPosition":false,"historyRecovery":"restart"}`),
		append(encoded[:len(encoded)-1], []byte(`,"unknown":true}`)...),
	}
	for _, input := range invalid {
		if _, err := protocol.DecodeStreamSourceAdvertisement(input, protocol.DefaultStreamLimits()); !errors.Is(err, protocol.ErrInvalidStreamAdvertisement) {
			t.Fatalf("invalid advertisement accepted: %s (%v)", input, err)
		}
	}
}

func TestStreamReceiverExposesFinalTerminalAndSeparateHistoryRecovery(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		terminal protocol.StreamFrame
		wantType protocol.StreamEventType
		wantCode string
		recovery bool
	}{
		{
			name: "final-error",
			terminal: protocol.StreamFrame{Type: protocol.StreamError, Stream: "s", Sequence: 2, Final: true,
				Path: []any{"user"}, Error: &protocol.StreamFrameError{Code: "FAILED", Message: "failed"}},
			wantType: protocol.StreamError, wantCode: "FAILED",
		},
		{
			name: "history-unavailable",
			terminal: protocol.StreamFrame{Type: protocol.StreamHistoryUnavailable, Stream: "s", Sequence: 2,
				Recovery: protocol.StreamRecoveryRefetch},
			wantType: protocol.StreamHistoryUnavailable,
			recovery: true,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			receiver, err := protocol.NewStreamReceiver("s", protocol.DefaultStreamLimits())
			if err != nil {
				t.Fatal(err)
			}
			for _, frame := range []protocol.StreamFrame{
				{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"}, test.terminal,
			} {
				if _, err := receiver.Accept(frame); err != nil {
					t.Fatal(err)
				}
			}
			if err := receiver.Finish(); err != nil {
				t.Fatal(err)
			}
			outcome, ok := receiver.Terminal()
			if test.recovery {
				if ok {
					t.Fatal("history recovery reported as logical terminal")
				}
				outcome, ok = receiver.Recovery()
			}
			if !ok || outcome.Type != test.wantType || test.wantCode != "" && (outcome.Error == nil || outcome.Error.Code != test.wantCode) {
				t.Fatalf("terminal outcome = %#v, %t", outcome, ok)
			}
			if test.wantCode != "" && !reflect.DeepEqual(outcome.Path, []any{"user"}) {
				t.Fatalf("terminal path = %#v", outcome.Path)
			}
		})
	}
}

func TestStreamReceiverAcceptsValidatedUnsignedPathTypes(t *testing.T) {
	t.Parallel()
	for _, index := range []any{uint8(0), uint16(0), uint32(0), uint64(0), uint(0)} {
		receiver, err := protocol.NewStreamReceiver("s", protocol.DefaultStreamLimits())
		if err != nil {
			t.Fatal(err)
		}
		frames := []protocol.StreamFrame{
			{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"},
			{Type: protocol.StreamData, Stream: "s", Sequence: 2, Data: json.RawMessage(`[{"value":false}]`)},
			{Type: protocol.StreamPatch, Stream: "s", Sequence: 3, Position: 1, HasPosition: true, Path: []any{index, "value"}, Data: json.RawMessage(`true`)},
			{Type: protocol.StreamComplete, Stream: "s", Sequence: 4},
		}
		for _, frame := range frames {
			if _, err := receiver.Accept(frame); err != nil {
				t.Fatalf("path index %T rejected: %v", index, err)
			}
		}
		if got := string(receiver.Snapshot()); got != `[{"value":true}]` {
			t.Fatalf("path index %T snapshot = %s", index, got)
		}
	}
}

func TestStreamReceiverRequiresOpenAndSingleImmediateResume(t *testing.T) {
	receiver, err := protocol.NewStreamReceiver("s", protocol.DefaultStreamLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Accept(protocol.StreamFrame{Type: protocol.StreamKeepalive, Stream: "s"}); !errors.Is(err, protocol.ErrInvalidStreamFrame) {
		t.Fatalf("keepalive before open = %v", err)
	}
	if _, err := receiver.Accept(protocol.StreamFrame{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Accept(protocol.StreamFrame{Type: protocol.StreamKeepalive, Stream: "s"}); err != nil {
		t.Fatalf("keepalive after open = %v", err)
	}
	if _, err := receiver.Accept(protocol.StreamFrame{Type: protocol.StreamResume, Stream: "s", Sequence: 2, Cursor: "cursor"}); err != nil {
		t.Fatalf("immediate resume = %v", err)
	}
	if _, err := receiver.Accept(protocol.StreamFrame{Type: protocol.StreamResume, Stream: "s", Sequence: 3, Cursor: "cursor"}); !errors.Is(err, protocol.ErrInvalidStreamFrame) {
		t.Fatalf("second resume = %v", err)
	}

	late, err := protocol.NewStreamReceiver("s", protocol.DefaultStreamLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range []protocol.StreamFrame{
		{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"},
		{Type: protocol.StreamData, Stream: "s", Sequence: 2, Data: json.RawMessage(`true`)},
	} {
		if _, err := late.Accept(frame); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := late.Accept(protocol.StreamFrame{Type: protocol.StreamResume, Stream: "s", Sequence: 3, Cursor: "cursor"}); !errors.Is(err, protocol.ErrInvalidStreamFrame) {
		t.Fatalf("late resume = %v", err)
	}
}

func TestStreamOpenFrameRequiresSequenceOneAtCodecBoundary(t *testing.T) {
	frame := protocol.StreamFrame{Type: protocol.StreamOpen, Stream: "s", Sequence: 2, SchemaRevision: "schema-r1"}
	if err := frame.Validate(protocol.DefaultStreamLimits()); !errors.Is(err, protocol.ErrInvalidStreamFrame) {
		t.Fatalf("open sequence validation = %v", err)
	}
	if _, err := protocol.MarshalStreamFrame(frame, protocol.DefaultStreamLimits()); !errors.Is(err, protocol.ErrInvalidStreamFrame) {
		t.Fatalf("open sequence marshal = %v", err)
	}
}

func TestStreamPatchCannotAppendMissingListPosition(t *testing.T) {
	receiver, err := protocol.NewStreamReceiver("s", protocol.DefaultStreamLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range []protocol.StreamFrame{
		{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"},
		{Type: protocol.StreamData, Stream: "s", Sequence: 2, Data: json.RawMessage(`[true]`)},
	} {
		if _, err := receiver.Accept(frame); err != nil {
			t.Fatal(err)
		}
	}
	patch := protocol.StreamFrame{Type: protocol.StreamPatch, Stream: "s", Sequence: 3, Position: 1, HasPosition: true, Path: []any{uint64(1)}, Data: json.RawMessage(`false`)}
	if _, err := receiver.Accept(patch); !errors.Is(err, protocol.ErrStreamPatchOrder) {
		t.Fatalf("append patch = %v", err)
	}
	if got := string(receiver.Snapshot()); got != `[true]` {
		t.Fatalf("snapshot after rejected append = %s", got)
	}
}

func TestResumingStreamReceiverAppliesPatchToExistingSnapshot(t *testing.T) {
	receiver, err := protocol.NewResumingStreamReceiver("s", json.RawMessage(`{"value":false}`), 1, protocol.DefaultStreamLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range []protocol.StreamFrame{
		{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"},
		{Type: protocol.StreamResume, Stream: "s", Sequence: 2, Cursor: "opaque-cursor"},
		{Type: protocol.StreamPatch, Stream: "s", Sequence: 3, Position: 2, HasPosition: true, Path: []any{"value"}, Data: json.RawMessage(`true`)},
		{Type: protocol.StreamComplete, Stream: "s", Sequence: 4},
	} {
		if _, err := receiver.Accept(frame); err != nil {
			t.Fatalf("resume frame %#v: %v", frame, err)
		}
	}
	if got := string(receiver.Snapshot()); got != `{"value":true}` {
		t.Fatalf("resumed snapshot = %s", got)
	}
}

func TestResumingStreamReceiverRejectsReplayBeforeResume(t *testing.T) {
	receiver, err := protocol.NewResumingStreamReceiver("s", json.RawMessage(`{"value":false}`), 1, protocol.DefaultStreamLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Accept(protocol.StreamFrame{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"}); err != nil {
		t.Fatal(err)
	}
	patch := protocol.StreamFrame{Type: protocol.StreamPatch, Stream: "s", Sequence: 2, Position: 2, HasPosition: true, Path: []any{"value"}, Data: json.RawMessage(`true`)}
	if _, err := receiver.Accept(patch); !errors.Is(err, protocol.ErrInvalidStreamFrame) {
		t.Fatalf("patch before resume = %v", err)
	}
}

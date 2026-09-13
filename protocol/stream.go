package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode"
	"unicode/utf8"
)

// StreamEventType identifies one transport-independent stream event.
type StreamEventType string

const (
	StreamProfileVersion = "1"

	StreamOpen               StreamEventType = "open"
	StreamData               StreamEventType = "data"
	StreamPatch              StreamEventType = "patch"
	StreamError              StreamEventType = "error"
	StreamComplete           StreamEventType = "complete"
	StreamKeepalive          StreamEventType = "keepalive"
	StreamResume             StreamEventType = "resume"
	StreamHistoryUnavailable StreamEventType = "history-unavailable"
)

// StreamRecovery is the safe client action after replay history is unavailable.
type StreamRecovery string

const (
	StreamRecoveryRestart StreamRecovery = "restart"
	StreamRecoveryRefetch StreamRecovery = "refetch"
)

var (
	ErrInvalidStreamFrame       = errors.New("invalid stream frame")
	ErrStreamSequenceGap        = errors.New("stream sequence gap")
	ErrStreamDuplicateConflict  = errors.New("conflicting duplicate stream frame")
	ErrStreamDuplicateExpired   = errors.New("stream duplicate is older than the retained verification window")
	ErrStreamPositionOrder      = errors.New("stream replay position is not monotonic")
	ErrStreamPatchOrder         = errors.New("stream patch parent is unavailable")
	ErrStreamLimit              = errors.New("stream receiver limit exceeded")
	ErrStreamTruncated          = errors.New("stream ended without a terminal frame")
	ErrStreamClosed             = errors.New("stream is already terminal")
	ErrUnsupportedStreamVersion = errors.New("unsupported streaming profile version")
)

// NegotiateStreamProfile accepts only the exact logical-stream profile
// version implemented by this package.
func NegotiateStreamProfile(version string) error {
	if version != StreamProfileVersion {
		return ErrUnsupportedStreamVersion
	}
	return nil
}

// StreamLimits bounds a logical frame independently of transport buffering.
type StreamLimits struct {
	JSON                 Limits
	MaxFrameBytes        int
	MaxDataBytes         int
	MaxPathDepth         int
	MaxIdentifierBytes   int
	MaxStateBytes        int
	MaxIssues            int
	MaxTrackedSequences  int
	MaxErrorMessageBytes int
}

func DefaultStreamLimits() StreamLimits {
	limits := StreamLimits{JSON: DefaultLimits()}
	limits.MaxFrameBytes = 256 << 10
	limits.MaxDataBytes = 192 << 10
	limits.MaxPathDepth = 64
	limits.MaxIdentifierBytes = 512
	limits.MaxStateBytes = 4 << 20
	limits.MaxIssues = 1024
	limits.MaxTrackedSequences = 4096
	limits.MaxErrorMessageBytes = 32 << 10
	return limits
}

// StreamFrameError is the stable public error carried by an error frame.
type StreamFrameError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// StreamFrame is one independently parseable logical stream event. Sequence is
// transport delivery order; Position is an optional application replay order.
type StreamFrame struct {
	Type           StreamEventType
	Stream         string
	Sequence       uint64
	Position       uint64
	HasPosition    bool
	EventID        string
	Path           []any
	Data           json.RawMessage
	Error          *StreamFrameError
	Final          bool
	Cursor         string
	Recovery       StreamRecovery
	SchemaRevision string
}

type streamFrameWire struct {
	Type           StreamEventType   `json:"type"`
	Stream         string            `json:"stream"`
	Sequence       uint64            `json:"sequence,omitempty"`
	Position       *uint64           `json:"position,omitempty"`
	EventID        string            `json:"eventId,omitempty"`
	Path           []any             `json:"path,omitempty"`
	Data           json.RawMessage   `json:"data,omitempty"`
	Error          *StreamFrameError `json:"error,omitempty"`
	Final          bool              `json:"final,omitempty"`
	Cursor         string            `json:"cursor,omitempty"`
	Recovery       StreamRecovery    `json:"recovery,omitempty"`
	SchemaRevision string            `json:"schemaRevision,omitempty"`
}

func (f StreamFrame) Validate(limits StreamLimits) error {
	limits = ResolveStreamLimits(limits)
	if !validStreamIdentifier(f.Stream, limits.MaxIdentifierBytes) || !validStreamEventType(f.Type) {
		return ErrInvalidStreamFrame
	}
	if f.EventID != "" && !validStreamIdentifier(f.EventID, limits.MaxIdentifierBytes) {
		return ErrInvalidStreamFrame
	}
	if f.Cursor != "" && !validStreamIdentifier(f.Cursor, limits.MaxIdentifierBytes*4) {
		return ErrInvalidStreamFrame
	}
	if len(f.Path) > limits.MaxPathDepth || !validStreamPath(f.Path, limits.MaxIdentifierBytes) {
		return ErrInvalidStreamFrame
	}
	if len(f.Data) > limits.MaxDataBytes || (len(f.Data) != 0 && ValidateJSON(f.Data, limits.JSON) != nil) {
		return ErrInvalidStreamFrame
	}
	if f.Type == StreamKeepalive {
		if f.Sequence != 0 || f.HasPosition || f.EventID != "" || len(f.Path) != 0 || len(f.Data) != 0 || f.Error != nil || f.Final || f.Cursor != "" || f.Recovery != "" || f.SchemaRevision != "" {
			return ErrInvalidStreamFrame
		}
		return nil
	}
	if f.Sequence == 0 || (f.HasPosition && f.Position == 0) {
		return ErrInvalidStreamFrame
	}
	switch f.Type {
	case StreamOpen:
		if f.Sequence != 1 || f.SchemaRevision == "" || !validStreamIdentifier(f.SchemaRevision, limits.MaxIdentifierBytes) ||
			f.HasPosition || f.EventID != "" || len(f.Path) != 0 || len(f.Data) != 0 || f.Error != nil || f.Final || f.Cursor != "" || f.Recovery != "" {
			return ErrInvalidStreamFrame
		}
	case StreamData:
		if len(f.Path) != 0 || len(f.Data) == 0 || f.Error != nil || f.Final || f.Recovery != "" || f.SchemaRevision != "" || (f.Cursor != "" && !f.HasPosition) {
			return ErrInvalidStreamFrame
		}
	case StreamPatch:
		if len(f.Path) == 0 || len(f.Data) == 0 || !f.HasPosition || f.Error != nil || f.Final || f.Recovery != "" || f.SchemaRevision != "" {
			return ErrInvalidStreamFrame
		}
	case StreamError:
		if f.Error == nil || !validStreamCode(f.Error.Code, limits.MaxIdentifierBytes) || f.Error.Message == "" || len(f.Error.Message) > limits.MaxErrorMessageBytes || !utf8.ValidString(f.Error.Message) || len(f.Data) != 0 || f.Cursor != "" || f.Recovery != "" || f.SchemaRevision != "" || (!f.Final && len(f.Path) == 0) {
			return ErrInvalidStreamFrame
		}
	case StreamComplete:
		if hasStreamPayload(f) {
			return ErrInvalidStreamFrame
		}
	case StreamResume:
		if f.Cursor == "" || f.HasPosition || f.EventID != "" || len(f.Path) != 0 || len(f.Data) != 0 || f.Error != nil || f.Final || f.Recovery != "" || f.SchemaRevision != "" {
			return ErrInvalidStreamFrame
		}
	case StreamHistoryUnavailable:
		if f.Recovery != StreamRecoveryRestart && f.Recovery != StreamRecoveryRefetch {
			return ErrInvalidStreamFrame
		}
		if f.HasPosition || f.EventID != "" || len(f.Path) != 0 || len(f.Data) != 0 || f.Error != nil || f.Final || f.Cursor != "" || f.SchemaRevision != "" {
			return ErrInvalidStreamFrame
		}
	case StreamKeepalive:
		return nil
	default:
		return ErrInvalidStreamFrame
	}
	return nil
}

func MarshalStreamFrame(frame StreamFrame, limits StreamLimits) ([]byte, error) {
	limits = ResolveStreamLimits(limits)
	if err := frame.Validate(limits); err != nil {
		return nil, err
	}
	wire := streamFrameWire{
		Type: frame.Type, Stream: frame.Stream, Sequence: frame.Sequence, EventID: frame.EventID,
		Path: frame.Path, Data: frame.Data, Error: frame.Error, Final: frame.Final,
		Cursor: frame.Cursor, Recovery: frame.Recovery, SchemaRevision: frame.SchemaRevision,
	}
	if frame.HasPosition {
		position := frame.Position
		wire.Position = &position
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("marshal stream frame: %w", err)
	}
	if len(encoded) > limits.MaxFrameBytes {
		return nil, ErrInvalidStreamFrame
	}
	return encoded, nil
}

func DecodeStreamFrame(input []byte, limits StreamLimits) (StreamFrame, error) {
	limits = ResolveStreamLimits(limits)
	if len(input) == 0 || len(input) > limits.MaxFrameBytes || ValidateJSON(input, limits.JSON) != nil {
		return StreamFrame{}, ErrInvalidStreamFrame
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var wire streamFrameWire
	if err := decoder.Decode(&wire); err != nil {
		return StreamFrame{}, ErrInvalidStreamFrame
	}
	frame := streamFrameFromWire(wire)
	if err := frame.Validate(limits); err != nil {
		return StreamFrame{}, err
	}
	return frame, nil
}

func streamFrameFromWire(wire streamFrameWire) StreamFrame {
	frame := StreamFrame{
		Type: wire.Type, Stream: wire.Stream, Sequence: wire.Sequence, EventID: wire.EventID,
		Path: wire.Path, Data: wire.Data, Error: wire.Error, Final: wire.Final,
		Cursor: wire.Cursor, Recovery: wire.Recovery, SchemaRevision: wire.SchemaRevision,
	}
	if wire.Position != nil {
		frame.Position, frame.HasPosition = *wire.Position, true
	}
	return frame
}

func hasStreamPayload(frame StreamFrame) bool {
	return frame.HasPosition || frame.EventID != "" || len(frame.Path) != 0 || len(frame.Data) != 0 || frame.Error != nil || frame.Final || frame.Cursor != "" || frame.Recovery != "" || frame.SchemaRevision != ""
}

func validStreamEventType(kind StreamEventType) bool {
	allowed := [...]StreamEventType{StreamOpen, StreamData, StreamPatch, StreamError, StreamComplete, StreamKeepalive, StreamResume, StreamHistoryUnavailable}
	for _, candidate := range allowed {
		if kind == candidate {
			return true
		}
	}
	return false
}

func validStreamIdentifier(value string, maximum int) bool {
	if value == "" || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func validStreamCode(value string, maximum int) bool {
	if !validStreamIdentifier(value, maximum) {
		return false
	}
	for _, char := range value {
		if (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func validStreamPath(path []any, maximum int) bool {
	for _, segment := range path {
		switch value := segment.(type) {
		case string:
			if !validStreamIdentifier(value, maximum) {
				return false
			}
		case json.Number:
			if _, err := strconv.ParseUint(string(value), 10, 64); err != nil {
				return false
			}
		case uint64, uint32, uint16, uint8, uint:
		case int:
			if value < 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// ResolveStreamLimits fills unset fields with the safe profile defaults while
// retaining every caller-provided limit.
func ResolveStreamLimits(limits StreamLimits) StreamLimits {
	defaults := DefaultStreamLimits()
	if limits.JSON.MaxBytes == 0 {
		limits.JSON = defaults.JSON
	}
	values := [...][2]*int{
		{&limits.MaxFrameBytes, &defaults.MaxFrameBytes},
		{&limits.MaxDataBytes, &defaults.MaxDataBytes},
		{&limits.MaxPathDepth, &defaults.MaxPathDepth},
		{&limits.MaxIdentifierBytes, &defaults.MaxIdentifierBytes},
		{&limits.MaxStateBytes, &defaults.MaxStateBytes},
		{&limits.MaxIssues, &defaults.MaxIssues},
		{&limits.MaxTrackedSequences, &defaults.MaxTrackedSequences},
		{&limits.MaxErrorMessageBytes, &defaults.MaxErrorMessageBytes},
	}
	for _, pair := range values {
		if *pair[0] <= 0 {
			*pair[0] = *pair[1]
		}
	}
	return limits
}

// StreamAcceptResult reports whether an accepted frame was an exact replay.
type StreamAcceptResult struct {
	Duplicate bool
}

// StreamIssue is one non-terminal response-path error observed by a receiver.
type StreamIssue struct {
	Code    string
	Message string
	Path    []any
}

// StreamTerminalOutcome records the explicit terminal control frame.
type StreamTerminalOutcome struct {
	Type     StreamEventType
	Error    *StreamFrameError
	Path     []any
	Recovery StreamRecovery
}

// StreamReceiver validates delivery order and applies initial data and patches.
type StreamReceiver struct {
	stream          string
	limits          StreamLimits
	expected        uint64
	position        uint64
	seen            map[uint64][sha256.Size]byte
	opened          bool
	closed          bool
	resumed         bool
	requiresResume  bool
	terminal        bool
	terminalOutcome StreamTerminalOutcome
	state           any
	hasState        bool
	issues          []StreamIssue
}

func NewStreamReceiver(stream string, limits StreamLimits) (*StreamReceiver, error) {
	limits = ResolveStreamLimits(limits)
	if !validStreamIdentifier(stream, limits.MaxIdentifierBytes) {
		return nil, ErrInvalidStreamFrame
	}
	return &StreamReceiver{stream: stream, limits: limits, expected: 1, seen: make(map[uint64][sha256.Size]byte)}, nil
}

// NewResumingStreamReceiver restores an already accepted client snapshot and
// replay position. The receiver still requires a fresh open frame followed by
// one resume frame before replayed patches are accepted.
func NewResumingStreamReceiver(stream string, snapshot json.RawMessage, position uint64, limits StreamLimits) (*StreamReceiver, error) {
	receiver, err := NewStreamReceiver(stream, limits)
	if err != nil || position == 0 || len(snapshot) == 0 || len(snapshot) > receiver.limits.MaxStateBytes {
		return nil, ErrInvalidStreamFrame
	}
	if err := ValidateJSON(snapshot, receiver.limits.JSON); err != nil {
		return nil, ErrInvalidStreamFrame
	}
	state, err := decodeStreamData(snapshot)
	if err != nil {
		return nil, err
	}
	receiver.state = state
	receiver.hasState = true
	receiver.position = position
	receiver.requiresResume = true
	return receiver, nil
}

func (r *StreamReceiver) Accept(frame StreamFrame) (StreamAcceptResult, error) {
	if r == nil || r.closed {
		return StreamAcceptResult{}, ErrStreamClosed
	}
	if err := frame.Validate(r.limits); err != nil || frame.Stream != r.stream {
		return StreamAcceptResult{}, ErrInvalidStreamFrame
	}
	if frame.Type == StreamKeepalive {
		if !r.opened {
			return StreamAcceptResult{}, ErrInvalidStreamFrame
		}
		return StreamAcceptResult{}, nil
	}
	digest, err := streamFrameDigest(frame, r.limits)
	if err != nil {
		return StreamAcceptResult{}, err
	}
	if frame.Sequence < r.expected {
		if previous, ok := r.seen[frame.Sequence]; ok && previous == digest {
			return StreamAcceptResult{Duplicate: true}, nil
		}
		if _, ok := r.seen[frame.Sequence]; !ok {
			return StreamAcceptResult{}, ErrStreamDuplicateExpired
		}
		return StreamAcceptResult{}, ErrStreamDuplicateConflict
	}
	if frame.Sequence > r.expected {
		return StreamAcceptResult{}, ErrStreamSequenceGap
	}
	if frame.Sequence == ^uint64(0) && frame.Type != StreamComplete && (frame.Type != StreamError || !frame.Final) {
		return StreamAcceptResult{}, ErrStreamLimit
	}
	if frame.HasPosition && frame.Position <= r.position {
		return StreamAcceptResult{}, ErrStreamPositionOrder
	}
	if len(frame.Data) > r.limits.MaxStateBytes {
		return StreamAcceptResult{}, ErrStreamLimit
	}
	if frame.Type == StreamError && !frame.Final && len(r.issues) == r.limits.MaxIssues {
		return StreamAcceptResult{}, ErrStreamLimit
	}
	previousState, previousHasState := r.state, r.hasState
	if (frame.Type == StreamData || frame.Type == StreamPatch) && r.hasState {
		r.state = cloneStreamValue(r.state)
	}
	if err := r.apply(frame); err != nil {
		r.state, r.hasState = previousState, previousHasState
		return StreamAcceptResult{}, err
	}
	if frame.Type == StreamData || frame.Type == StreamPatch {
		encoded, err := json.Marshal(r.state)
		if err != nil || len(encoded) > r.limits.MaxStateBytes {
			r.state, r.hasState = previousState, previousHasState
			return StreamAcceptResult{}, ErrStreamLimit
		}
	}
	if frame.HasPosition {
		r.position = frame.Position
	}
	r.seen[frame.Sequence] = digest
	if len(r.seen) > r.limits.MaxTrackedSequences {
		delete(r.seen, frame.Sequence-uint64(r.limits.MaxTrackedSequences))
	}
	r.expected++
	return StreamAcceptResult{}, nil
}

func (r *StreamReceiver) apply(frame StreamFrame) error {
	if !r.opened {
		if frame.Type != StreamOpen {
			return ErrInvalidStreamFrame
		}
		r.opened = true
		return nil
	}
	if r.requiresResume && !r.resumed && frame.Type != StreamResume && frame.Type != StreamHistoryUnavailable && frame.Type != StreamKeepalive {
		return ErrInvalidStreamFrame
	}
	switch frame.Type {
	case StreamOpen:
		return ErrInvalidStreamFrame
	case StreamData:
		value, err := decodeStreamData(frame.Data)
		if err != nil {
			return err
		}
		if r.hasState {
			return ErrStreamPatchOrder
		}
		r.state, r.hasState = value, true
		return nil
	case StreamPatch:
		if !r.hasState {
			return ErrStreamPatchOrder
		}
		value, err := decodeStreamData(frame.Data)
		if err != nil {
			return err
		}
		r.state, err = setStreamPath(r.state, frame.Path, value)
		return err
	case StreamError:
		if !frame.Final {
			r.issues = append(r.issues, StreamIssue{Code: frame.Error.Code, Message: frame.Error.Message, Path: cloneStreamPath(frame.Path)})
		} else {
			terminalError := *frame.Error
			r.terminalOutcome = StreamTerminalOutcome{Type: StreamError, Error: &terminalError, Path: cloneStreamPath(frame.Path)}
			r.closed = true
		}
		r.terminal = frame.Final
	case StreamComplete:
		r.closed = true
		r.terminal = true
		r.terminalOutcome = StreamTerminalOutcome{Type: StreamComplete}
	case StreamHistoryUnavailable:
		if r.resumed || r.expected != 2 || len(r.issues) != 0 {
			return ErrInvalidStreamFrame
		}
		r.closed = true
		r.terminalOutcome = StreamTerminalOutcome{Type: StreamHistoryUnavailable, Recovery: frame.Recovery}
	case StreamResume:
		if r.resumed || r.expected != 2 {
			return ErrInvalidStreamFrame
		}
		r.resumed = true
		return nil
	case StreamKeepalive:
		return nil
	default:
		return ErrInvalidStreamFrame
	}
	return nil
}

func (r *StreamReceiver) Finish() error {
	if r != nil && r.closed {
		return nil
	}
	return ErrStreamTruncated
}

// Recovery returns an accepted history-loss outcome. Recovery closes the
// reconnect attempt but is not a successful logical stream terminal.
func (r *StreamReceiver) Recovery() (StreamTerminalOutcome, bool) {
	if r == nil || !r.closed || r.terminalOutcome.Type != StreamHistoryUnavailable {
		return StreamTerminalOutcome{}, false
	}
	return r.terminalOutcome, true
}

func (r *StreamReceiver) Snapshot() json.RawMessage {
	if r == nil || !r.hasState {
		return nil
	}
	encoded, _ := json.Marshal(r.state)
	return encoded
}

func (r *StreamReceiver) Errors() []StreamIssue {
	if r == nil {
		return nil
	}
	result := make([]StreamIssue, len(r.issues))
	copy(result, r.issues)
	for index := range result {
		result[index].Path = cloneStreamPath(result[index].Path)
	}
	return result
}

// Terminal returns the accepted terminal outcome without requiring callers to
// retain the input frame separately.
func (r *StreamReceiver) Terminal() (StreamTerminalOutcome, bool) {
	if r == nil || !r.terminal {
		return StreamTerminalOutcome{}, false
	}
	outcome := r.terminalOutcome
	if outcome.Error != nil {
		cloned := *outcome.Error
		outcome.Error = &cloned
	}
	outcome.Path = cloneStreamPath(outcome.Path)
	return outcome, true
}

func streamFrameDigest(frame StreamFrame, limits StreamLimits) ([sha256.Size]byte, error) {
	encoded, err := MarshalStreamFrame(frame, limits)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

func decodeStreamData(raw json.RawMessage) (any, error) {
	value, err := DecodeJSONValue(raw)
	if err != nil {
		return nil, ErrInvalidStreamFrame
	}
	return value, nil
}

func setStreamPath(current any, path []any, value any) (any, error) {
	if len(path) == 0 {
		return value, nil
	}
	switch container := current.(type) {
	case map[string]any:
		key, ok := path[0].(string)
		if !ok {
			return nil, ErrStreamPatchOrder
		}
		if len(path) == 1 {
			container[key] = value
			return current, nil
		}
		child, ok := container[key]
		if !ok {
			return nil, ErrStreamPatchOrder
		}
		updated, err := setStreamPath(child, path[1:], value)
		if err != nil {
			return nil, err
		}
		container[key] = updated
		return current, nil
	case []any:
		index, ok := streamPathIndex(path[0])
		if !ok || index >= uint64(len(container)) {
			return nil, ErrStreamPatchOrder
		}
		if len(path) == 1 {
			container[index] = value
			return container, nil
		}
		if index >= uint64(len(container)) {
			return nil, ErrStreamPatchOrder
		}
		updated, err := setStreamPath(container[index], path[1:], value)
		if err != nil {
			return nil, err
		}
		container[index] = updated
		return container, nil
	default:
		return nil, ErrStreamPatchOrder
	}
}

func streamPathIndex(segment any) (uint64, bool) {
	switch value := segment.(type) {
	case json.Number:
		parsed, err := strconv.ParseUint(string(value), 10, 64)
		return parsed, err == nil
	case uint64:
		return value, true
	case uint32:
		return uint64(value), true
	case uint16:
		return uint64(value), true
	case uint8:
		return uint64(value), true
	case uint:
		return uint64(value), true
	case int:
		return uint64(value), value >= 0
	default:
		return 0, false
	}
}

func cloneStreamPath(path []any) []any {
	return append([]any(nil), path...)
}

func cloneStreamValue(value any) any {
	switch current := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(current))
		for key, child := range current {
			cloned[key] = cloneStreamValue(child)
		}
		return cloned
	case []any:
		cloned := make([]any, len(current))
		for index, child := range current {
			cloned[index] = cloneStreamValue(child)
		}
		return cloned
	default:
		return current
	}
}

package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/valksor/naatre/event"
)

type ReceiveOutcome string

const (
	ReceiveAccepted  ReceiveOutcome = "accepted"
	ReceiveDuplicate ReceiveOutcome = "authenticated-duplicate"
)

// ReceiveResult contains authenticated identity and validated envelopes. It is
// an application input, not a routine logging shape.
type ReceiveResult struct {
	Outcome      ReceiveOutcome
	Verification event.VerificationResult
	Events       []event.Envelope
}

type ReceiverConfig struct {
	Verifier         *event.Verifier
	Store            *SQLiteStore
	TargetURI        string
	MaximumBodyBytes int
	MaximumEvents    int
}

// Receiver verifies exact HTTP content before decoding it, then persists the
// per-stream monotonic sequence boundary. It does not invoke application code,
// send acknowledgements, or own an HTTP listener.
type Receiver struct {
	config ReceiverConfig
}

func NewReceiver(config ReceiverConfig) (*Receiver, error) {
	parsed, err := url.Parse(config.TargetURI)
	if config.Verifier == nil || config.Store == nil || err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" ||
		config.MaximumBodyBytes < 1 || config.MaximumBodyBytes > event.DefaultMaximumBodyBytes ||
		config.MaximumEvents < 1 || config.MaximumEvents > 100 {
		return nil, publicError(CodeInvalidConfig, "webhook receiver configuration is invalid", nil)
	}
	return &Receiver{config: config}, nil
}

func (r *Receiver) VerifyRequest(ctx context.Context, request *http.Request) (ReceiveResult, error) {
	if r == nil || request == nil || !requestMatchesTarget(request, r.config.TargetURI) || request.Body == nil {
		return ReceiveResult{}, publicError(CodeSignatureInvalid, "webhook request authentication failed", nil)
	}
	if err := contextFailure(ctx); err != nil {
		return ReceiveResult{}, err
	}
	if request.ContentLength > int64(r.config.MaximumBodyBytes) {
		return ReceiveResult{}, publicError(CodeResourceExhausted, "webhook request exceeds configured limits", nil)
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, int64(r.config.MaximumBodyBytes)+1))
	if err != nil {
		return ReceiveResult{}, publicError(CodeSignatureInvalid, "webhook request body is invalid", nil)
	}
	if len(body) == 0 || len(body) > r.config.MaximumBodyBytes {
		return ReceiveResult{}, publicError(CodeResourceExhausted, "webhook request exceeds configured limits", nil)
	}
	contentType, ok := singletonHeader(request, "Content-Type")
	if !ok {
		return ReceiveResult{}, publicError(CodeSignatureInvalid, "webhook request authentication failed", nil)
	}
	contentEncoding, ok := singletonHeader(request, "Content-Encoding")
	if !ok {
		return ReceiveResult{}, publicError(CodeSignatureInvalid, "webhook request authentication failed", nil)
	}
	message := event.Message{
		Method: request.Method, TargetURI: r.config.TargetURI, ContentType: contentType,
		ContentEncoding: contentEncoding, Body: body,
	}
	for header, destination := range map[string]*string{
		"Content-Digest": &message.ContentDigest, "Naatre-Webhook-Id": &message.DeliveryID,
		"Naatre-Webhook-Timestamp": &message.Timestamp, "Naatre-Webhook-Audience": &message.Audience,
		"Signature-Input": &message.SignatureInput, "Signature": &message.Signature,
	} {
		value, ok := singletonHeader(request, header)
		if !ok {
			return ReceiveResult{}, publicError(CodeSignatureInvalid, "webhook request authentication failed", nil)
		}
		*destination = value
	}
	verified, err := r.config.Verifier.Verify(message)
	if err != nil {
		return ReceiveResult{}, receiverFailure(err)
	}
	if verified.Replay {
		return ReceiveResult{Outcome: ReceiveDuplicate, Verification: verified}, nil
	}
	decoded, err := decodeBody(body, contentEncoding, r.config.MaximumBodyBytes)
	if err != nil {
		return ReceiveResult{}, err
	}
	envelopes, err := decodeEnvelopes(decoded, contentType, r.config.MaximumEvents)
	if err != nil {
		return ReceiveResult{}, err
	}
	sequences := make([]sequenceValue, 0, len(envelopes))
	for _, envelope := range envelopes {
		if envelope.Sequence != nil {
			sequences = append(sequences, sequenceValue{
				Stream:   sequenceStream(verified.Sender, verified.Audience, envelope.Type, envelope.OrderingKey),
				Sequence: *envelope.Sequence,
			})
		}
	}
	if err := r.config.Store.recordSequences(ctx, sequences); err != nil {
		return ReceiveResult{}, err
	}
	return ReceiveResult{Outcome: ReceiveAccepted, Verification: verified, Events: envelopes}, nil
}

func requestMatchesTarget(request *http.Request, target string) bool {
	if request.Method != http.MethodPost || request.URL == nil {
		return false
	}
	want, err := url.Parse(target)
	if err != nil {
		return false
	}
	if request.URL.IsAbs() {
		return request.URL.String() == target
	}
	return request.Host == want.Host && request.URL.EscapedPath() == want.EscapedPath() && request.URL.RawQuery == want.RawQuery
}

func singletonHeader(request *http.Request, name string) (string, bool) {
	values := request.Header.Values(name)
	returnValue := ""
	if len(values) == 1 {
		returnValue = values[0]
	}
	return returnValue, len(values) == 1 && returnValue != "" && !strings.ContainsAny(returnValue, "\x00\r\n")
}

func receiverFailure(err error) error {
	switch {
	case errors.Is(err, event.ErrFreshness):
		return publicError(CodeStale, "webhook signature is outside its validity window", nil)
	case errors.Is(err, event.ErrReplayStore):
		return publicError(CodeReplayStore, "webhook replay decision is unavailable", nil)
	default:
		return publicError(CodeSignatureInvalid, "webhook request authentication failed", nil)
	}
}

func decodeEnvelopes(body []byte, contentType string, maximum int) ([]event.Envelope, error) {
	var raws []json.RawMessage
	switch contentType {
	case event.JSONContentType:
		raws = []json.RawMessage{append([]byte(nil), body...)}
	case event.BatchJSONContentType:
		if err := json.Unmarshal(body, &raws); err != nil {
			return nil, publicError(CodeInvalidRecord, "webhook batch is invalid", nil)
		}
	default:
		return nil, publicError(CodeInvalidRecord, "webhook content type is unsupported", nil)
	}
	if len(raws) == 0 || len(raws) > maximum {
		return nil, publicError(CodeResourceExhausted, "webhook batch exceeds configured limits", nil)
	}
	envelopes := make([]event.Envelope, 0, len(raws))
	for _, raw := range raws {
		envelope, err := decodeEnvelope(raw)
		if err != nil {
			return nil, err
		}
		envelopes = append(envelopes, envelope)
	}
	return envelopes, nil
}

func decodeEnvelope(raw []byte) (event.Envelope, error) {
	var wire map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&wire); err != nil {
		return event.Envelope{}, publicError(CodeInvalidRecord, "webhook event envelope is invalid", nil)
	}
	var envelope event.Envelope
	if !takeString(wire, "id", &envelope.ID) || !takeString(wire, "source", &envelope.Source) ||
		!takeString(wire, "type", &envelope.Type) || !takeOptionalString(wire, "subject", &envelope.Subject) ||
		!takeString(wire, "dataschema", &envelope.DataSchema) || !takeString(wire, "naatreschemarevision", &envelope.SchemaRevision) ||
		!takeString(wire, "naatreschemadigest", &envelope.SchemaDigest) {
		return event.Envelope{}, publicError(CodeInvalidRecord, "webhook event envelope is invalid", nil)
	}
	var specVersion, contentType, timestamp string
	if !takeString(wire, "specversion", &specVersion) || specVersion != event.CloudEventsSpecVersion ||
		!takeString(wire, "datacontenttype", &contentType) || contentType != "application/json" || !takeString(wire, "time", &timestamp) {
		return event.Envelope{}, publicError(CodeInvalidRecord, "webhook event envelope is invalid", nil)
	}
	parsedTime, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return event.Envelope{}, publicError(CodeInvalidRecord, "webhook event envelope is invalid", nil)
	}
	envelope.Time = parsedTime
	data, ok := wire["data"]
	if !ok {
		return event.Envelope{}, publicError(CodeInvalidRecord, "webhook event envelope is invalid", nil)
	}
	envelope.Data = append([]byte(nil), data...)
	delete(wire, "data")
	if ordering, ok := wire["naatreorderingkey"]; ok {
		if json.Unmarshal(ordering, &envelope.OrderingKey) != nil {
			return event.Envelope{}, publicError(CodeInvalidRecord, "webhook event ordering is invalid", nil)
		}
		delete(wire, "naatreorderingkey")
	}
	if sequence, ok := wire["naatresequence"]; ok {
		var value uint64
		if json.Unmarshal(sequence, &value) != nil {
			return event.Envelope{}, publicError(CodeInvalidRecord, "webhook event ordering is invalid", nil)
		}
		envelope.Sequence = &value
		delete(wire, "naatresequence")
	}
	envelope.Extensions = make(map[string]any, len(wire))
	for name, encoded := range wire {
		var value any
		if json.Unmarshal(encoded, &value) != nil {
			return event.Envelope{}, publicError(CodeInvalidRecord, "webhook event extension is invalid", nil)
		}
		envelope.Extensions[name] = value
	}
	if err := envelope.Validate(); err != nil {
		return event.Envelope{}, publicError(CodeInvalidRecord, "webhook event envelope is invalid", nil)
	}
	return envelope, nil
}

func takeString(values map[string]json.RawMessage, name string, destination *string) bool {
	encoded, ok := values[name]
	if !ok || json.Unmarshal(encoded, destination) != nil {
		return false
	}
	delete(values, name)
	return true
}

func takeOptionalString(values map[string]json.RawMessage, name string, destination *string) bool {
	encoded, ok := values[name]
	if !ok {
		return true
	}
	if json.Unmarshal(encoded, destination) != nil {
		return false
	}
	delete(values, name)
	return true
}

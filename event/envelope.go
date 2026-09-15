// Package event defines the portable Naatre business-event and webhook
// contracts. Applications produce business events; this package only supplies
// the interoperable envelope and delivery primitives.
package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// Profile is the Naatre event and webhook contract version.
	Profile = "core.events-1"
	// CloudEventsVersion pins the specification that defines the mapped envelope.
	CloudEventsVersion = "1.0.2"
	// CloudEventsSpecVersion is the value carried by CloudEvents 1.0 envelopes.
	CloudEventsSpecVersion = "1.0"
	// JSONContentType is the structured CloudEvents JSON media type.
	JSONContentType = "application/cloudevents+json"
	// BatchJSONContentType is the structured CloudEvents JSON batch media type.
	BatchJSONContentType = "application/cloudevents-batch+json"
)

var (
	eventTypePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:\.[a-z0-9][a-z0-9-]*)+\.v[1-9][0-9]*$`)
	digestPattern    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	extensionPattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,19}$`)
)

// Envelope is the Naatre mapping to the CloudEvents v1.0 JSON format. The
// schema revision and digest pin the immutable schema named by DataSchema.
// OrderingKey plus Sequence declare per-stream ordering; when both are absent,
// the producer declares that no ordering guarantee exists.
type Envelope struct {
	ID             string
	Source         string
	Type           string
	Subject        string
	Time           time.Time
	DataSchema     string
	SchemaRevision string
	SchemaDigest   string
	Data           json.RawMessage
	OrderingKey    string
	Sequence       *uint64
	Extensions     map[string]any
}

// Validate checks the portable envelope independently of any application event
// registry. Unknown event types remain syntactically valid and are handled by
// receiver compatibility policy.
func (e Envelope) Validate() error {
	if !boundedText(e.ID, 128) || !eventTypePattern.MatchString(e.Type) {
		return errors.New("event requires a bounded identifier and versioned type")
	}
	if !absoluteURI(e.Source) || !absoluteURI(e.DataSchema) || !boundedOptionalText(e.Subject, 512) {
		return errors.New("event source, subject, or schema reference is invalid")
	}
	_, timeOffset := e.Time.Zone()
	if e.Time.IsZero() || timeOffset != 0 || !boundedText(e.SchemaRevision, 128) || !digestPattern.MatchString(e.SchemaDigest) {
		return errors.New("event requires UTC time and a pinned schema revision")
	}
	if len(e.Data) == 0 || len(e.Data) > 1<<20 || !json.Valid(e.Data) {
		return errors.New("event data must be one bounded JSON value")
	}
	if (e.OrderingKey == "") != (e.Sequence == nil) || !boundedOptionalText(e.OrderingKey, 256) {
		return errors.New("event ordering key and sequence must be declared together")
	}
	for name, value := range e.Extensions {
		if !extensionPattern.MatchString(name) || strings.HasPrefix(name, "naatre") || reservedCloudEventAttribute(name) || !validExtensionValue(value) {
			return fmt.Errorf("invalid event extension %q", name)
		}
	}
	return nil
}

// MarshalJSON emits the CloudEvents structured JSON mapping. The returned
// bytes are ordinary JSON, not a canonical representation; webhook signatures
// cover these exact transmitted bytes.
func (e Envelope) MarshalJSON() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	value := make(map[string]any, 12+len(e.Extensions))
	value["specversion"] = CloudEventsSpecVersion
	value["id"] = e.ID
	value["source"] = e.Source
	value["type"] = e.Type
	value["time"] = e.Time.Format(time.RFC3339Nano)
	value["dataschema"] = e.DataSchema
	value["datacontenttype"] = "application/json"
	value["data"] = json.RawMessage(e.Data)
	value["naatreschemarevision"] = e.SchemaRevision
	value["naatreschemadigest"] = e.SchemaDigest
	if e.Subject != "" {
		value["subject"] = e.Subject
	}
	if e.OrderingKey != "" {
		value["naatreorderingkey"] = e.OrderingKey
		value["naatresequence"] = *e.Sequence
	}
	for name, extension := range e.Extensions {
		value[name] = extension
	}
	return json.Marshal(value)
}

func absoluteURI(value string) bool {
	if !boundedText(value, 2048) {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.IsAbs() && parsed.Scheme != "" && !strings.Contains(value, "#")
}

func boundedText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && !strings.ContainsAny(value, "\x00\r\n")
}

func boundedOptionalText(value string, maximum int) bool {
	return value == "" || boundedText(value, maximum)
}

func reservedCloudEventAttribute(name string) bool {
	switch name {
	case "specversion", "id", "source", "type", "subject", "time", "dataschema", "datacontenttype", "data":
		return true
	default:
		return false
	}
}

func validExtensionValue(value any) bool {
	switch typed := value.(type) {
	case string:
		return boundedOptionalText(typed, 1024)
	case float64:
		return !math.IsNaN(typed) && !math.IsInf(typed, 0)
	case bool, int, int32, int64, uint, uint32, uint64, nil:
		return true
	default:
		return false
	}
}

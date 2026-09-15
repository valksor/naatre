package playground

import (
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/valksor/naatre/tooling"
)

type InspectionRequest struct {
	Target  string            `json:"target"`
	Headers map[string]string `json:"headers,omitempty"`
	Payload json.RawMessage   `json:"payload,omitempty"`
}

type InspectionPayload struct {
	Value     string `json:"value"`
	Truncated bool   `json:"truncated"`
}

type Inspection struct {
	Profile string                       `json:"profile"`
	Target  InspectionPayload            `json:"target"`
	Headers map[string]InspectionPayload `json:"headers"`
	Payload InspectionPayload            `json:"payload"`
}

func Inspect(input InspectionRequest, allowedOrigins []string, maxPayloadBytes int) (Inspection, error) {
	parsed, err := url.ParseRequestURI(input.Target)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Fragment != "" {
		return Inspection{}, errors.New("invalid target")
	}
	origin := strings.ToLower(parsed.Scheme + "://" + parsed.Host)
	if len(allowedOrigins) != 0 && !slices.Contains(allowedOrigins, origin) {
		return Inspection{}, errors.New("target origin is not allowed")
	}
	headers := make(map[string]InspectionPayload, len(input.Headers))
	for name, value := range input.Headers {
		combined := name + ": " + value
		if tooling.IsCredentialName(name) {
			combined = name + ": " + tooling.Redacted
		}
		redacted, truncated := tooling.RedactAndBound(combined, maxPayloadBytes)
		headers[name] = InspectionPayload{Value: redacted, Truncated: truncated}
	}
	target, targetTruncated := tooling.RedactAndBound(input.Target, maxPayloadBytes)
	payload, payloadTruncated := tooling.RedactAndBound(string(input.Payload), maxPayloadBytes)
	return Inspection{
		Profile: Profile,
		Target:  InspectionPayload{Value: target, Truncated: targetTruncated},
		Headers: headers,
		Payload: InspectionPayload{Value: payload, Truncated: payloadTruncated},
	}, nil
}

func normalizeOrigins(input []string) ([]string, error) {
	result := make([]string, 0, len(input))
	for _, raw := range input {
		parsed, err := url.ParseRequestURI(raw)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, errors.New("invalid allowed origin")
		}
		origin := strings.ToLower(parsed.Scheme + "://" + parsed.Host)
		if !slices.Contains(result, origin) {
			result = append(result, origin)
		}
	}
	slices.Sort(result)
	return result, nil
}

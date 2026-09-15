package asyncapi

import (
	"encoding/json"

	"github.com/valksor/naatre/interopadapter"
)

type wireDocument struct {
	AsyncAPI           string                   `json:"asyncapi"`
	ID                 string                   `json:"id"`
	Info               wireInfo                 `json:"info"`
	DefaultContentType string                   `json:"defaultContentType"`
	Servers            map[string]wireServer    `json:"servers"`
	Channels           map[string]wireChannel   `json:"channels"`
	Operations         map[string]wireOperation `json:"operations"`
	Components         wireComponents           `json:"components"`
	Profile            string                   `json:"x-naatre-profile"`
	Exporter           string                   `json:"x-naatre-exporter"`
	Specification      string                   `json:"x-naatre-normative-source"`
	Revisions          Revisions                `json:"x-naatre-revisions"`
	Fidelity           []interopadapter.Mapping `json:"x-naatre-fidelity"`
}

type wireInfo struct {
	Title   string `json:"title"`
	Version string `json:"version"`
}

type wireServer struct {
	Host     string   `json:"host"`
	Protocol string   `json:"protocol"`
	Pathname string   `json:"pathname,omitempty"`
	Identity Identity `json:"x-naatre-identity"`
}

type wireReference struct {
	Ref string `json:"$ref"`
}

type wireChannel struct {
	Address   string                   `json:"address"`
	Messages  map[string]wireReference `json:"messages"`
	Bindings  map[string]wireBinding   `json:"bindings"`
	Identity  Identity                 `json:"x-naatre-identity"`
	Semantics Semantics                `json:"x-naatre-semantics"`
}

type wireBinding struct {
	Transport         Transport     `json:"transport"`
	Server            wireReference `json:"server"`
	Implemented       bool          `json:"implemented"`
	Evidence          []string      `json:"evidence"`
	WireCompatibility bool          `json:"asyncapiWireCompatibility"`
	Identity          Identity      `json:"x-naatre-identity"`
}

type wireOperation struct {
	Action   Action          `json:"action"`
	Channel  wireReference   `json:"channel"`
	Messages []wireReference `json:"messages"`
	Security []wireReference `json:"security"`
	Identity Identity        `json:"x-naatre-identity"`
}

type wireComponents struct {
	Messages        map[string]wireMessage        `json:"messages"`
	Schemas         map[string]wireSchema         `json:"schemas"`
	SecuritySchemes map[string]wireSecurityScheme `json:"securitySchemes"`
	CorrelationIDs  map[string]wireCorrelation    `json:"correlationIds"`
}

type wireMessage struct {
	Name          string            `json:"name"`
	ContentType   string            `json:"contentType"`
	SchemaFormat  string            `json:"schemaFormat"`
	Payload       wireReference     `json:"payload"`
	CorrelationID *wireReference    `json:"correlationId,omitempty"`
	Correlations  []Correlation     `json:"x-naatre-correlations"`
	Examples      []json.RawMessage `json:"examples,omitempty"`
	Identity      Identity          `json:"x-naatre-identity"`
}

type wireSchema struct {
	Type                 string   `json:"type"`
	AdditionalProperties bool     `json:"additionalProperties"`
	NaatreType           string   `json:"x-naatre-type-id"`
	Identity             Identity `json:"x-naatre-identity"`
}

type wireSecurityScheme struct {
	Type     string   `json:"type"`
	Scheme   string   `json:"scheme,omitempty"`
	Name     string   `json:"name,omitempty"`
	In       string   `json:"in,omitempty"`
	Identity Identity `json:"x-naatre-identity"`
}

type wireCorrelation struct {
	Location string   `json:"location"`
	Lifetime string   `json:"x-naatre-lifetime"`
	Trust    string   `json:"x-naatre-trust"`
	Identity Identity `json:"x-naatre-identity"`
}

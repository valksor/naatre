package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"

	"github.com/valksor/naatre/protocol"
)

const (
	// ScalarValidatorShape validates strict JSON shape and declared limits.
	ScalarValidatorShape = "naatre.shape-1"
	// ScalarSerializerIdentity retains the selected canonicalizer's JSON value.
	ScalarSerializerIdentity = "naatre.identity-json-1"
	// ScalarCanonicalJSON applies the protocol RFC 8785 c14n-1 profile.
	ScalarCanonicalJSON = "naatre.c14n-1"
	// ScalarCanonicalASCIILowercaseString lowercases ASCII A-Z in a Unicode
	// string, leaves every other scalar unchanged, then applies c14n-1.
	ScalarCanonicalASCIILowercaseString = "naatre.ascii-lowercase-string.c14n-1"
)

var scalarContractIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._:/-]{0,127}$`)

// scalarRuntime contains only an immutable portable contract. Canonicalization
// dispatches to closed profiles implemented by this package, so handler code,
// clocks, locale, I/O, and mutable process state cannot affect a result.
type scalarRuntime struct {
	contract ScalarDescriptor
}

func newScalarRuntime(descriptor TypeDescriptor) (scalarRuntime, error) {
	if descriptor.Kind != ScalarType || descriptor.Scalar == nil {
		return scalarRuntime{}, fmt.Errorf("custom scalar %q requires a portable scalar contract", descriptor.ID)
	}
	contract := cloneScalarDescriptor(*descriptor.Scalar)
	if err := validateScalarContract(descriptor.ID, contract); err != nil {
		return scalarRuntime{}, err
	}
	runtime := scalarRuntime{contract: contract}
	if err := runtime.verifyConformance(descriptor.ID); err != nil {
		return scalarRuntime{}, err
	}
	return runtime, nil
}

func validateScalarContract(typeID TypeID, contract ScalarDescriptor) error {
	if len(contract.AcceptedWireShapes) == 0 {
		return fmt.Errorf("custom scalar %q requires accepted wire shapes", typeID)
	}
	seenShapes := make(map[JSONShape]bool, len(contract.AcceptedWireShapes))
	for _, shape := range contract.AcceptedWireShapes {
		switch shape {
		case JSONBoolean, JSONString, JSONNumber, JSONArray, JSONObject:
		default:
			return fmt.Errorf("custom scalar %q has unknown wire shape %q", typeID, shape)
		}
		if seenShapes[shape] {
			return fmt.Errorf("custom scalar %q repeats wire shape %q", typeID, shape)
		}
		seenShapes[shape] = true
	}
	for _, item := range []struct {
		name       string
		identifier string
	}{
		{name: "validator", identifier: contract.Validator},
		{name: "serializer", identifier: contract.Serializer},
		{name: "canonicalizer", identifier: contract.Canonicalizer},
		{name: "canonical profile", identifier: contract.CanonicalProfile},
	} {
		if !scalarContractIDPattern.MatchString(item.identifier) {
			return fmt.Errorf("custom scalar %q requires a portable %s identifier", typeID, item.name)
		}
	}
	if contract.Validator != ScalarValidatorShape || contract.Serializer != ScalarSerializerIdentity || contract.CanonicalProfile != "c14n-1" {
		return fmt.Errorf("custom scalar %q uses an unsupported v1 validator, serializer, or canonical profile", typeID)
	}
	switch contract.Canonicalizer {
	case ScalarCanonicalJSON:
	case ScalarCanonicalASCIILowercaseString:
		if len(contract.AcceptedWireShapes) != 1 || contract.AcceptedWireShapes[0] != JSONString {
			return fmt.Errorf("custom scalar %q ASCII lowercase profile requires only the string wire shape", typeID)
		}
	default:
		return fmt.Errorf("custom scalar %q uses unsupported canonicalizer %q", typeID, contract.Canonicalizer)
	}
	if err := validateScalarLimits(typeID, contract.Limits); err != nil {
		return err
	}
	if len(contract.Conformance) < 2 {
		return fmt.Errorf("custom scalar %q requires at least two conformance vectors", typeID)
	}
	return nil
}

func validateScalarLimits(typeID TypeID, limits ScalarLimits) error {
	for _, item := range []struct {
		name  string
		value int
	}{
		{name: "maxBytes", value: limits.MaxBytes},
		{name: "maxDepth", value: limits.MaxDepth},
		{name: "maxMembers", value: limits.MaxMembers},
		{name: "maxArrayItems", value: limits.MaxArrayItems},
		{name: "maxStringBytes", value: limits.MaxStringBytes},
		{name: "maxNumberBytes", value: limits.MaxNumberBytes},
		{name: "maxTokens", value: limits.MaxTokens},
	} {
		if item.value < 0 {
			return fmt.Errorf("custom scalar %q has negative %s limit", typeID, item.name)
		}
	}
	return nil
}

func (r scalarRuntime) verifyConformance(typeID TypeID) error {
	seen := make(map[string][]byte, len(r.contract.Conformance))
	for index, vector := range r.contract.Conformance {
		expected, err := protocol.CanonicalizeJSON(vector.Canonical, r.contract.protocolLimits())
		if err != nil || !bytes.Equal(expected, vector.Canonical) {
			return fmt.Errorf("custom scalar %q conformance vector %d has non-canonical expected output", typeID, index)
		}
		actual, err := r.canonicalize(vector.Input)
		if err != nil || !bytes.Equal(actual, expected) {
			return fmt.Errorf("custom scalar %q fails conformance vector %d", typeID, index)
		}
		key, err := protocol.CanonicalizeJSON(vector.Input, r.contract.protocolLimits())
		if err != nil {
			return fmt.Errorf("custom scalar %q conformance vector %d input: %w", typeID, index, err)
		}
		if prior, exists := seen[string(key)]; exists && !bytes.Equal(prior, expected) {
			return fmt.Errorf("custom scalar %q has conflicting conformance vectors", typeID)
		}
		seen[string(key)] = append([]byte(nil), expected...)
	}
	return nil
}

func (r scalarRuntime) canonicalize(raw json.RawMessage) ([]byte, error) {
	if err := validateScalarWire(r.contract, raw); err != nil {
		return nil, err
	}
	var canonical []byte
	var err error
	switch r.contract.Canonicalizer {
	case ScalarCanonicalJSON:
		canonical, err = protocol.CanonicalizeJSON(raw, r.contract.protocolLimits())
	case ScalarCanonicalASCIILowercaseString:
		canonical, err = canonicalizeASCIILowercaseString(raw, r.contract.protocolLimits())
	default:
		return nil, fmt.Errorf("unsupported custom scalar canonicalizer %q", r.contract.Canonicalizer)
	}
	if err != nil {
		return nil, err
	}
	if err := validateScalarWire(r.contract, canonical); err != nil {
		return nil, fmt.Errorf("canonicalizer returned invalid wire value: %w", err)
	}
	return canonical, nil
}

func canonicalizeASCIILowercaseString(raw json.RawMessage, limits protocol.Limits) ([]byte, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("decode string custom scalar: %w", err)
	}
	runes := []rune(value)
	for index, current := range runes {
		if current >= 'A' && current <= 'Z' {
			runes[index] = current + ('a' - 'A')
		}
	}
	encoded, err := json.Marshal(string(runes))
	if err != nil {
		return nil, fmt.Errorf("encode string custom scalar: %w", err)
	}
	return protocol.CanonicalizeJSON(encoded, limits)
}

func validateScalarWire(contract ScalarDescriptor, raw json.RawMessage) error {
	if err := protocol.ValidateJSON(raw, contract.protocolLimits()); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	shape := shapeOfJSON(bytes.TrimSpace(raw))
	if !slices.Contains(contract.AcceptedWireShapes, shape) {
		return fmt.Errorf("wire shape %q is not accepted", shape)
	}
	return nil
}

func shapeOfJSON(raw []byte) JSONShape {
	if len(raw) == 0 {
		return ""
	}
	switch raw[0] {
	case 't', 'f':
		return JSONBoolean
	case '"':
		return JSONString
	case '[':
		return JSONArray
	case '{':
		return JSONObject
	case 'n':
		// Nullability belongs to the containing field or element contract. A
		// present custom scalar value is never the JSON null literal.
		return ""
	default:
		return JSONNumber
	}
}

func (s ScalarDescriptor) protocolLimits() protocol.Limits {
	return protocol.Limits{
		MaxBytes:       s.Limits.MaxBytes,
		MaxDepth:       s.Limits.MaxDepth,
		MaxMembers:     s.Limits.MaxMembers,
		MaxArrayItems:  s.Limits.MaxArrayItems,
		MaxStringBytes: s.Limits.MaxStringBytes,
		MaxNumberBytes: s.Limits.MaxNumberBytes,
		MaxTokens:      s.Limits.MaxTokens,
	}
}

func cloneScalarDescriptor(input ScalarDescriptor) ScalarDescriptor {
	result := input
	result.AcceptedWireShapes = append([]JSONShape(nil), input.AcceptedWireShapes...)
	result.Conformance = make([]ScalarConformanceVector, len(input.Conformance))
	for index, vector := range input.Conformance {
		result.Conformance[index] = ScalarConformanceVector{
			Input:     append(json.RawMessage(nil), vector.Input...),
			Canonical: append(json.RawMessage(nil), vector.Canonical...),
		}
	}
	return result
}

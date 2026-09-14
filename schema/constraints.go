package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/valksor/naatre/protocol"
)

const (
	// ConstraintTraitID identifies the portable core validation metadata.
	ConstraintTraitID         = "naatre.constraints-1"
	maxConstraintViolations   = 32
	maxConstraintPatternBytes = 1024
	maxConstraintRules        = 32
	maxConstraintRuleDepth    = 16
	maxConstraintRuleNodes    = 128
)

type LengthUnit string

const (
	LengthUnicodeScalar LengthUnit = "unicode-scalar"
	LengthBytes         LengthUnit = "bytes"
)

type PatternMode string

const (
	PatternFull   PatternMode = "full"
	PatternSearch PatternMode = "search"
)

type FormatConstraint struct {
	ID        string `json:"id"`
	Assertion bool   `json:"assertion,omitempty"`
}

type RuleOperator string

const (
	RulePresent  RuleOperator = "present"
	RuleAbsent   RuleOperator = "absent"
	RuleEqual    RuleOperator = "eq"
	RuleNotEqual RuleOperator = "ne"
	RuleAnd      RuleOperator = "and"
	RuleOr       RuleOperator = "or"
	RuleNot      RuleOperator = "not"
)

type RuleExpression struct {
	Operator RuleOperator     `json:"operator"`
	Field    string           `json:"field,omitempty"`
	Value    json.RawMessage  `json:"value,omitempty"`
	Children []RuleExpression `json:"children,omitempty"`
}

type ConstraintRule struct {
	ID     string         `json:"id"`
	Assert RuleExpression `json:"assert"`
}

// ConstraintSet is the closed, portable validation vocabulary. Numeric values
// remain json.Number so schema import/export never narrows them through float64.
type ConstraintSet struct {
	Minimum          *json.Number      `json:"minimum,omitempty"`
	Maximum          *json.Number      `json:"maximum,omitempty"`
	ExclusiveMinimum bool              `json:"exclusiveMinimum,omitempty"`
	ExclusiveMaximum bool              `json:"exclusiveMaximum,omitempty"`
	Precision        *int              `json:"precision,omitempty"`
	Scale            *int              `json:"scale,omitempty"`
	MinLength        *int              `json:"minLength,omitempty"`
	MaxLength        *int              `json:"maxLength,omitempty"`
	LengthUnit       LengthUnit        `json:"lengthUnit,omitempty"`
	Pattern          string            `json:"pattern,omitempty"`
	PatternMode      PatternMode       `json:"patternMode,omitempty"`
	Format           *FormatConstraint `json:"format,omitempty"`
	MinItems         *int              `json:"minItems,omitempty"`
	MaxItems         *int              `json:"maxItems,omitempty"`
	UniqueItems      bool              `json:"uniqueItems,omitempty"`
	MinProperties    *int              `json:"minProperties,omitempty"`
	MaxProperties    *int              `json:"maxProperties,omitempty"`
	KeyPattern       string            `json:"keyPattern,omitempty"`
	Rules            []ConstraintRule  `json:"rules,omitempty"`
}

type ConstraintViolation struct {
	ID         string            `json:"id"`
	Code       string            `json:"code"`
	Path       string            `json:"path"`
	Parameters map[string]string `json:"parameters,omitempty"`
}

type ConstraintError struct {
	violations []ConstraintViolation
}

// ConstraintMetadataError locates unsupported or malformed constraint
// metadata without echoing the metadata value into a public error.
type ConstraintMetadataError struct {
	Code    string `json:"code"`
	Pointer string `json:"pointer"`
	Keyword string `json:"keyword,omitempty"`
	Message string `json:"message"`
}

func (e *ConstraintMetadataError) Error() string { return "portable constraint metadata is invalid" }

func (e *ConstraintError) Error() string { return "input violates portable constraints" }

func (e *ConstraintError) Violations() []ConstraintViolation {
	return mapSlice(e.violations, func(violation ConstraintViolation) ConstraintViolation {
		violation.Parameters = maps.Clone(violation.Parameters)
		return violation
	})
}

// ConstraintTrait encodes validated portable constraints as a critical schema
// trait. The caller receives an error instead of a partially valid descriptor.
func ConstraintTrait(constraints ConstraintSet) (TraitDescriptor, error) {
	if err := validateConstraintSet(constraints); err != nil {
		return TraitDescriptor{}, err
	}
	value, err := json.Marshal(constraints)
	if err != nil {
		return TraitDescriptor{}, fmt.Errorf("encode constraints: %w", err)
	}
	return TraitDescriptor{ID: ConstraintTraitID, Semantics: TraitValidation, Value: value}, nil
}

// ParseConstraintTrait returns the one portable constraint set in traits.
func ParseConstraintTrait(traits []TraitDescriptor) (ConstraintSet, bool, error) {
	for _, trait := range traits {
		if trait.ID != ConstraintTraitID {
			continue
		}
		if trait.Semantics != TraitValidation {
			return ConstraintSet{}, false, fmt.Errorf("constraint trait has semantics %q", trait.Semantics)
		}
		constraints, err := decodeConstraintSet(trait.Value)
		return constraints, true, err
	}
	return ConstraintSet{}, false, nil
}

func validateConstraintTraits(descriptor TypeDescriptor) error {
	if _, _, err := ParseConstraintTrait(descriptor.Traits); err != nil {
		return fmt.Errorf("schema type %q: %w", descriptor.ID, err)
	}
	for _, name := range sortedKeys(descriptor.Fields) {
		if _, _, err := ParseConstraintTrait(descriptor.Fields[name].Traits); err != nil {
			return fmt.Errorf("schema type %q field %q: %w", descriptor.ID, name, err)
		}
	}
	return nil
}

func validateCatalogConstraintApplicability(types map[TypeID]TypeDescriptor) error {
	for _, identifier := range sortedKeys(types) {
		descriptor := types[identifier]
		if err := validateAppliedConstraintTraits(descriptor, descriptor.Traits); err != nil {
			return fmt.Errorf("schema type %q: %w", descriptor.ID, err)
		}
		for _, name := range sortedKeys(descriptor.Fields) {
			field := descriptor.Fields[name]
			if err := validateAppliedConstraintTraits(types[field.Type], field.Traits); err != nil {
				return fmt.Errorf("schema type %q field %q: %w", descriptor.ID, name, err)
			}
		}
	}
	return nil
}

func validateAppliedConstraintTraits(target TypeDescriptor, traits []TraitDescriptor) error {
	constraints, present, err := ParseConstraintTrait(traits)
	if err != nil || !present {
		return err
	}
	return validateConstraintApplicability(target, constraints)
}

func validateConstraintApplicability(target TypeDescriptor, constraints ConstraintSet) error {
	numericTarget := slices.Contains([]TypeID{
		TypeID(Int32), TypeID(Float64), TypeID(Int64), TypeID(UInt64),
		TypeID(BigInt), TypeID(Decimal), TypeID(Duration),
	}, target.ID)
	checks := []struct {
		present bool
		allowed bool
		family  string
	}{
		{hasNumericBounds(constraints), numericTarget, "numeric bounds"},
		{hasDecimalDimensions(constraints), target.ID == TypeID(Decimal), "decimal precision and scale"},
		{hasStringConstraints(constraints), target.ID == TypeID(String) || target.ID == TypeID(ID), "string constraints"},
		{hasItemConstraints(constraints), target.Kind == ListType, "item constraints"},
		{hasPropertyConstraints(constraints), slices.Contains([]TypeKind{MapType, ObjectType, InputObjectType, OneOfType}, target.Kind), "property constraints"},
		{constraints.KeyPattern != "", target.Kind == MapType, "keyPattern"},
		{len(constraints.Rules) != 0, slices.Contains([]TypeKind{ObjectType, InputObjectType, OneOfType}, target.Kind), "cross-field rules"},
	}
	for _, check := range checks {
		if check.present && !check.allowed {
			return fmt.Errorf("%s do not apply to %s type %q", check.family, target.Kind, target.ID)
		}
	}
	return validateDeclaredRuleFields(target, constraints.Rules)
}

func hasNumericConstraints(constraints ConstraintSet) bool {
	return hasNumericBounds(constraints) || hasDecimalDimensions(constraints)
}

func hasNumericBounds(constraints ConstraintSet) bool {
	return constraints.Minimum != nil || constraints.Maximum != nil
}

func hasDecimalDimensions(constraints ConstraintSet) bool {
	return constraints.Precision != nil || constraints.Scale != nil
}

func hasStringConstraints(constraints ConstraintSet) bool {
	return constraints.MinLength != nil || constraints.MaxLength != nil || constraints.Pattern != "" || constraints.Format != nil
}

func hasItemConstraints(constraints ConstraintSet) bool {
	return constraints.MinItems != nil || constraints.MaxItems != nil || constraints.UniqueItems
}

func hasPropertyConstraints(constraints ConstraintSet) bool {
	return constraints.MinProperties != nil || constraints.MaxProperties != nil
}

func validateDeclaredRuleFields(target TypeDescriptor, rules []ConstraintRule) error {
	for _, rule := range rules {
		if field := firstUndeclaredRuleField(rule.Assert, target.Fields); field != "" {
			return fmt.Errorf("constraint rule %q references undeclared field %q", rule.ID, field)
		}
	}
	return nil
}

func firstUndeclaredRuleField(expression RuleExpression, fields map[string]FieldDescriptor) string {
	if expression.Field != "" {
		if _, declared := fields[expression.Field]; !declared {
			return expression.Field
		}
	}
	for _, child := range expression.Children {
		if field := firstUndeclaredRuleField(child, fields); field != "" {
			return field
		}
	}
	return ""
}

// ValidateConstraintValue validates already well-formed JSON against the
// portable constraint trait. Missing and null values are intentionally outside
// constraint evaluation and are handled by coercion presence rules.
func ValidateConstraintValue(raw json.RawMessage, traits []TraitDescriptor) error {
	constraints, present, err := ParseConstraintTrait(traits)
	if err != nil || !present || raw == nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return err
	}
	violations := validateConstraintValue(raw, constraints)
	if len(violations) == 0 {
		return nil
	}
	return &ConstraintError{violations: violations}
}

func decodeConstraintSet(raw json.RawMessage) (ConstraintSet, error) {
	if err := protocol.ValidateJSON(raw, protocol.Limits{MaxBytes: 64 << 10, MaxDepth: 32, MaxMembers: 512, MaxArrayItems: 512}); err != nil {
		return ConstraintSet{}, fmt.Errorf("invalid constraint metadata: %w", err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return ConstraintSet{}, &ConstraintMetadataError{Code: "CONSTRAINT_METADATA_TYPE", Message: "constraint metadata requires an object"}
	}
	known := map[string]bool{
		"minimum": true, "maximum": true, "exclusiveMinimum": true, "exclusiveMaximum": true,
		"precision": true, "scale": true, "minLength": true, "maxLength": true,
		"lengthUnit": true, "pattern": true, "patternMode": true, "format": true,
		"minItems": true, "maxItems": true, "uniqueItems": true,
		"minProperties": true, "maxProperties": true, "keyPattern": true, "rules": true,
	}
	for _, keyword := range sortedKeys(members) {
		if known[keyword] {
			continue
		}
		code := "CONSTRAINT_UNKNOWN_KEYWORD"
		if keyword == "cel" {
			code = "CONSTRAINT_FEATURE_UNSUPPORTED"
		}
		return ConstraintSet{}, &ConstraintMetadataError{
			Code: code, Pointer: "/" + escapeJSONPointer(keyword), Keyword: keyword,
			Message: "keyword is outside naatre.constraints-1",
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var constraints ConstraintSet
	if err := decoder.Decode(&constraints); err != nil {
		return ConstraintSet{}, fmt.Errorf("invalid constraint metadata: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ConstraintSet{}, fmt.Errorf("constraint metadata has trailing content")
	}
	if err := validateConstraintSet(constraints); err != nil {
		return ConstraintSet{}, err
	}
	return constraints, nil
}

func validateConstraintSet(constraints ConstraintSet) error {
	if err := validateConstraintLimits(constraints); err != nil {
		return err
	}
	if err := validateConstraintRanges(constraints); err != nil {
		return err
	}
	if err := validateConstraintModes(constraints); err != nil {
		return err
	}
	if err := validateConstraintPatterns(constraints); err != nil {
		return err
	}
	if err := validateConstraintFormat(constraints); err != nil {
		return err
	}
	return validateConstraintRules(constraints)
}

func validateConstraintLimits(constraints ConstraintSet) error {
	for name, value := range map[string]*int{
		"precision": constraints.Precision, "scale": constraints.Scale,
		"minLength": constraints.MinLength, "maxLength": constraints.MaxLength,
		"minItems": constraints.MinItems, "maxItems": constraints.MaxItems,
		"minProperties": constraints.MinProperties, "maxProperties": constraints.MaxProperties,
	} {
		if value != nil && *value < 0 {
			return fmt.Errorf("constraint %s cannot be negative", name)
		}
	}
	return nil
}

func validateConstraintModes(constraints ConstraintSet) error {
	if constraints.ExclusiveMinimum && constraints.Minimum == nil {
		return fmt.Errorf("exclusiveMinimum requires minimum")
	}
	if constraints.ExclusiveMaximum && constraints.Maximum == nil {
		return fmt.Errorf("exclusiveMaximum requires maximum")
	}
	if constraints.LengthUnit != "" && constraints.MinLength == nil && constraints.MaxLength == nil {
		return fmt.Errorf("lengthUnit requires a string length constraint")
	}
	if constraints.PatternMode != "" && constraints.Pattern == "" && constraints.KeyPattern == "" {
		return fmt.Errorf("patternMode requires pattern or keyPattern")
	}
	if constraints.LengthUnit != "" && constraints.LengthUnit != LengthUnicodeScalar && constraints.LengthUnit != LengthBytes {
		return fmt.Errorf("unknown string length unit %q", constraints.LengthUnit)
	}
	if constraints.PatternMode != "" && constraints.PatternMode != PatternFull && constraints.PatternMode != PatternSearch {
		return fmt.Errorf("unknown pattern mode %q", constraints.PatternMode)
	}
	return nil
}

func validateConstraintPatterns(constraints ConstraintSet) error {
	for name, pattern := range map[string]string{"pattern": constraints.Pattern, "keyPattern": constraints.KeyPattern} {
		if len(pattern) > maxConstraintPatternBytes {
			return fmt.Errorf("constraint %s exceeds pattern size limit", name)
		}
		if pattern != "" {
			if _, err := compileConstraintPattern(pattern, constraints.PatternMode); err != nil {
				return fmt.Errorf("constraint %s: %w", name, err)
			}
		}
	}
	return nil
}

func validateConstraintFormat(constraints ConstraintSet) error {
	if constraints.Format != nil {
		if constraints.Format.ID == "" || len(constraints.Format.ID) > 128 {
			return fmt.Errorf("constraint format identifier is invalid")
		}
		if constraints.Format.Assertion && !slices.Contains([]string{"uuid", "timestamp", "uri", "email"}, constraints.Format.ID) {
			return fmt.Errorf("asserting format %q is unsupported", constraints.Format.ID)
		}
	}
	return nil
}

func validateConstraintRules(constraints ConstraintSet) error {
	if len(constraints.Rules) > maxConstraintRules {
		return fmt.Errorf("constraint rule count exceeds %d", maxConstraintRules)
	}
	seen := make(map[string]bool, len(constraints.Rules))
	for _, rule := range constraints.Rules {
		if !typeIDPattern.MatchString(rule.ID) || seen[rule.ID] {
			return fmt.Errorf("constraint rule identifier %q is invalid or duplicate", rule.ID)
		}
		seen[rule.ID] = true
		nodes := 0
		if err := validateRuleExpression(rule.Assert, 1, &nodes); err != nil {
			return fmt.Errorf("constraint rule %q: %w", rule.ID, err)
		}
	}
	return nil
}

func validateConstraintRanges(constraints ConstraintSet) error {
	for _, pair := range [][3]any{
		{"length", constraints.MinLength, constraints.MaxLength},
		{"items", constraints.MinItems, constraints.MaxItems},
		{"properties", constraints.MinProperties, constraints.MaxProperties},
	} {
		minimum, maximum := pair[1].(*int), pair[2].(*int)
		if minimum != nil && maximum != nil && *minimum > *maximum {
			return fmt.Errorf("constraint %s minimum exceeds maximum", pair[0])
		}
	}
	if constraints.Scale != nil && constraints.Precision != nil && *constraints.Scale > *constraints.Precision {
		return fmt.Errorf("constraint scale exceeds precision")
	}
	return validateNumericConstraintRange(constraints)
}

func validateNumericConstraintRange(constraints ConstraintSet) error {
	if constraints.Minimum != nil {
		if _, ok := exactDecimalRat(constraints.Minimum.String()); !ok {
			return fmt.Errorf("constraint minimum is not an exact decimal")
		}
	}
	if constraints.Maximum != nil {
		if _, ok := exactDecimalRat(constraints.Maximum.String()); !ok {
			return fmt.Errorf("constraint maximum is not an exact decimal")
		}
	}
	if constraints.Minimum != nil && constraints.Maximum != nil {
		minimum, _ := exactDecimalRat(constraints.Minimum.String())
		maximum, _ := exactDecimalRat(constraints.Maximum.String())
		if minimum.Cmp(maximum) > 0 {
			return fmt.Errorf("constraint numeric minimum exceeds maximum")
		}
	}
	return nil
}

func validateRuleExpression(expression RuleExpression, depth int, nodes *int) error {
	*nodes++
	if depth > maxConstraintRuleDepth || *nodes > maxConstraintRuleNodes {
		return fmt.Errorf("rule expression exceeds resource limits")
	}
	switch expression.Operator {
	case RulePresent, RuleAbsent, RuleEqual, RuleNotEqual:
		return validateRuleLeaf(expression)
	case RuleAnd, RuleOr:
		return validateRuleBranch(expression, depth, nodes, 2)
	case RuleNot:
		return validateRuleBranch(expression, depth, nodes, 1)
	default:
		return fmt.Errorf("unknown rule operator %q", expression.Operator)
	}
}

func validateRuleLeaf(expression RuleExpression) error {
	if !typeIDPattern.MatchString(expression.Field) || len(expression.Children) != 0 {
		return fmt.Errorf("%s requires one field and no children", expression.Operator)
	}
	needsValue := expression.Operator == RuleEqual || expression.Operator == RuleNotEqual
	if needsValue != (expression.Value != nil) || (expression.Value != nil && !json.Valid(expression.Value)) {
		return fmt.Errorf("%s has an invalid value", expression.Operator)
	}
	return nil
}

func validateRuleBranch(expression RuleExpression, depth int, nodes *int, requiredChildren int) error {
	if expression.Field != "" || expression.Value != nil {
		return fmt.Errorf("%s cannot declare field or value", expression.Operator)
	}
	if len(expression.Children) < requiredChildren || (expression.Operator == RuleNot && len(expression.Children) != requiredChildren) {
		return fmt.Errorf("%s requires %d child expressions", expression.Operator, requiredChildren)
	}
	for _, child := range expression.Children {
		if err := validateRuleExpression(child, depth+1, nodes); err != nil {
			return err
		}
	}
	return nil
}

func validateConstraintValue(raw json.RawMessage, constraints ConstraintSet) []ConstraintViolation {
	collector := constraintCollector{}
	validateNumericConstraints(raw, constraints, &collector)
	validateStringConstraints(raw, constraints, &collector)
	validateCollectionConstraints(raw, constraints, &collector)
	validateObjectConstraints(raw, constraints, &collector)
	sort.Slice(collector.violations, func(left, right int) bool {
		if collector.violations[left].Path != collector.violations[right].Path {
			return collector.violations[left].Path < collector.violations[right].Path
		}
		return collector.violations[left].ID < collector.violations[right].ID
	})
	return collector.violations
}

type constraintCollector struct {
	violations []ConstraintViolation
}

func (c *constraintCollector) add(id, code, path string, parameters map[string]string) {
	if len(c.violations) >= maxConstraintViolations {
		return
	}
	c.violations = append(c.violations, ConstraintViolation{ID: id, Code: code, Path: path, Parameters: maps.Clone(parameters)})
}

func validateNumericConstraints(raw json.RawMessage, constraints ConstraintSet, collector *constraintCollector) {
	if !hasNumericConstraints(constraints) {
		return
	}
	text, ok := constraintNumberText(raw)
	if !ok {
		collector.add("numeric", "CONSTRAINT_TYPE", "", nil)
		return
	}
	value, ok := exactDecimalRat(text)
	if !ok {
		collector.add("numeric", "CONSTRAINT_TYPE", "", nil)
		return
	}
	validateNumericBounds(value, constraints, collector)
	validateDecimalDimensions(text, constraints, collector)
}

func validateNumericBounds(value *big.Rat, constraints ConstraintSet, collector *constraintCollector) {
	if constraints.Minimum != nil {
		minimum, _ := exactDecimalRat(constraints.Minimum.String())
		comparison := value.Cmp(minimum)
		if comparison < 0 || (comparison == 0 && constraints.ExclusiveMinimum) {
			collector.add("minimum", "CONSTRAINT_MINIMUM", "", map[string]string{"limit": constraints.Minimum.String(), "exclusive": strconv.FormatBool(constraints.ExclusiveMinimum)})
		}
	}
	if constraints.Maximum != nil {
		maximum, _ := exactDecimalRat(constraints.Maximum.String())
		comparison := value.Cmp(maximum)
		if comparison > 0 || (comparison == 0 && constraints.ExclusiveMaximum) {
			collector.add("maximum", "CONSTRAINT_MAXIMUM", "", map[string]string{"limit": constraints.Maximum.String(), "exclusive": strconv.FormatBool(constraints.ExclusiveMaximum)})
		}
	}
}

func validateDecimalDimensions(text string, constraints ConstraintSet, collector *constraintCollector) {
	precision, scale, ok := decimalShape(text)
	if constraints.Precision != nil && (!ok || precision > *constraints.Precision) {
		collector.add("precision", "CONSTRAINT_PRECISION", "", map[string]string{"limit": strconv.Itoa(*constraints.Precision)})
	}
	if constraints.Scale != nil && (!ok || scale > *constraints.Scale) {
		collector.add("scale", "CONSTRAINT_SCALE", "", map[string]string{"limit": strconv.Itoa(*constraints.Scale)})
	}
}

func validateStringConstraints(raw json.RawMessage, constraints ConstraintSet, collector *constraintCollector) {
	if !hasStringConstraints(constraints) {
		return
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || !utf8.ValidString(value) {
		collector.add("string", "CONSTRAINT_TYPE", "", nil)
		return
	}
	validateStringLength(value, constraints, collector)
	validateStringPattern(value, constraints, collector)
	validateStringFormat(value, constraints, collector)
}

func validateStringLength(value string, constraints ConstraintSet, collector *constraintCollector) {
	length := utf8.RuneCountInString(value)
	if constraints.LengthUnit == LengthBytes {
		length = len(value)
	}
	if constraints.MinLength != nil && length < *constraints.MinLength {
		collector.add("minLength", "CONSTRAINT_MIN_LENGTH", "", map[string]string{"limit": strconv.Itoa(*constraints.MinLength), "unit": normalizedLengthUnit(constraints.LengthUnit)})
	}
	if constraints.MaxLength != nil && length > *constraints.MaxLength {
		collector.add("maxLength", "CONSTRAINT_MAX_LENGTH", "", map[string]string{"limit": strconv.Itoa(*constraints.MaxLength), "unit": normalizedLengthUnit(constraints.LengthUnit)})
	}
}

func validateStringPattern(value string, constraints ConstraintSet, collector *constraintCollector) {
	if constraints.Pattern == "" {
		return
	}
	pattern, _ := compileConstraintPattern(constraints.Pattern, constraints.PatternMode)
	if !pattern.MatchString(value) {
		collector.add("pattern", "CONSTRAINT_PATTERN", "", map[string]string{"mode": normalizedPatternMode(constraints.PatternMode)})
	}
}

func validateStringFormat(value string, constraints ConstraintSet, collector *constraintCollector) {
	if constraints.Format != nil && constraints.Format.Assertion && !validConstraintFormat(constraints.Format.ID, value) {
		collector.add("format", "CONSTRAINT_FORMAT", "", map[string]string{"format": constraints.Format.ID})
	}
}

func validateCollectionConstraints(raw json.RawMessage, constraints ConstraintSet, collector *constraintCollector) {
	if !hasItemConstraints(constraints) {
		return
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		collector.add("items", "CONSTRAINT_TYPE", "", nil)
		return
	}
	validateItemCardinality(len(items), constraints, collector)
	validateUniqueItems(items, constraints.UniqueItems, collector)
}

func validateItemCardinality(count int, constraints ConstraintSet, collector *constraintCollector) {
	if constraints.MinItems != nil && count < *constraints.MinItems {
		collector.add("minItems", "CONSTRAINT_MIN_ITEMS", "", map[string]string{"limit": strconv.Itoa(*constraints.MinItems)})
	}
	if constraints.MaxItems != nil && count > *constraints.MaxItems {
		collector.add("maxItems", "CONSTRAINT_MAX_ITEMS", "", map[string]string{"limit": strconv.Itoa(*constraints.MaxItems)})
	}
}

func validateUniqueItems(items []json.RawMessage, enabled bool, collector *constraintCollector) {
	if !enabled {
		return
	}
	seen := make(map[string]bool, len(items))
	for index, item := range items {
		canonical, err := protocol.CanonicalizeJSON(item, protocol.Limits{})
		if err != nil {
			continue
		}
		key := string(canonical)
		if seen[key] {
			collector.add("uniqueItems", "CONSTRAINT_UNIQUE_ITEMS", "/"+strconv.Itoa(index), nil)
			return
		}
		seen[key] = true
	}
}

func validateObjectConstraints(raw json.RawMessage, constraints ConstraintSet, collector *constraintCollector) {
	if !hasPropertyConstraints(constraints) && constraints.KeyPattern == "" && len(constraints.Rules) == 0 {
		return
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		collector.add("properties", "CONSTRAINT_TYPE", "", nil)
		return
	}
	validatePropertyCardinality(len(members), constraints, collector)
	validateMapKeys(members, constraints, collector)
	validateCrossFieldRules(members, constraints.Rules, collector)
}

func validatePropertyCardinality(count int, constraints ConstraintSet, collector *constraintCollector) {
	if constraints.MinProperties != nil && count < *constraints.MinProperties {
		collector.add("minProperties", "CONSTRAINT_MIN_PROPERTIES", "", map[string]string{"limit": strconv.Itoa(*constraints.MinProperties)})
	}
	if constraints.MaxProperties != nil && count > *constraints.MaxProperties {
		collector.add("maxProperties", "CONSTRAINT_MAX_PROPERTIES", "", map[string]string{"limit": strconv.Itoa(*constraints.MaxProperties)})
	}
}

func validateMapKeys(members map[string]json.RawMessage, constraints ConstraintSet, collector *constraintCollector) {
	if constraints.KeyPattern == "" {
		return
	}
	pattern, _ := compileConstraintPattern(constraints.KeyPattern, constraints.PatternMode)
	for _, key := range sortedKeys(members) {
		if !pattern.MatchString(key) {
			collector.add("keyPattern", "CONSTRAINT_KEY_PATTERN", "/"+escapeJSONPointer(key), nil)
		}
	}
}

func validateCrossFieldRules(members map[string]json.RawMessage, rules []ConstraintRule, collector *constraintCollector) {
	for _, rule := range rules {
		if !evaluateConstraintRule(rule.Assert, members) {
			collector.add(rule.ID, "CONSTRAINT_RULE", "", nil)
		}
	}
}

/*
	The validation phases above stay separate so adding one constraint family
	cannot reorder or short-circuit another family. Keep the exact evaluation
	order in validateConstraintValue.
*/

func evaluateConstraintRule(expression RuleExpression, members map[string]json.RawMessage) bool {
	value, present := members[expression.Field]
	switch expression.Operator {
	case RulePresent:
		return present
	case RuleAbsent:
		return !present
	case RuleEqual, RuleNotEqual:
		if !present {
			return expression.Operator == RuleNotEqual
		}
		left, leftErr := protocol.CanonicalizeJSON(value, protocol.Limits{})
		right, rightErr := protocol.CanonicalizeJSON(expression.Value, protocol.Limits{})
		equal := leftErr == nil && rightErr == nil && bytes.Equal(left, right)
		if expression.Operator == RuleEqual {
			return equal
		}
		return !equal
	case RuleAnd:
		return allRuleChildren(expression.Children, members, true)
	case RuleOr:
		return allRuleChildren(expression.Children, members, false)
	case RuleNot:
		return !evaluateConstraintRule(expression.Children[0], members)
	default:
		return false
	}
}

func allRuleChildren(children []RuleExpression, members map[string]json.RawMessage, conjunction bool) bool {
	for _, child := range children {
		matched := evaluateConstraintRule(child, members)
		if conjunction != matched {
			return !conjunction
		}
	}
	return conjunction
}

func compileConstraintPattern(pattern string, mode PatternMode) (*regexp.Regexp, error) {
	if mode == "" || mode == PatternFull {
		pattern = "^(?:" + pattern + ")$"
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("pattern is outside the RE2-compatible subset: %w", err)
	}
	return compiled, nil
}

func validConstraintFormat(format, value string) bool {
	switch format {
	case "uuid":
		encoded, _ := json.Marshal(value)
		_, err := canonicalUUID(encoded)
		return err == nil
	case "timestamp":
		encoded, _ := json.Marshal(value)
		_, err := canonicalTimestamp(encoded)
		return err == nil
	case "uri":
		parsed, err := url.Parse(value)
		return err == nil && parsed.IsAbs() && parsed.Scheme != "" && parsed.Host != ""
	case "email":
		return portableEmailPattern.MatchString(value)
	default:
		return false
	}
}

var portableEmailPattern = regexp.MustCompile(`^[A-Za-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+$`)

func constraintNumberText(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return "", false
		}
		return text, true
	}
	return string(trimmed), true
}

func exactDecimalRat(text string) (*big.Rat, bool) {
	match := exactDecimalPattern.FindStringSubmatch(text)
	if match == nil {
		return nil, false
	}
	digits := match[2] + match[3]
	numerator := new(big.Int)
	if _, ok := numerator.SetString(digits, 10); !ok {
		return nil, false
	}
	if match[1] == "-" {
		numerator.Neg(numerator)
	}
	exponent := 0
	if match[4] != "" {
		parsed, err := strconv.Atoi(match[4])
		if err != nil || parsed > 10000 || parsed < -10000 {
			return nil, false
		}
		exponent = parsed
	}
	scale := len(match[3]) - exponent
	if scale <= 0 {
		return new(big.Rat).SetInt(numerator.Mul(numerator, powerOfTen(-scale))), true
	}
	return new(big.Rat).SetFrac(numerator, powerOfTen(scale)), true
}

func powerOfTen(exponent int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exponent)), nil)
}

func decimalShape(text string) (precision, scale int, ok bool) {
	match := decimalShapePattern.FindStringSubmatch(text)
	if match == nil {
		return 0, 0, false
	}
	digits := strings.TrimLeft(match[1]+match[2], "0")
	if digits == "" {
		digits = "0"
	}
	return len(digits), len(match[2]), true
}

var (
	exactDecimalPattern = regexp.MustCompile(`^([+-]?)([0-9]+)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$`)
	decimalShapePattern = regexp.MustCompile(`^[+-]?([0-9]+)(?:\.([0-9]+))?$`)
)

func normalizedLengthUnit(unit LengthUnit) string {
	if unit == "" {
		return string(LengthUnicodeScalar)
	}
	return string(unit)
}

func normalizedPatternMode(mode PatternMode) string {
	if mode == "" {
		return string(PatternFull)
	}
	return string(mode)
}

func prependConstraintPath(err error, token string) error {
	var constraints *ConstraintError
	if !errors.As(err, &constraints) {
		return err
	}
	prefix := "/" + escapeJSONPointer(token)
	violations := constraints.Violations()
	for index := range violations {
		violations[index].Path = prefix + violations[index].Path
	}
	return &ConstraintError{violations: violations}
}

func escapeJSONPointer(value string) string {
	return jsonPointerEscaper.Replace(value)
}

var jsonPointerEscaper = strings.NewReplacer("~", "~0", "/", "~1")

package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/valksor/naatre/protocol"
)

const (
	SchemaDocumentVersion  = "1"
	SchemaCanonicalVersion = "c14n-1"
	// MaxDirectiveCost is the portable per-directive and per-operation cost ceiling.
	MaxDirectiveCost uint64 = 1 << 20
)

var (
	schemaDigestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	directiveVersionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z_.-]{0,127}$`)
	directiveNamePattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

type Deprecation struct {
	Reason          string `json:"reason"`
	Replacement     string `json:"replacement,omitempty"`
	Sunset          string `json:"sunset,omitempty"`
	RemovalRevision string `json:"removalRevision,omitempty"`
}

type EntityDescriptor struct {
	Keys []string `json:"keys"`
}

type SourceMetadata struct {
	URI    string `json:"uri"`
	Line   int    `json:"line,omitempty"`
	Column int    `json:"column,omitempty"`
}

type TraitSemantics string

const (
	TraitDocumentation TraitSemantics = "documentation"
	TraitValidation    TraitSemantics = "validation"
	TraitExecution     TraitSemantics = "execution"
	TraitAuthorization TraitSemantics = "authorization"
	TraitIdentity      TraitSemantics = "identity"
)

type TraitDescriptor struct {
	ID        string          `json:"id"`
	Semantics TraitSemantics  `json:"semantics"`
	Value     json.RawMessage `json:"value"`
}

type RetiredIdentity struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Reason string `json:"reason"`
}

type SchemaReference struct {
	URI      string `json:"uri"`
	Digest   string `json:"digest"`
	Revision string `json:"revision"`
}

type EnumMemberDescriptor struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Deprecation *Deprecation      `json:"deprecation,omitempty"`
	Traits      []TraitDescriptor `json:"traits,omitempty"`
	Source      *SourceMetadata   `json:"source,omitempty"`
}

type VariantMemberDescriptor struct {
	ID          string            `json:"id"`
	Type        TypeID            `json:"type"`
	Description string            `json:"description,omitempty"`
	Deprecation *Deprecation      `json:"deprecation,omitempty"`
	Traits      []TraitDescriptor `json:"traits,omitempty"`
	Source      *SourceMetadata   `json:"source,omitempty"`
}

type FieldDeclaration struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Type        TypeID            `json:"type"`
	Required    bool              `json:"required,omitempty"`
	Nullable    bool              `json:"nullable,omitempty"`
	Default     json.RawMessage   `json:"default,omitempty"`
	Description string            `json:"description,omitempty"`
	Deprecation *Deprecation      `json:"deprecation,omitempty"`
	Cost        uint64            `json:"cost,omitempty"`
	Traits      []TraitDescriptor `json:"traits,omitempty"`
	Source      *SourceMetadata   `json:"source,omitempty"`
}

type DirectivePhase string

const (
	DirectiveValidation DirectivePhase = "validation"
	DirectivePlanning   DirectivePhase = "planning"
	DirectiveExecution  DirectivePhase = "execution"
	DirectiveResponse   DirectivePhase = "response"
)

type DirectiveArgumentDescriptor struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Type        TypeID          `json:"type"`
	Required    bool            `json:"required,omitempty"`
	Nullable    bool            `json:"nullable,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"`
	Description string          `json:"description,omitempty"`
}

// DirectiveDescriptor is the portable, version-pinned contract for one
// language extension. Runtime callbacks never participate in schema identity.
type DirectiveDescriptor struct {
	ID            string                        `json:"id"`
	Name          string                        `json:"name"`
	Version       string                        `json:"version"`
	Capability    string                        `json:"capability"`
	Repeatable    bool                          `json:"repeatable,omitempty"`
	Locations     []protocol.SelectionKind      `json:"locations"`
	Arguments     []DirectiveArgumentDescriptor `json:"arguments,omitempty"`
	Phases        []DirectivePhase              `json:"phases"`
	Effect        string                        `json:"effect"`
	Cost          uint64                        `json:"cost"`
	Deterministic bool                          `json:"deterministic"`
	Compatibility ChangeClassification          `json:"compatibility"`
	Description   string                        `json:"description,omitempty"`
	Deprecation   *Deprecation                  `json:"deprecation,omitempty"`
	Capabilities  []string                      `json:"capabilities,omitempty"`
	Traits        []TraitDescriptor             `json:"traits,omitempty"`
	Source        *SourceMetadata               `json:"source,omitempty"`
}

type TypeDeclaration struct {
	ID              TypeID                    `json:"id"`
	Name            string                    `json:"name"`
	Kind            TypeKind                  `json:"kind"`
	Input           bool                      `json:"input,omitempty"`
	Output          bool                      `json:"output,omitempty"`
	Open            bool                      `json:"open,omitempty"`
	Element         TypeID                    `json:"element,omitempty"`
	ElementNullable bool                      `json:"elementNullable,omitempty"`
	Fields          []FieldDeclaration        `json:"fields,omitempty"`
	VariantMembers  []VariantMemberDescriptor `json:"variantMembers,omitempty"`
	EnumMembers     []EnumMemberDescriptor    `json:"enumMembers,omitempty"`
	MaxDepth        int                       `json:"maxDepth,omitempty"`
	Scalar          *ScalarDescriptor         `json:"scalar,omitempty"`
	Description     string                    `json:"description,omitempty"`
	Deprecation     *Deprecation              `json:"deprecation,omitempty"`
	Entity          *EntityDescriptor         `json:"entity,omitempty"`
	Capabilities    []string                  `json:"capabilities,omitempty"`
	Traits          []TraitDescriptor         `json:"traits,omitempty"`
	Source          *SourceMetadata           `json:"source,omitempty"`
	Retired         []RetiredIdentity         `json:"retired,omitempty"`
}

type OperationDescriptor struct {
	ID                  string                 `json:"id"`
	Name                string                 `json:"name"`
	Kind                protocol.OperationKind `json:"kind"`
	Input               TypeID                 `json:"input,omitempty"`
	InputNullable       bool                   `json:"inputNullable,omitempty"`
	Output              TypeID                 `json:"output"`
	OutputNullable      bool                   `json:"outputNullable,omitempty"`
	Description         string                 `json:"description,omitempty"`
	Deprecation         *Deprecation           `json:"deprecation,omitempty"`
	Effect              string                 `json:"effect"`
	Deterministic       bool                   `json:"deterministic,omitempty"`
	Cacheable           bool                   `json:"cacheable,omitempty"`
	RetrySafe           bool                   `json:"retrySafe,omitempty"`
	ThreadSafety        string                 `json:"threadSafety,omitempty"`
	Batching            string                 `json:"batching,omitempty"`
	Transaction         string                 `json:"transaction,omitempty"`
	AuthorizationPolicy string                 `json:"authorizationPolicy,omitempty"`
	Idempotency         string                 `json:"idempotency,omitempty"`
	Cost                uint64                 `json:"cost,omitempty"`
	ParallelMutation    bool                   `json:"parallelMutation,omitempty"`
	Collection          *CollectionDescriptor  `json:"collection,omitempty"`
	Capabilities        []string               `json:"capabilities,omitempty"`
	Traits              []TraitDescriptor      `json:"traits,omitempty"`
	Source              *SourceMetadata        `json:"source,omitempty"`
}

// CollectionDescriptor is the portable, non-secret portion of a pageable
// collection contract.
type CollectionDescriptor struct {
	MaxPageSize    uint64 `json:"maxPageSize"`
	TotalCountCost uint64 `json:"totalCountCost,omitempty"`
}

type MemberDescriptor struct {
	ID                  string                `json:"id"`
	Name                string                `json:"name"`
	Owner               TypeID                `json:"owner"`
	Kind                string                `json:"kind"`
	Input               TypeID                `json:"input,omitempty"`
	InputNullable       bool                  `json:"inputNullable,omitempty"`
	Output              TypeID                `json:"output"`
	OutputNullable      bool                  `json:"outputNullable,omitempty"`
	Description         string                `json:"description,omitempty"`
	Deprecation         *Deprecation          `json:"deprecation,omitempty"`
	Effect              string                `json:"effect"`
	Deterministic       bool                  `json:"deterministic,omitempty"`
	Cacheable           bool                  `json:"cacheable,omitempty"`
	RetrySafe           bool                  `json:"retrySafe,omitempty"`
	ThreadSafety        string                `json:"threadSafety,omitempty"`
	Batching            string                `json:"batching,omitempty"`
	Transaction         string                `json:"transaction,omitempty"`
	AuthorizationPolicy string                `json:"authorizationPolicy,omitempty"`
	Idempotency         string                `json:"idempotency,omitempty"`
	Cost                uint64                `json:"cost,omitempty"`
	ParallelMutation    bool                  `json:"parallelMutation,omitempty"`
	Collection          *CollectionDescriptor `json:"collection,omitempty"`
	Capabilities        []string              `json:"capabilities,omitempty"`
	Traits              []TraitDescriptor     `json:"traits,omitempty"`
	Source              *SourceMetadata       `json:"source,omitempty"`
}

type ExportOptions struct {
	Revision     string
	Capabilities []string
	Directives   []DirectiveDescriptor
	Retired      []RetiredIdentity
	References   []SchemaReference
	Traits       []TraitDescriptor
}

type ImportOptions struct {
	SupportedTraits map[string]bool
}

type documentWire struct {
	Version          string                `json:"version"`
	CanonicalVersion string                `json:"canonicalVersion"`
	Revision         string                `json:"revision"`
	Capabilities     []string              `json:"capabilities,omitempty"`
	Types            []TypeDeclaration     `json:"types"`
	Operations       []OperationDescriptor `json:"operations"`
	Members          []MemberDescriptor    `json:"members"`
	Directives       []DirectiveDescriptor `json:"directives,omitempty"`
	Retired          []RetiredIdentity     `json:"retired,omitempty"`
	References       []SchemaReference     `json:"references,omitempty"`
	Traits           []TraitDescriptor     `json:"traits,omitempty"`
}

type Document struct {
	wire      documentWire
	canonical []byte
}

func ExportDocument(snapshot Snapshot, operations []OperationDescriptor, members []MemberDescriptor, options ExportOptions) (Document, error) {
	types := make([]TypeDeclaration, 0)
	for _, descriptor := range snapshot.Descriptors() {
		if descriptor.Kind == ScalarType && descriptor.Scalar == nil && knownScalar(ScalarKind(descriptor.ID)) {
			continue
		}
		types = append(types, declarationFromDescriptor(descriptor))
	}
	directives, err := canonicalizeDirectiveDefaults(snapshot, options.Directives)
	if err != nil {
		return Document{}, err
	}
	return buildDocument(documentWire{
		Version: SchemaDocumentVersion, CanonicalVersion: SchemaCanonicalVersion,
		Revision: options.Revision, Capabilities: slices.Clone(options.Capabilities), Types: types,
		Operations: cloneOperations(operations), Members: cloneMembers(members), Directives: directives, Retired: slices.Clone(options.Retired),
		References: slices.Clone(options.References), Traits: cloneTraits(options.Traits),
	}, ImportOptions{SupportedTraits: collectTraitIDs(types, operations, members, directives, options.Traits)})
}

func canonicalizeDirectiveDefaults(snapshot Snapshot, input []DirectiveDescriptor) ([]DirectiveDescriptor, error) {
	result := cloneDirectives(input)
	for directiveIndex := range result {
		for argumentIndex := range result[directiveIndex].Arguments {
			argument := &result[directiveIndex].Arguments[argumentIndex]
			if len(argument.Default) == 0 {
				continue
			}
			value, err := CoerceInput(snapshot, argument.Type, argument.Default, argument.Nullable)
			if err != nil {
				return nil, fmt.Errorf("directive %q argument %q has invalid default: %w", result[directiveIndex].ID, argument.ID, err)
			}
			canonical, err := value.MarshalJSON()
			if err != nil {
				return nil, fmt.Errorf("canonicalize directive %q argument %q default: %w", result[directiveIndex].ID, argument.ID, err)
			}
			argument.Default = canonical
		}
	}
	return result, nil
}

func ParseDocument(input []byte, options ImportOptions) (Document, error) {
	canonical, err := protocol.CanonicalizeSchema(input, protocol.Limits{})
	if err != nil {
		return Document{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	var wire documentWire
	if err := decoder.Decode(&wire); err != nil {
		return Document{}, fmt.Errorf("decode schema document: %w", err)
	}
	return buildDocument(wire, options)
}

func (d Document) Version() string  { return d.wire.Version }
func (d Document) Revision() string { return d.wire.Revision }

func (d Document) Types() []TypeDeclaration          { return cloneTypeDeclarations(d.wire.Types) }
func (d Document) Operations() []OperationDescriptor { return cloneOperations(d.wire.Operations) }
func (d Document) Members() []MemberDescriptor       { return cloneMembers(d.wire.Members) }
func (d Document) Directives() []DirectiveDescriptor { return cloneDirectives(d.wire.Directives) }

// ExportOptions returns a detached copy of the document-level lifecycle and
// extension metadata needed to reproduce this document from a registry.
func (d Document) ExportOptions() ExportOptions {
	options := ExportOptions{Revision: d.wire.Revision}
	options.Capabilities = slices.Clone(d.wire.Capabilities)
	options.Directives = cloneDirectives(d.wire.Directives)
	options.Retired = slices.Clone(d.wire.Retired)
	options.References = slices.Clone(d.wire.References)
	options.Traits = cloneTraits(d.wire.Traits)
	return options
}

func (d Document) CanonicalJSON() ([]byte, error) {
	return cloneCanonical(d.canonical, d.requireInitialized())
}

func cloneCanonical(canonical []byte, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	return bytes.Clone(canonical), nil
}

func (d Document) requireInitialized() error {
	if len(d.canonical) != 0 {
		return nil
	}
	return errors.New("schema document is not initialized")
}

func (d Document) Hash() (protocol.Digest, error) {
	return hashCanonical(d.canonical, protocol.SchemaHash, d.requireInitialized())
}

func hashCanonical(canonical []byte, purpose protocol.HashPurpose, err error) (protocol.Digest, error) {
	if err != nil {
		return protocol.Digest{}, err
	}
	return protocol.SemanticHash(purpose, canonical)
}

func (d Document) Snapshot() (Snapshot, error) {
	catalog := NewCatalog()
	for _, declaration := range d.wire.Types {
		descriptor := descriptorFromDeclaration(declaration)
		var err error
		if descriptor.Kind == ScalarType {
			err = catalog.RegisterScalar(descriptor)
		} else {
			err = catalog.Register(descriptor)
		}
		if err != nil {
			return Snapshot{}, fmt.Errorf("import type %q: %w", declaration.ID, err)
		}
	}
	return catalog.Freeze()
}

func buildDocument(wire documentWire, options ImportOptions) (Document, error) {
	normalizeDocument(&wire)
	if err := validateDocument(wire, options); err != nil {
		return Document{}, err
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return Document{}, fmt.Errorf("marshal schema document: %w", err)
	}
	canonical, err := protocol.CanonicalizeSchema(encoded, protocol.Limits{})
	if err != nil {
		return Document{}, err
	}
	return Document{wire: cloneDocumentWire(wire), canonical: canonical}, nil
}

func normalizeDocument(wire *documentWire) {
	if wire.Types == nil {
		wire.Types = []TypeDeclaration{}
	}
	if wire.Operations == nil {
		wire.Operations = []OperationDescriptor{}
	}
	if wire.Members == nil {
		wire.Members = []MemberDescriptor{}
	}
	wire.Capabilities = sortedUnique(wire.Capabilities)
	wire.Traits = normalizedTraits(wire.Traits)
	for index := range wire.Types {
		normalizeTypeDeclaration(&wire.Types[index])
	}
	for index := range wire.Operations {
		normalizeOperation(&wire.Operations[index])
	}
	for index := range wire.Members {
		normalizeMember(&wire.Members[index])
	}
	for index := range wire.Directives {
		normalizeDirective(&wire.Directives[index])
	}
	sort.Slice(wire.Types, func(i, j int) bool { return wire.Types[i].ID < wire.Types[j].ID })
	sort.Slice(wire.Operations, func(i, j int) bool { return wire.Operations[i].ID < wire.Operations[j].ID })
	sort.Slice(wire.Members, func(i, j int) bool { return wire.Members[i].ID < wire.Members[j].ID })
	sort.Slice(wire.Directives, func(i, j int) bool { return wire.Directives[i].ID < wire.Directives[j].ID })
	sort.Slice(wire.Retired, func(i, j int) bool { return wire.Retired[i].ID < wire.Retired[j].ID })
	sort.Slice(wire.References, func(i, j int) bool {
		if wire.References[i].URI == wire.References[j].URI {
			return wire.References[i].Revision < wire.References[j].Revision
		}
		return wire.References[i].URI < wire.References[j].URI
	})
	if len(wire.Retired) == 0 {
		wire.Retired = nil
	}
	if len(wire.References) == 0 {
		wire.References = nil
	}
}

func normalizeTypeDeclaration(declaration *TypeDeclaration) {
	if declaration.Name == "" {
		declaration.Name = string(declaration.ID)
	}
	normalizeFieldDeclarations(declaration)
	normalizeEnumDeclarations(declaration)
	normalizeVariantDeclarations(declaration)
	declaration.Capabilities = sortedUnique(declaration.Capabilities)
	declaration.Traits = normalizedTraits(declaration.Traits)
	sort.Slice(declaration.Retired, func(i, j int) bool { return declaration.Retired[i].ID < declaration.Retired[j].ID })
	if declaration.Scalar != nil {
		slices.Sort(declaration.Scalar.AcceptedWireShapes)
	}
	if len(declaration.Retired) == 0 {
		declaration.Retired = nil
	}
	if declaration.Entity != nil {
		declaration.Entity.Keys = sortedUnique(declaration.Entity.Keys)
	}
}

func normalizeFieldDeclarations(declaration *TypeDeclaration) {
	for index := range declaration.Fields {
		field := &declaration.Fields[index]
		if field.ID == "" {
			field.ID = string(declaration.ID) + "." + field.Name
		}
		field.Traits = normalizedTraits(field.Traits)
	}
	sort.Slice(declaration.Fields, func(i, j int) bool { return declaration.Fields[i].ID < declaration.Fields[j].ID })
	if len(declaration.Fields) == 0 {
		declaration.Fields = nil
	}
}

func normalizeEnumDeclarations(declaration *TypeDeclaration) {
	for index := range declaration.EnumMembers {
		member := &declaration.EnumMembers[index]
		if member.ID == "" {
			member.ID = string(declaration.ID) + "." + member.Name
		}
		member.Traits = normalizedTraits(member.Traits)
	}
	sort.Slice(declaration.EnumMembers, func(i, j int) bool { return declaration.EnumMembers[i].ID < declaration.EnumMembers[j].ID })
	if len(declaration.EnumMembers) == 0 {
		declaration.EnumMembers = nil
	}
}

func normalizeVariantDeclarations(declaration *TypeDeclaration) {
	for index := range declaration.VariantMembers {
		member := &declaration.VariantMembers[index]
		if member.ID == "" {
			member.ID = string(declaration.ID) + "." + string(member.Type)
		}
		member.Traits = normalizedTraits(member.Traits)
	}
	sort.Slice(declaration.VariantMembers, func(i, j int) bool { return declaration.VariantMembers[i].ID < declaration.VariantMembers[j].ID })
	if len(declaration.VariantMembers) == 0 {
		declaration.VariantMembers = nil
	}
}

func normalizeOperation(operation *OperationDescriptor) {
	operation.Capabilities, operation.Traits = normalizedCallableCollections(operation.Capabilities, operation.Traits)
}

func normalizeMember(member *MemberDescriptor) {
	member.Capabilities, member.Traits = normalizedCallableCollections(member.Capabilities, member.Traits)
}

func normalizedCallableCollections(capabilities []string, traits []TraitDescriptor) ([]string, []TraitDescriptor) {
	return sortedUnique(capabilities), normalizedTraits(traits)
}

func normalizeDirective(directive *DirectiveDescriptor) {
	sort.Slice(directive.Arguments, func(i, j int) bool { return directive.Arguments[i].ID < directive.Arguments[j].ID })
	slices.Sort(directive.Locations)
	slices.Sort(directive.Phases)
	directive.Capabilities = sortedUnique(directive.Capabilities)
	directive.Traits = normalizedTraits(directive.Traits)
	if len(directive.Arguments) == 0 {
		directive.Arguments = nil
	}
}

func validateDocument(wire documentWire, options ImportOptions) error {
	if err := validateDocumentHeader(wire); err != nil {
		return err
	}
	activeIDs, activeNames, err := validateActiveDeclarations(wire, options)
	if err != nil {
		return err
	}
	if err := validateRetiredIdentities(wire.Retired, activeIDs, activeNames); err != nil {
		return err
	}
	if err := validatePinnedReferences(wire.References); err != nil {
		return err
	}
	if err := validateTraits(wire.Traits, options); err != nil {
		return err
	}
	if _, err := (Document{wire: wire}).Snapshot(); err != nil {
		return err
	}
	return validateDocumentReferences(wire)
}

func validateDocumentHeader(wire documentWire) error {
	if wire.Version != SchemaDocumentVersion || wire.CanonicalVersion != SchemaCanonicalVersion {
		return fmt.Errorf("unsupported schema document version %q/%q", wire.Version, wire.CanonicalVersion)
	}
	if !typeIDPattern.MatchString(wire.Revision) {
		return fmt.Errorf("invalid schema revision %q", wire.Revision)
	}
	return nil
}

func validateActiveDeclarations(wire documentWire, options ImportOptions) (map[string]bool, map[string]bool, error) {
	activeIDs := make(map[string]bool)
	activeNames := make(map[string]bool)
	for _, declaration := range wire.Types {
		if err := validateTypeDeclaration(declaration, activeIDs, activeNames, options); err != nil {
			return nil, nil, err
		}
	}
	for _, operation := range wire.Operations {
		if err := validateOperationDescriptor(operation, activeIDs, activeNames, options); err != nil {
			return nil, nil, err
		}
	}
	for _, member := range wire.Members {
		if err := validateMemberDescriptor(member, activeIDs, activeNames, options); err != nil {
			return nil, nil, err
		}
	}
	for _, directive := range wire.Directives {
		if err := validateDirectiveDescriptor(directive, activeIDs, activeNames, options); err != nil {
			return nil, nil, err
		}
	}
	return activeIDs, activeNames, nil
}

func validateDirectiveDescriptor(directive DirectiveDescriptor, activeIDs, activeNames map[string]bool, options ImportOptions) error {
	if !typeIDPattern.MatchString(directive.ID) || !directiveNamePattern.MatchString(directive.Name) ||
		!directiveVersionPattern.MatchString(directive.Version) || !typeIDPattern.MatchString(directive.Capability) {
		return fmt.Errorf("invalid directive descriptor %q", directive.ID)
	}
	if knownScalar(ScalarKind(directive.ID)) || knownScalar(ScalarKind(directive.Name)) {
		return fmt.Errorf("directive %q collides with a built-in scalar", directive.ID)
	}
	if err := reserveActiveIdentity(directive.ID, directive.Name, activeIDs, activeNames); err != nil {
		return err
	}
	if err := validateDirectiveContract(directive); err != nil {
		return fmt.Errorf("directive %q: %w", directive.ID, err)
	}
	if err := validateElementMetadata(fmt.Sprintf("directive %q", directive.ID), directive.Deprecation, directive.Traits, directive.Source, options); err != nil {
		return err
	}
	return nil
}

func validateDirectiveContract(directive DirectiveDescriptor) error {
	if len(directive.Locations) == 0 || len(directive.Phases) == 0 || directive.Effect == "" {
		return errors.New("locations, phases, and effect are required")
	}
	if directive.Effect != "read" && directive.Effect != "write" {
		return fmt.Errorf("invalid effect %q", directive.Effect)
	}
	if directive.Cost > MaxDirectiveCost {
		return errors.New("directive cost exceeds the portable maximum")
	}
	if directive.Compatibility != ChangeBreaking && directive.Compatibility != ChangeDangerous &&
		directive.Compatibility != ChangeAdditive && directive.Compatibility != ChangeBehaviorOnly {
		return fmt.Errorf("invalid compatibility %q", directive.Compatibility)
	}
	if err := validateDirectiveLocations(directive.Locations); err != nil {
		return err
	}
	if err := validateDirectivePhases(directive.Phases); err != nil {
		return err
	}
	if slices.Contains(directive.Phases, DirectivePlanning) && !directive.Deterministic {
		return errors.New("planning phase requires deterministic behavior")
	}
	if directive.Effect == "write" && !slices.Contains(directive.Phases, DirectiveExecution) {
		return errors.New("write effect requires the execution phase")
	}
	return validateDirectiveArguments(directive.Arguments)
}

func validateDirectiveLocations(locations []protocol.SelectionKind) error {
	seen := make(map[protocol.SelectionKind]bool, len(locations))
	for _, location := range locations {
		if seen[location] || !slices.Contains([]protocol.SelectionKind{
			protocol.FieldSelection, protocol.CallSelection, protocol.PipelineSelection, protocol.MapSelection,
			protocol.IndexSelection, protocol.SliceSelection, protocol.PageSelection, protocol.MetaSelection,
			protocol.ParallelSelection, protocol.FragmentSelection, protocol.CurrentSelection,
			protocol.NestSelection, protocol.UnnestSelection,
		}, location) {
			return fmt.Errorf("invalid or duplicate location %q", location)
		}
		seen[location] = true
	}
	return nil
}

func validateDirectivePhases(phases []DirectivePhase) error {
	seen := make(map[DirectivePhase]bool, len(phases))
	for _, phase := range phases {
		if seen[phase] || (phase != DirectiveValidation && phase != DirectivePlanning && phase != DirectiveExecution && phase != DirectiveResponse) {
			return fmt.Errorf("invalid or duplicate phase %q", phase)
		}
		seen[phase] = true
	}
	if !seen[DirectiveValidation] {
		return errors.New("validation phase is required")
	}
	return nil
}

func validateDirectiveArguments(arguments []DirectiveArgumentDescriptor) error {
	ids := make(map[string]bool, len(arguments))
	names := make(map[string]bool, len(arguments))
	for _, argument := range arguments {
		if !typeIDPattern.MatchString(argument.ID) || !directiveNamePattern.MatchString(argument.Name) || argument.Type == "" ||
			ids[argument.ID] || names[argument.Name] || (argument.Required && len(argument.Default) != 0) {
			return fmt.Errorf("invalid or duplicate argument %q", argument.ID)
		}
		ids[argument.ID] = true
		names[argument.Name] = true
	}
	return nil
}

func validateRetiredIdentities(retiredIdentities []RetiredIdentity, activeIDs, activeNames map[string]bool) error {
	retiredIDs := make(map[string]bool, len(retiredIdentities))
	retiredNames := make(map[string]bool, len(retiredIdentities))
	for _, retired := range retiredIdentities {
		if retired.ID == "" || retired.Reason == "" {
			return errors.New("retired schema identity requires an id and reason")
		}
		if retiredIDs[retired.ID] || (retired.Name != "" && retiredNames[retired.Name]) {
			return fmt.Errorf("duplicate retired schema identity %q", retired.ID)
		}
		if activeIDs[retired.ID] || (retired.Name != "" && activeNames[retired.Name]) {
			return fmt.Errorf("schema reuses retired identity %q", retired.ID)
		}
		retiredIDs[retired.ID] = true
		if retired.Name != "" {
			retiredNames[retired.Name] = true
		}
	}
	return nil
}

func validatePinnedReferences(references []SchemaReference) error {
	for _, reference := range references {
		if reference.URI == "" || reference.Revision == "" || !schemaDigestPattern.MatchString(reference.Digest) {
			return fmt.Errorf("schema reference %q is not pinned", reference.URI)
		}
	}
	return nil
}

func validateTypeDeclaration(declaration TypeDeclaration, activeIDs, activeNames map[string]bool, options ImportOptions) error {
	if !typeIDPattern.MatchString(string(declaration.ID)) || !typeIDPattern.MatchString(declaration.Name) {
		return fmt.Errorf("invalid schema type identity %q/%q", declaration.ID, declaration.Name)
	}
	if err := reserveActiveIdentity(string(declaration.ID), declaration.Name, activeIDs, activeNames); err != nil {
		return err
	}
	memberNames := make(map[string]bool)
	if err := validateFieldDeclarations(declaration, activeIDs, memberNames, options); err != nil {
		return err
	}
	if err := validateEnumDeclarations(declaration, activeIDs, memberNames, options); err != nil {
		return err
	}
	if err := validateVariantDeclarations(declaration, activeIDs, memberNames, options); err != nil {
		return err
	}
	if err := validateElementMetadata(fmt.Sprintf("type %q", declaration.ID), declaration.Deprecation, declaration.Traits, declaration.Source, options); err != nil {
		return err
	}
	return validateNestedRetiredIdentities(declaration, activeIDs, memberNames)
}

func validateFieldDeclarations(declaration TypeDeclaration, activeIDs, memberNames map[string]bool, options ImportOptions) error {
	for _, field := range declaration.Fields {
		if field.ID == "" || !typeIDPattern.MatchString(field.Name) || field.Type == "" {
			return fmt.Errorf("type %q has invalid field %q", declaration.ID, field.ID)
		}
		if err := reserveActiveIdentity(field.ID, field.Name, activeIDs, memberNames); err != nil {
			return err
		}
		if err := validateElementMetadata(fmt.Sprintf("field %q", field.ID), field.Deprecation, field.Traits, field.Source, options); err != nil {
			return err
		}
	}
	return nil
}

func validateEnumDeclarations(declaration TypeDeclaration, activeIDs, memberNames map[string]bool, options ImportOptions) error {
	for _, member := range declaration.EnumMembers {
		if member.ID == "" || !utf8.ValidString(member.Name) {
			return fmt.Errorf("enum %q has invalid member %q", declaration.ID, member.ID)
		}
		if err := reserveActiveIdentity(member.ID, member.Name, activeIDs, memberNames); err != nil {
			return err
		}
		if err := validateElementMetadata(fmt.Sprintf("enum member %q", member.ID), member.Deprecation, member.Traits, member.Source, options); err != nil {
			return err
		}
	}
	return nil
}

func validateVariantDeclarations(declaration TypeDeclaration, activeIDs, memberNames map[string]bool, options ImportOptions) error {
	for _, member := range declaration.VariantMembers {
		if member.ID == "" || member.Type == "" {
			return fmt.Errorf("union %q has invalid variant %q", declaration.ID, member.ID)
		}
		if err := reserveActiveIdentity(member.ID, string(member.Type), activeIDs, memberNames); err != nil {
			return err
		}
		if err := validateElementMetadata(fmt.Sprintf("variant member %q", member.ID), member.Deprecation, member.Traits, member.Source, options); err != nil {
			return err
		}
	}
	return nil
}

func validateElementMetadata(label string, deprecation *Deprecation, traits []TraitDescriptor, source *SourceMetadata, options ImportOptions) error {
	if err := validateDeprecation(deprecation); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if err := validateTraits(traits, options); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if err := validateSource(source); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

func validateNestedRetiredIdentities(declaration TypeDeclaration, activeIDs, memberNames map[string]bool) error {
	retiredIDs := make(map[string]bool, len(declaration.Retired))
	retiredNames := make(map[string]bool, len(declaration.Retired))
	for _, retired := range declaration.Retired {
		if retired.ID == "" || retired.Reason == "" || activeIDs[retired.ID] || retiredIDs[retired.ID] ||
			(retired.Name != "" && (memberNames[retired.Name] || retiredNames[retired.Name])) {
			return fmt.Errorf("type %q has invalid or reused retired identity %q", declaration.ID, retired.ID)
		}
		retiredIDs[retired.ID] = true
		if retired.Name != "" {
			retiredNames[retired.Name] = true
		}
	}
	return nil
}

func validateOperationDescriptor(operation OperationDescriptor, activeIDs, activeNames map[string]bool, options ImportOptions) error {
	if operation.ID == "" || !typeIDPattern.MatchString(operation.Name) || operation.Output == "" || operation.Effect == "" ||
		(operation.Kind != protocol.Query && operation.Kind != protocol.Mutation && operation.Kind != protocol.Subscription) {
		return fmt.Errorf("invalid operation descriptor %q", operation.ID)
	}
	if err := reserveActiveIdentity(operation.ID, operation.Name, activeIDs, activeNames); err != nil {
		return err
	}
	if err := validateDeprecation(operation.Deprecation); err != nil {
		return fmt.Errorf("operation %q: %w", operation.ID, err)
	}
	if err := validateCollectionDescriptor("operation "+operation.ID, operation.Collection); err != nil {
		return err
	}
	if err := validateTraits(operation.Traits, options); err != nil {
		return err
	}
	return validateSource(operation.Source)
}

func validateMemberDescriptor(member MemberDescriptor, activeIDs, activeNames map[string]bool, options ImportOptions) error {
	if member.ID == "" || !typeIDPattern.MatchString(member.Name) || member.Owner == "" || member.Output == "" || member.Effect == "" ||
		(member.Kind != "field" && member.Kind != "call") {
		return fmt.Errorf("invalid member descriptor %q", member.ID)
	}
	if member.Kind == "field" && (member.Input != "" || member.InputNullable) {
		return fmt.Errorf("field member %q cannot declare input", member.ID)
	}
	if member.Kind == "call" && member.Input == "" {
		return fmt.Errorf("call member %q requires input", member.ID)
	}
	if err := reserveActiveIdentity(member.ID, member.Name, activeIDs, nil); err != nil {
		return err
	}
	if err := validateDeprecation(member.Deprecation); err != nil {
		return fmt.Errorf("member %q: %w", member.ID, err)
	}
	if err := validateCollectionDescriptor("member "+member.ID, member.Collection); err != nil {
		return err
	}
	if err := validateTraits(member.Traits, options); err != nil {
		return err
	}
	return validateSource(member.Source)
}

func validateCollectionDescriptor(owner string, collection *CollectionDescriptor) error {
	if collection == nil {
		return nil
	}
	if collection.MaxPageSize == 0 {
		return fmt.Errorf("%s collection requires a positive maximum page size", owner)
	}
	if collection.TotalCountCost > MaxDirectiveCost {
		return fmt.Errorf("%s collection total count cost exceeds the portable maximum", owner)
	}
	return nil
}

func reserveActiveIdentity(id, name string, activeIDs, activeNames map[string]bool) error {
	if activeIDs[id] {
		return fmt.Errorf("duplicate schema identity %q", id)
	}
	activeIDs[id] = true
	if activeNames != nil {
		if activeNames[name] {
			return fmt.Errorf("duplicate schema name %q", name)
		}
		activeNames[name] = true
	}
	return nil
}

func validateTraits(traits []TraitDescriptor, options ImportOptions) error {
	seen := make(map[string]bool, len(traits))
	for _, trait := range traits {
		if !typeIDPattern.MatchString(trait.ID) || seen[trait.ID] || !json.Valid(trait.Value) {
			return fmt.Errorf("invalid schema trait %q", trait.ID)
		}
		seen[trait.ID] = true
		switch trait.Semantics {
		case TraitDocumentation:
		case TraitValidation, TraitExecution, TraitAuthorization, TraitIdentity:
			if !options.SupportedTraits[trait.ID] {
				return fmt.Errorf("unsupported critical trait %q", trait.ID)
			}
		default:
			return fmt.Errorf("unknown schema trait semantics %q", trait.Semantics)
		}
	}
	return nil
}

func validateDeprecation(deprecation *Deprecation) error {
	if deprecation == nil {
		return nil
	}
	if deprecation.Reason == "" {
		return errors.New("deprecation requires a reason")
	}
	if deprecation.Sunset != "" {
		if _, err := time.Parse(time.RFC3339, deprecation.Sunset); err != nil {
			return fmt.Errorf("invalid deprecation sunset: %w", err)
		}
	}
	return nil
}

func validateSource(source *SourceMetadata) error {
	if source == nil {
		return nil
	}
	if source.URI == "" || source.Line < 0 || source.Column < 0 {
		return errors.New("source metadata requires a URI and non-negative location")
	}
	return nil
}

func validateDocumentReferences(wire documentWire) error {
	types := schemaTypeIndex(wire.Types)
	for _, operation := range wire.Operations {
		if err := validateCallableTypeReferences("operation "+operation.ID, operation.Input, operation.Output, types); err != nil {
			return err
		}
		if err := validateCollectionOutput("operation "+operation.ID, operation.Output, operation.Collection, types); err != nil {
			return err
		}
	}
	for _, member := range wire.Members {
		if _, exists := types[member.Owner]; !exists {
			return fmt.Errorf("member %q references unknown owner %q", member.ID, member.Owner)
		}
		if err := validateCallableTypeReferences("member "+member.ID, member.Input, member.Output, types); err != nil {
			return err
		}
		if err := validateCollectionOutput("member "+member.ID, member.Output, member.Collection, types); err != nil {
			return err
		}
	}
	return validateDirectiveTypeReferences(wire.Directives, types)
}

type schemaTypeReference struct {
	input      bool
	collection bool
}

func validateDirectiveTypeReferences(directives []DirectiveDescriptor, types map[TypeID]schemaTypeReference) error {
	for _, directive := range directives {
		for _, argument := range directive.Arguments {
			reference, exists := types[argument.Type]
			if !exists {
				return fmt.Errorf("directive %q argument %q references unknown type %q", directive.ID, argument.ID, argument.Type)
			}
			if !reference.input {
				return fmt.Errorf("directive %q argument %q references non-input type %q", directive.ID, argument.ID, argument.Type)
			}
		}
	}
	return nil
}

func schemaTypeIndex(declarations []TypeDeclaration) map[TypeID]schemaTypeReference {
	types := make(map[TypeID]schemaTypeReference, len(declarations)+13)
	for _, scalar := range []ScalarKind{Boolean, String, ID, Int32, Float64, Int64, UInt64, BigInt, Decimal, Timestamp, Duration, UUID, Bytes} {
		types[TypeID(scalar)] = schemaTypeReference{input: true}
	}
	for _, declaration := range declarations {
		types[declaration.ID] = schemaTypeReference{
			input: declaration.Input, collection: declaration.Kind == ListType || declaration.Kind == MapType,
		}
	}
	return types
}

func validateCollectionOutput(owner string, output TypeID, collection *CollectionDescriptor, types map[TypeID]schemaTypeReference) error {
	if collection != nil && !types[output].collection {
		return fmt.Errorf("%s collection metadata requires a collection output type", owner)
	}
	return nil
}

func validateCallableTypeReferences(owner string, input, output TypeID, types map[TypeID]schemaTypeReference) error {
	for _, reference := range []TypeID{input, output} {
		if _, exists := types[reference]; reference != "" && !exists {
			return fmt.Errorf("%s references unknown type %q", owner, reference)
		}
	}
	return nil
}

func declarationFromDescriptor(descriptor TypeDescriptor) TypeDeclaration {
	declaration := TypeDeclaration{
		ID: descriptor.ID, Name: descriptor.Name, Kind: descriptor.Kind, Input: descriptor.Input, Output: descriptor.Output,
		Open: descriptor.Open, Element: descriptor.Element, ElementNullable: descriptor.ElementNullable,
		MaxDepth: descriptor.MaxDepth, Scalar: cloneScalarDescriptorPointer(descriptor.Scalar), Description: descriptor.Description,
		Deprecation: cloneDeprecation(descriptor.Deprecation), Entity: cloneEntity(descriptor.Entity),
		Capabilities: slices.Clone(descriptor.Capabilities), Traits: cloneTraits(descriptor.Traits), Source: cloneSource(descriptor.Source),
		Retired: slices.Clone(descriptor.Retired),
	}
	for _, name := range sortedKeys(descriptor.Fields) {
		field := descriptor.Fields[name]
		declaration.Fields = append(declaration.Fields, FieldDeclaration{
			ID: field.ID, Name: name, Type: field.Type, Required: field.Required, Nullable: field.Nullable,
			Default: append(json.RawMessage(nil), field.Default...), Description: field.Description,
			Deprecation: cloneDeprecation(field.Deprecation), Cost: field.Cost, Traits: cloneTraits(field.Traits), Source: cloneSource(field.Source),
		})
	}
	declaration.EnumMembers = cloneEnumMembers(descriptor.EnumMembers)
	if len(declaration.EnumMembers) == 0 {
		for _, name := range descriptor.EnumValues {
			declaration.EnumMembers = append(declaration.EnumMembers, EnumMemberDescriptor{ID: enumMemberID(descriptor.ID, name), Name: name})
		}
	}
	declaration.VariantMembers = cloneVariantMembers(descriptor.VariantMembers)
	if len(declaration.VariantMembers) == 0 {
		for _, variant := range descriptor.Variants {
			declaration.VariantMembers = append(declaration.VariantMembers, VariantMemberDescriptor{ID: string(descriptor.ID) + "." + string(variant), Type: variant})
		}
	}
	return declaration
}

func enumMemberID(typeID TypeID, spelling string) string {
	if typeIDPattern.MatchString(spelling) {
		return string(typeID) + "." + spelling
	}
	digest := sha256.Sum256([]byte(string(typeID) + "\x00" + spelling))
	return fmt.Sprintf("enum.%x", digest)
}

func descriptorFromDeclaration(declaration TypeDeclaration) TypeDescriptor {
	descriptor := TypeDescriptor{
		ID: declaration.ID, Name: declaration.Name, Kind: declaration.Kind, Input: declaration.Input, Output: declaration.Output,
		Open: declaration.Open, Element: declaration.Element, ElementNullable: declaration.ElementNullable,
		Fields: make(map[string]FieldDescriptor, len(declaration.Fields)), MaxDepth: declaration.MaxDepth,
		Scalar: cloneScalarDescriptorPointer(declaration.Scalar), Description: declaration.Description,
		Deprecation: cloneDeprecation(declaration.Deprecation), Entity: cloneEntity(declaration.Entity),
		Capabilities: slices.Clone(declaration.Capabilities), Traits: cloneTraits(declaration.Traits), Source: cloneSource(declaration.Source),
		Retired: slices.Clone(declaration.Retired), EnumMembers: cloneEnumMembers(declaration.EnumMembers),
		VariantMembers: cloneVariantMembers(declaration.VariantMembers),
	}
	for _, field := range declaration.Fields {
		descriptor.Fields[field.Name] = FieldDescriptor{
			ID: field.ID, Type: field.Type, Required: field.Required, Nullable: field.Nullable,
			Default: append(json.RawMessage(nil), field.Default...), Description: field.Description,
			Deprecation: cloneDeprecation(field.Deprecation), Cost: field.Cost, Traits: cloneTraits(field.Traits), Source: cloneSource(field.Source),
		}
	}
	for _, member := range declaration.EnumMembers {
		descriptor.EnumValues = append(descriptor.EnumValues, member.Name)
	}
	for _, member := range declaration.VariantMembers {
		descriptor.Variants = append(descriptor.Variants, member.Type)
	}
	return descriptor
}

func cloneDocumentWire(input documentWire) documentWire {
	return documentWire{
		Version: input.Version, CanonicalVersion: input.CanonicalVersion, Revision: input.Revision,
		Capabilities: slices.Clone(input.Capabilities), Types: cloneTypeDeclarations(input.Types),
		Operations: cloneOperations(input.Operations), Members: cloneMembers(input.Members), Retired: slices.Clone(input.Retired),
		Directives: cloneDirectives(input.Directives), References: slices.Clone(input.References), Traits: cloneTraits(input.Traits),
	}
}

func cloneTypeDeclarations(input []TypeDeclaration) []TypeDeclaration {
	result := make([]TypeDeclaration, len(input))
	for remaining := len(input); remaining > 0; remaining-- {
		index := remaining - 1
		result[index] = declarationFromDescriptor(descriptorFromDeclaration(input[index]))
	}
	return result
}

func cloneOperations(input []OperationDescriptor) []OperationDescriptor {
	result := slices.Clone(input)
	for index := range result {
		result[index].Deprecation = cloneDeprecation(result[index].Deprecation)
		if result[index].Collection != nil {
			collection := *result[index].Collection
			result[index].Collection = &collection
		}
		result[index].Capabilities = slices.Clone(result[index].Capabilities)
		result[index].Traits = cloneTraits(result[index].Traits)
		result[index].Source = cloneSource(result[index].Source)
	}
	return result
}

func cloneMembers(input []MemberDescriptor) []MemberDescriptor {
	result := slices.Clone(input)
	for index := range result {
		result[index].Deprecation = cloneDeprecation(result[index].Deprecation)
		if result[index].Collection != nil {
			collection := *result[index].Collection
			result[index].Collection = &collection
		}
		result[index].Capabilities = slices.Clone(result[index].Capabilities)
		result[index].Traits = cloneTraits(result[index].Traits)
		result[index].Source = cloneSource(result[index].Source)
	}
	return result
}

func cloneDirectives(input []DirectiveDescriptor) []DirectiveDescriptor {
	result := slices.Clone(input)
	for index := range result {
		result[index].Locations = slices.Clone(result[index].Locations)
		result[index].Arguments = slices.Clone(result[index].Arguments)
		for argumentIndex := range result[index].Arguments {
			result[index].Arguments[argumentIndex].Default = bytes.Clone(result[index].Arguments[argumentIndex].Default)
		}
		result[index].Phases = slices.Clone(result[index].Phases)
		result[index].Deprecation = cloneDeprecation(result[index].Deprecation)
		result[index].Capabilities = slices.Clone(result[index].Capabilities)
		result[index].Traits = cloneTraits(result[index].Traits)
		result[index].Source = cloneSource(result[index].Source)
	}
	return result
}

func cloneDeprecation(input *Deprecation) *Deprecation {
	if input == nil {
		return nil
	}
	result := *input
	return &result
}

func cloneEntity(input *EntityDescriptor) *EntityDescriptor {
	if input == nil {
		return nil
	}
	return &EntityDescriptor{Keys: slices.Clone(input.Keys)}
}

func cloneSource(input *SourceMetadata) *SourceMetadata {
	if input == nil {
		return nil
	}
	result := *input
	return &result
}

func cloneTraits(input []TraitDescriptor) []TraitDescriptor {
	result := slices.Clone(input)
	for index := range result {
		result[index].Value = append(json.RawMessage(nil), result[index].Value...)
	}
	return result
}

func cloneEnumMembers(input []EnumMemberDescriptor) []EnumMemberDescriptor {
	return mapSlice(input, cloneEnumMember)
}

func cloneEnumMember(input EnumMemberDescriptor) EnumMemberDescriptor {
	input.Deprecation = cloneDeprecation(input.Deprecation)
	input.Traits = cloneTraits(input.Traits)
	input.Source = cloneSource(input.Source)
	return input
}

func cloneVariantMembers(input []VariantMemberDescriptor) []VariantMemberDescriptor {
	result := slices.Clone(input)
	for index := range result {
		result[index].Deprecation = cloneDeprecation(result[index].Deprecation)
		result[index].Traits = cloneTraits(result[index].Traits)
		result[index].Source = cloneSource(result[index].Source)
	}
	return result
}

func cloneScalarDescriptorPointer(input *ScalarDescriptor) *ScalarDescriptor {
	if input == nil {
		return nil
	}
	result := cloneScalarDescriptor(*input)
	return &result
}

func normalizedTraits(input []TraitDescriptor) []TraitDescriptor {
	if len(input) == 0 {
		return nil
	}
	result := cloneTraits(input)
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func sortedUnique(input []string) []string {
	if len(input) == 0 {
		return nil
	}
	result := slices.Clone(input)
	slices.Sort(result)
	return slices.Compact(result)
}

func collectTraitIDs(types []TypeDeclaration, operations []OperationDescriptor, members []MemberDescriptor, directives []DirectiveDescriptor, documentTraits []TraitDescriptor) map[string]bool {
	result := make(map[string]bool)
	add := func(traits []TraitDescriptor) {
		for _, trait := range traits {
			result[trait.ID] = true
		}
	}
	add(documentTraits)
	for _, declaration := range types {
		add(declaration.Traits)
		for _, field := range declaration.Fields {
			add(field.Traits)
		}
	}
	for _, operation := range operations {
		add(operation.Traits)
	}
	for _, member := range members {
		add(member.Traits)
	}
	for _, directive := range directives {
		add(directive.Traits)
	}
	return result
}

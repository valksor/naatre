package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"time"

	"github.com/valksor/naatre/protocol"
)

const (
	SchemaDocumentVersion  = "1"
	SchemaCanonicalVersion = "c14n-1"
)

var schemaDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

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
	Capabilities        []string               `json:"capabilities,omitempty"`
	Traits              []TraitDescriptor      `json:"traits,omitempty"`
	Source              *SourceMetadata        `json:"source,omitempty"`
}

type MemberDescriptor struct {
	ID                  string            `json:"id"`
	Name                string            `json:"name"`
	Owner               TypeID            `json:"owner"`
	Kind                string            `json:"kind"`
	Input               TypeID            `json:"input,omitempty"`
	InputNullable       bool              `json:"inputNullable,omitempty"`
	Output              TypeID            `json:"output"`
	OutputNullable      bool              `json:"outputNullable,omitempty"`
	Description         string            `json:"description,omitempty"`
	Deprecation         *Deprecation      `json:"deprecation,omitempty"`
	Effect              string            `json:"effect"`
	Deterministic       bool              `json:"deterministic,omitempty"`
	Cacheable           bool              `json:"cacheable,omitempty"`
	RetrySafe           bool              `json:"retrySafe,omitempty"`
	ThreadSafety        string            `json:"threadSafety,omitempty"`
	Batching            string            `json:"batching,omitempty"`
	Transaction         string            `json:"transaction,omitempty"`
	AuthorizationPolicy string            `json:"authorizationPolicy,omitempty"`
	Idempotency         string            `json:"idempotency,omitempty"`
	Cost                uint64            `json:"cost,omitempty"`
	ParallelMutation    bool              `json:"parallelMutation,omitempty"`
	Capabilities        []string          `json:"capabilities,omitempty"`
	Traits              []TraitDescriptor `json:"traits,omitempty"`
	Source              *SourceMetadata   `json:"source,omitempty"`
}

type ExportOptions struct {
	Revision     string
	Capabilities []string
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
	return buildDocument(documentWire{
		Version: SchemaDocumentVersion, CanonicalVersion: SchemaCanonicalVersion,
		Revision: options.Revision, Capabilities: slices.Clone(options.Capabilities), Types: types,
		Operations: cloneOperations(operations), Members: cloneMembers(members), Retired: slices.Clone(options.Retired),
		References: slices.Clone(options.References), Traits: cloneTraits(options.Traits),
	}, ImportOptions{SupportedTraits: collectTraitIDs(types, operations, members, options.Traits)})
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

// ExportOptions returns a detached copy of the document-level lifecycle and
// extension metadata needed to reproduce this document from a registry.
func (d Document) ExportOptions() ExportOptions {
	options := ExportOptions{Revision: d.wire.Revision}
	options.Capabilities = slices.Clone(d.wire.Capabilities)
	options.Retired = slices.Clone(d.wire.Retired)
	options.References = slices.Clone(d.wire.References)
	options.Traits = cloneTraits(d.wire.Traits)
	return options
}

func (d Document) CanonicalJSON() ([]byte, error) {
	if err := d.requireInitialized(); err != nil {
		return nil, err
	}
	return bytes.Clone(d.canonical), nil
}

func (d Document) requireInitialized() error {
	if len(d.canonical) != 0 {
		return nil
	}
	return errors.New("schema document is not initialized")
}

func (d Document) Hash() (protocol.Digest, error) {
	if len(d.canonical) == 0 {
		return protocol.Digest{}, errors.New("schema document is not initialized")
	}
	return protocol.SemanticHash(protocol.SchemaHash, d.canonical)
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
	sort.Slice(wire.Types, func(i, j int) bool { return wire.Types[i].ID < wire.Types[j].ID })
	sort.Slice(wire.Operations, func(i, j int) bool { return wire.Operations[i].ID < wire.Operations[j].ID })
	sort.Slice(wire.Members, func(i, j int) bool { return wire.Members[i].ID < wire.Members[j].ID })
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
	return activeIDs, activeNames, nil
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
		if member.ID == "" || !typeIDPattern.MatchString(member.Name) {
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
	if err := validateTraits(member.Traits, options); err != nil {
		return err
	}
	return validateSource(member.Source)
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
	}
	for _, member := range wire.Members {
		if !types[member.Owner] {
			return fmt.Errorf("member %q references unknown owner %q", member.ID, member.Owner)
		}
		if err := validateCallableTypeReferences("member "+member.ID, member.Input, member.Output, types); err != nil {
			return err
		}
	}
	return nil
}

func schemaTypeIndex(declarations []TypeDeclaration) map[TypeID]bool {
	types := make(map[TypeID]bool, len(declarations)+13)
	for _, scalar := range []ScalarKind{Boolean, String, ID, Int32, Float64, Int64, UInt64, BigInt, Decimal, Timestamp, Duration, UUID, Bytes} {
		types[TypeID(scalar)] = true
	}
	for _, declaration := range declarations {
		types[declaration.ID] = true
	}
	return types
}

func validateCallableTypeReferences(owner string, input, output TypeID, types map[TypeID]bool) error {
	for _, reference := range []TypeID{input, output} {
		if reference != "" && !types[reference] {
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
			declaration.EnumMembers = append(declaration.EnumMembers, EnumMemberDescriptor{ID: string(descriptor.ID) + "." + name, Name: name})
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
		References: slices.Clone(input.References), Traits: cloneTraits(input.Traits),
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
	result := slices.Clone(input)
	for index := range result {
		result[index].Deprecation = cloneDeprecation(result[index].Deprecation)
		result[index].Traits = cloneTraits(result[index].Traits)
		result[index].Source = cloneSource(result[index].Source)
	}
	return result
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

func collectTraitIDs(types []TypeDeclaration, operations []OperationDescriptor, members []MemberDescriptor, documentTraits []TraitDescriptor) map[string]bool {
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
	return result
}

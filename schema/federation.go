package schema

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/valksor/naatre/protocol"
)

const (
	CodeFederationInvalidManifest   = "FEDERATION_INVALID_MANIFEST"
	CodeFederationUntrustedService  = "FEDERATION_UNTRUSTED_SERVICE"
	CodeFederationProfileMismatch   = "FEDERATION_PROFILE_MISMATCH"
	CodeFederationSchemaMismatch    = "FEDERATION_SCHEMA_MISMATCH"
	CodeFederationOwnershipConflict = "FEDERATION_OWNERSHIP_CONFLICT"
	CodeFederationTypeConflict      = "FEDERATION_TYPE_CONFLICT"
	CodeFederationEntityCycle       = "FEDERATION_ENTITY_CYCLE"
)

// CompositionError is one deterministic, deployment-time federation failure.
// It contains stable identifiers but never endpoint credentials or schema data.
type CompositionError struct {
	Code       string
	ServiceID  string
	Definition string
	Message    string
}

func (e *CompositionError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Code + ": " + e.Message
}

// ServiceTrust is operator configuration for one authenticated downstream.
// EndpointReference is an opaque allowlist key, not a network address supplied
// by a request or schema document.
type ServiceTrust struct {
	Audience          string `json:"audience"`
	EndpointReference string `json:"endpointReference"`
	FederationProfile string `json:"federationProfile"`
	SchemaRevision    string `json:"schemaRevision"`
	SchemaDigest      string `json:"schemaDigest"`
}

// EntityFetchDependency identifies another composed entity lookup that must
// complete before an entity fetch can run.
type EntityFetchDependency struct {
	ServiceID string `json:"serviceId"`
	Type      TypeID `json:"type"`
}

// EntityFetch advertises a stable-key lookup owned by one service.
type EntityFetch struct {
	Type        TypeID                  `json:"type"`
	OperationID string                  `json:"operationId"`
	Keys        []string                `json:"keys"`
	Requires    []EntityFetchDependency `json:"requires,omitempty"`
}

// ServiceManifest is one immutable, pinned service contribution. Service
// ownership is separate from MemberDescriptor.Owner, which names a schema type.
type ServiceManifest struct {
	ID                string
	Audience          string
	EndpointReference string
	FederationProfile string
	Schema            Document
	SchemaRevision    string
	SchemaDigest      string
	OwnedOperations   []string
	OwnedMembers      []string
	EntityFetches     []EntityFetch
}

// FederationOptions binds composition to operator-controlled service trust and
// assigns the immutable revision of the resulting schema.
type FederationOptions struct {
	Revision string
	Services map[string]ServiceTrust
}

type federationOwner struct {
	ID      string `json:"id"`
	Service string `json:"service"`
}

// FederationService is one pinned, allowlisted downstream in a composition.
type FederationService struct {
	ID                string `json:"id"`
	Audience          string `json:"audience"`
	EndpointReference string `json:"endpointReference"`
	FederationProfile string `json:"federationProfile"`
	SchemaRevision    string `json:"schemaRevision"`
	SchemaDigest      string `json:"schemaDigest"`
}

// FederationEntityRoute is one validated cross-service entity lookup route.
type FederationEntityRoute struct {
	ServiceID   string                  `json:"serviceId"`
	Type        TypeID                  `json:"type"`
	OperationID string                  `json:"operationId"`
	Keys        []string                `json:"keys"`
	Requires    []EntityFetchDependency `json:"requires,omitempty"`
}

// FederationComposition is an immutable composed schema and deterministic
// routing authority. Callers receive detached copies of mutable data.
type FederationComposition struct {
	schema          Document
	canonical       []byte
	services        []FederationService
	operationOwners map[string]string
	memberOwners    map[string]string
	entityRoutes    map[string]FederationEntityRoute
}

func (c FederationComposition) Schema() Document { return c.schema }

func (c FederationComposition) CanonicalJSON() ([]byte, error) {
	return cloneCanonical(c.canonical, c.requireInitialized())
}

func (c FederationComposition) requireInitialized() error {
	if len(c.canonical) == 0 {
		return fmt.Errorf("federation composition is not initialized")
	}
	return nil
}

// Hash identifies the complete composition, including pinned downstreams and
// routing ownership. Schema().Hash identifies only the public composed schema.
func (c FederationComposition) Hash() (protocol.Digest, error) {
	return hashCanonical(c.canonical, protocol.FederationHash, c.requireInitialized())
}

func (c FederationComposition) Services() []string {
	return mapSlice(c.services, func(service FederationService) string { return service.ID })
}

func (c FederationComposition) Service(id string) (FederationService, bool) {
	index, found := slices.BinarySearchFunc(c.services, id, func(service FederationService, target string) int {
		return strings.Compare(service.ID, target)
	})
	if !found {
		return FederationService{}, false
	}
	return c.services[index], true
}

func (c FederationComposition) OperationOwner(id string) (string, bool) {
	owner, ok := c.operationOwners[id]
	return owner, ok
}

func (c FederationComposition) MemberOwner(id string) (string, bool) {
	owner, ok := c.memberOwners[id]
	return owner, ok
}

func (c FederationComposition) Operation(id string) (OperationDescriptor, bool) {
	for _, operation := range c.schema.Operations() {
		if operation.ID == id {
			return operation, true
		}
	}
	return OperationDescriptor{}, false
}

func (c FederationComposition) EntityRoutes() []FederationEntityRoute {
	return mapSlice(mapValues(c.entityRoutes), cloneFederationEntityRoute)
}

// ComposeFederation validates and canonically composes pinned service
// manifests. It performs no discovery and never dereferences endpoint values.
func ComposeFederation(manifests []ServiceManifest, options FederationOptions) (FederationComposition, error) {
	ordered := slices.Clone(manifests)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	if len(ordered) == 0 {
		return FederationComposition{}, compositionError(CodeFederationInvalidManifest, "", "", "at least one service manifest is required")
	}

	state := federationCompositionState{
		options: options, serviceIDs: make(map[string]bool, len(ordered)),
		types: make(map[TypeID]TypeDeclaration), operations: make(map[string]OperationDescriptor),
		members: make(map[string]MemberDescriptor), directives: make(map[string]DirectiveDescriptor),
		operationOwners: make(map[string]string), memberOwners: make(map[string]string),
		entityRoutes: make(map[string]FederationEntityRoute), retired: make(map[string]RetiredIdentity),
		traits: make(map[string]TraitDescriptor), references: make(map[string]SchemaReference),
	}
	for _, manifest := range ordered {
		if err := state.addManifest(manifest); err != nil {
			return FederationComposition{}, err
		}
	}
	if err := state.validateOwnership(); err != nil {
		return FederationComposition{}, err
	}
	if err := state.validateEntityRoutes(); err != nil {
		return FederationComposition{}, err
	}
	return state.build()
}

type federationCompositionState struct {
	options         FederationOptions
	serviceIDs      map[string]bool
	types           map[TypeID]TypeDeclaration
	operations      map[string]OperationDescriptor
	members         map[string]MemberDescriptor
	directives      map[string]DirectiveDescriptor
	operationOwners map[string]string
	memberOwners    map[string]string
	entityRoutes    map[string]FederationEntityRoute
	services        []FederationService
	capabilities    []string
	retired         map[string]RetiredIdentity
	traits          map[string]TraitDescriptor
	references      map[string]SchemaReference
}

func (s *federationCompositionState) addManifest(manifest ServiceManifest) error {
	if err := s.validateManifestTrust(manifest); err != nil {
		return err
	}
	s.addService(manifest)
	local, err := s.mergeManifestDeclarations(manifest)
	if err != nil {
		return err
	}
	if err := s.claimManifestOwnership(manifest, local); err != nil {
		return err
	}
	if err := s.addManifestEntityRoutes(manifest, local); err != nil {
		return err
	}
	return s.mergeManifestMetadata(manifest)
}

func (s *federationCompositionState) validateManifestTrust(manifest ServiceManifest) error {
	if !typeIDPattern.MatchString(manifest.ID) || s.serviceIDs[manifest.ID] {
		return compositionError(CodeFederationInvalidManifest, manifest.ID, "", "service identifiers must be non-empty and unique")
	}
	trusted, ok := s.options.Services[manifest.ID]
	if manifest.FederationProfile != "core.federation-1" || !ok || trusted.FederationProfile != "core.federation-1" ||
		manifest.FederationProfile != trusted.FederationProfile {
		return compositionError(CodeFederationProfileMismatch, manifest.ID, "", "service federation profile is unsupported or does not match operator configuration")
	}
	if trusted.Audience == "" || !typeIDPattern.MatchString(trusted.EndpointReference) ||
		manifest.Audience != trusted.Audience || manifest.EndpointReference != trusted.EndpointReference {
		return compositionError(CodeFederationUntrustedService, manifest.ID, "", "service is not bound to its configured audience and endpoint reference")
	}
	digest, err := manifest.Schema.Hash()
	if err != nil {
		return compositionError(CodeFederationInvalidManifest, manifest.ID, "", "service schema is not initialized")
	}
	wantDigest := "sha256:" + digest.Hex
	if manifest.SchemaRevision != manifest.Schema.Revision() || manifest.SchemaDigest != wantDigest {
		return compositionError(CodeFederationSchemaMismatch, manifest.ID, "", "service schema revision or digest does not match its pinned document")
	}
	if trusted.SchemaRevision != manifest.SchemaRevision || trusted.SchemaDigest != manifest.SchemaDigest {
		return compositionError(CodeFederationUntrustedService, manifest.ID, "", "service schema provenance does not match operator pins")
	}
	return nil
}

func (s *federationCompositionState) addService(manifest ServiceManifest) {
	s.serviceIDs[manifest.ID] = true
	s.services = append(s.services, FederationService{
		ID: manifest.ID, Audience: manifest.Audience, EndpointReference: manifest.EndpointReference,
		FederationProfile: manifest.FederationProfile,
		SchemaRevision:    manifest.SchemaRevision, SchemaDigest: manifest.SchemaDigest,
	})
}

type federationLocalDeclarations struct {
	types      map[TypeID]TypeDeclaration
	operations map[string]bool
	members    map[string]bool
}

func (s *federationCompositionState) mergeManifestDeclarations(manifest ServiceManifest) (federationLocalDeclarations, error) {
	types := manifest.Schema.Types()
	operations := manifest.Schema.Operations()
	members := manifest.Schema.Members()
	directives := manifest.Schema.Directives()
	local := federationLocalDeclarations{
		types:      make(map[TypeID]TypeDeclaration, len(types)),
		operations: make(map[string]bool, len(operations)),
		members:    make(map[string]bool, len(members)),
	}
	for _, declaration := range types {
		local.types[declaration.ID] = declaration
		if err := s.addType(manifest.ID, declaration); err != nil {
			return federationLocalDeclarations{}, err
		}
	}
	for _, operation := range operations {
		local.operations[operation.ID] = true
		if existing, exists := s.operations[operation.ID]; exists && !reflect.DeepEqual(existing, operation) {
			return federationLocalDeclarations{}, compositionError(CodeFederationTypeConflict, manifest.ID, operation.ID, "operation declarations are incompatible")
		}
		s.operations[operation.ID] = operation
	}
	for _, member := range members {
		local.members[member.ID] = true
		if existing, exists := s.members[member.ID]; exists && !reflect.DeepEqual(existing, member) {
			return federationLocalDeclarations{}, compositionError(CodeFederationTypeConflict, manifest.ID, member.ID, "member declarations are incompatible")
		}
		s.members[member.ID] = member
	}
	for _, directive := range directives {
		if existing, exists := s.directives[directive.ID]; exists && !reflect.DeepEqual(existing, directive) {
			return federationLocalDeclarations{}, compositionError(CodeFederationTypeConflict, manifest.ID, directive.ID, "directive declarations are incompatible")
		}
		s.directives[directive.ID] = directive
	}
	return local, nil
}

func (s *federationCompositionState) claimManifestOwnership(manifest ServiceManifest, local federationLocalDeclarations) error {
	claimedOperations := make(map[string]bool, len(manifest.OwnedOperations))
	for _, id := range manifest.OwnedOperations {
		if !local.operations[id] || claimedOperations[id] {
			return compositionError(CodeFederationInvalidManifest, manifest.ID, id, "owned operation must occur exactly once in the declaring service schema")
		}
		claimedOperations[id] = true
		if owner, exists := s.operationOwners[id]; exists && owner != manifest.ID {
			return compositionError(CodeFederationOwnershipConflict, manifest.ID, id, "operation is claimed by more than one service")
		}
		s.operationOwners[id] = manifest.ID
	}
	claimedMembers := make(map[string]bool, len(manifest.OwnedMembers))
	for _, id := range manifest.OwnedMembers {
		if !local.members[id] || claimedMembers[id] {
			return compositionError(CodeFederationInvalidManifest, manifest.ID, id, "owned member must occur exactly once in the declaring service schema")
		}
		claimedMembers[id] = true
		if owner, exists := s.memberOwners[id]; exists && owner != manifest.ID {
			return compositionError(CodeFederationOwnershipConflict, manifest.ID, id, "member is claimed by more than one service")
		}
		s.memberOwners[id] = manifest.ID
	}
	return nil
}

func (s *federationCompositionState) addManifestEntityRoutes(manifest ServiceManifest, local federationLocalDeclarations) error {
	for _, fetch := range manifest.EntityFetches {
		declaration, hasType := local.types[fetch.Type]
		if !hasType || declaration.Entity == nil || !slices.Equal(fetch.Keys, declaration.Entity.Keys) || !local.operations[fetch.OperationID] {
			return compositionError(CodeFederationInvalidManifest, manifest.ID, string(fetch.Type), "entity fetch type, keys, and operation must be declared by the owning service schema")
		}
		key := federationEntityKey(manifest.ID, fetch.Type)
		if _, exists := s.entityRoutes[key]; exists {
			return compositionError(CodeFederationOwnershipConflict, manifest.ID, string(fetch.Type), "entity fetch is declared more than once")
		}
		fetch.Keys = slices.Clone(fetch.Keys)
		fetch.Requires = slices.Clone(fetch.Requires)
		s.entityRoutes[key] = FederationEntityRoute{
			ServiceID: manifest.ID, Type: fetch.Type, OperationID: fetch.OperationID,
			Keys: fetch.Keys, Requires: fetch.Requires,
		}
	}
	return nil
}

func (s *federationCompositionState) mergeManifestMetadata(manifest ServiceManifest) error {
	metadata := manifest.Schema.ExportOptions()
	s.capabilities = append(s.capabilities, metadata.Capabilities...)
	if err := mergeFederationMetadata(s.retired, metadata.Retired, manifest.ID, "retired identity metadata is incompatible", func(value RetiredIdentity) string { return value.ID }); err != nil {
		return err
	}
	if err := mergeFederationMetadata(s.traits, metadata.Traits, manifest.ID, "schema trait metadata is incompatible", func(value TraitDescriptor) string { return value.ID }); err != nil {
		return err
	}
	if err := mergeFederationMetadata(s.references, metadata.References, manifest.ID, "schema reference metadata is incompatible", func(value SchemaReference) string { return value.URI }); err != nil {
		return err
	}
	return nil
}

func (s *federationCompositionState) addType(serviceID string, declaration TypeDeclaration) error {
	existing, exists := s.types[declaration.ID]
	if !exists {
		s.types[declaration.ID] = declaration
		return nil
	}
	merged, ok := mergeFederatedType(existing, declaration)
	if !ok {
		return compositionError(CodeFederationTypeConflict, serviceID, string(declaration.ID), "type declarations are incompatible")
	}
	s.types[declaration.ID] = merged
	return nil
}

func mergeFederatedType(left, right TypeDeclaration) (TypeDeclaration, bool) {
	leftBase, rightBase := left, right
	leftBase.Fields, rightBase.Fields = nil, nil
	leftBase.EnumMembers, rightBase.EnumMembers = nil, nil
	leftBase.VariantMembers, rightBase.VariantMembers = nil, nil
	if !reflect.DeepEqual(leftBase, rightBase) {
		return TypeDeclaration{}, false
	}
	var ok bool
	left.Fields, ok = mergeDescriptors(left.Fields, right.Fields, func(value FieldDeclaration) string { return value.ID })
	if !ok {
		return TypeDeclaration{}, false
	}
	left.EnumMembers, ok = mergeDescriptors(left.EnumMembers, right.EnumMembers, func(value EnumMemberDescriptor) string { return value.ID })
	if !ok {
		return TypeDeclaration{}, false
	}
	left.VariantMembers, ok = mergeDescriptors(left.VariantMembers, right.VariantMembers, func(value VariantMemberDescriptor) string { return value.ID })
	return left, ok
}

func mergeDescriptors[T any](left, right []T, id func(T) string) ([]T, bool) {
	byID := make(map[string]T, len(left)+len(right))
	for _, value := range append(slices.Clone(left), right...) {
		key := id(value)
		if existing, exists := byID[key]; exists && !reflect.DeepEqual(existing, value) {
			return nil, false
		}
		byID[key] = value
	}
	keys := make([]string, 0, len(byID))
	for key := range byID {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]T, 0, len(keys))
	for _, key := range keys {
		result = append(result, byID[key])
	}
	return result, true
}

func (s *federationCompositionState) validateOwnership() error {
	for _, id := range sortedKeys(s.operations) {
		if _, ok := s.operationOwners[id]; !ok {
			return compositionError(CodeFederationInvalidManifest, "", id, "operation has no owning service")
		}
	}
	for _, id := range sortedKeys(s.operationOwners) {
		if _, ok := s.operations[id]; !ok {
			return compositionError(CodeFederationInvalidManifest, s.operationOwners[id], id, "owned operation is absent from the service schema")
		}
	}
	for _, id := range sortedKeys(s.members) {
		if _, ok := s.memberOwners[id]; !ok {
			return compositionError(CodeFederationInvalidManifest, "", id, "member has no owning service")
		}
	}
	for _, id := range sortedKeys(s.memberOwners) {
		if _, ok := s.members[id]; !ok {
			return compositionError(CodeFederationInvalidManifest, s.memberOwners[id], id, "owned member is absent from the service schema")
		}
	}
	return nil
}

func (s *federationCompositionState) validateEntityRoutes() error {
	if err := s.validateEntityRouteDeclarations(); err != nil {
		return err
	}
	return s.validateEntityRouteCycles()
}

func (s *federationCompositionState) validateEntityRouteDeclarations() error {
	for _, key := range sortedKeys(s.entityRoutes) {
		route := s.entityRoutes[key]
		declaration, ok := s.types[route.Type]
		if !ok || declaration.Entity == nil || !slices.Equal(route.Keys, declaration.Entity.Keys) {
			return compositionError(CodeFederationInvalidManifest, route.ServiceID, string(route.Type), "entity fetch keys do not match the composed entity declaration")
		}
		if owner, ok := s.operationOwners[route.OperationID]; !ok || owner != route.ServiceID {
			return compositionError(CodeFederationInvalidManifest, route.ServiceID, route.OperationID, "entity fetch operation is not owned by the declaring service")
		}
		for _, dependency := range route.Requires {
			if _, ok := s.entityRoutes[federationEntityKey(dependency.ServiceID, dependency.Type)]; !ok {
				return compositionError(CodeFederationInvalidManifest, route.ServiceID, key, "entity fetch dependency is not declared")
			}
		}
	}
	return nil
}

func (s *federationCompositionState) validateEntityRouteCycles() error {
	state := make(map[string]uint8, len(s.entityRoutes))
	for _, key := range sortedKeys(s.entityRoutes) {
		if err := s.visitEntityRoute(key, state); err != nil {
			return err
		}
	}
	return nil
}

func (s *federationCompositionState) visitEntityRoute(key string, state map[string]uint8) error {
	switch state[key] {
	case 1:
		return compositionError(CodeFederationEntityCycle, s.entityRoutes[key].ServiceID, string(s.entityRoutes[key].Type), "entity fetch dependency cycle detected")
	case 2:
		return nil
	}
	state[key] = 1
	requires := slices.Clone(s.entityRoutes[key].Requires)
	sort.Slice(requires, func(i, j int) bool {
		return federationEntityKey(requires[i].ServiceID, requires[i].Type) < federationEntityKey(requires[j].ServiceID, requires[j].Type)
	})
	for _, dependency := range requires {
		if err := s.visitEntityRoute(federationEntityKey(dependency.ServiceID, dependency.Type), state); err != nil {
			return err
		}
	}
	state[key] = 2
	return nil
}

func (s *federationCompositionState) build() (FederationComposition, error) {
	references := maps.Clone(s.references)
	for _, service := range s.services {
		reference := SchemaReference{URI: "service:" + service.ID, Revision: service.SchemaRevision, Digest: service.SchemaDigest}
		if existing, exists := references[reference.URI]; exists && existing != reference {
			return FederationComposition{}, compositionError(CodeFederationTypeConflict, service.ID, reference.URI, "service schema reference conflicts with composed metadata")
		}
		references[reference.URI] = reference
	}
	wire := documentWire{
		Version: SchemaDocumentVersion, CanonicalVersion: SchemaCanonicalVersion, Revision: s.options.Revision,
		Types: mapValues(s.types), Operations: mapValues(s.operations), Members: mapValues(s.members), Directives: mapValues(s.directives),
		Capabilities: uniqueStrings(s.capabilities), Retired: mapValues(s.retired), References: mapValues(references), Traits: mapValues(s.traits),
	}
	document, err := buildDocument(wire, ImportOptions{SupportedTraits: collectTraitIDs(wire.Types, wire.Operations, wire.Members, wire.Directives, wire.Traits)})
	if err != nil {
		return FederationComposition{}, compositionError(CodeFederationTypeConflict, "", "", "composed schema is invalid: "+err.Error())
	}
	canonicalSchema, _ := document.CanonicalJSON()
	entityRoutes := mapValues(s.entityRoutes)
	for index := range entityRoutes {
		sort.Strings(entityRoutes[index].Keys)
		sort.Slice(entityRoutes[index].Requires, func(i, j int) bool {
			return federationEntityKey(entityRoutes[index].Requires[i].ServiceID, entityRoutes[index].Requires[i].Type) < federationEntityKey(entityRoutes[index].Requires[j].ServiceID, entityRoutes[index].Requires[j].Type)
		})
	}
	wireOutput := struct {
		Profile         string                  `json:"profile"`
		Revision        string                  `json:"revision"`
		Schema          json.RawMessage         `json:"schema"`
		Services        []FederationService     `json:"services"`
		OperationOwners []federationOwner       `json:"operationOwners"`
		MemberOwners    []federationOwner       `json:"memberOwners"`
		EntityRoutes    []FederationEntityRoute `json:"entityRoutes"`
	}{
		Profile: "core.federation-1", Revision: s.options.Revision, Schema: canonicalSchema,
		Services: slices.Clone(s.services), OperationOwners: ownerEntries(s.operationOwners),
		MemberOwners: ownerEntries(s.memberOwners), EntityRoutes: entityRoutes,
	}
	encoded, err := json.Marshal(wireOutput)
	if err != nil {
		return FederationComposition{}, err
	}
	canonical, err := protocol.CanonicalizeJSON(encoded, protocol.Limits{})
	if err != nil {
		return FederationComposition{}, err
	}
	return FederationComposition{
		schema: document, canonical: canonical, services: slices.Clone(s.services),
		operationOwners: maps.Clone(s.operationOwners), memberOwners: maps.Clone(s.memberOwners),
		entityRoutes: s.entityRoutes,
	}, nil
}

func compositionError(code, serviceID, definition, message string) *CompositionError {
	return &CompositionError{Code: code, ServiceID: serviceID, Definition: definition, Message: message}
}

func federationEntityKey(serviceID string, typeID TypeID) string {
	return serviceID + "\x00" + string(typeID)
}

func mapValues[K ~string, V any](values map[K]V) []V {
	return sortedValues(values)
}

func ownerEntries(owners map[string]string) []federationOwner {
	return mapSlice(sortedKeys(owners), func(id string) federationOwner {
		return federationOwner{ID: id, Service: owners[id]}
	})
}

func uniqueStrings(values []string) []string {
	result := slices.Clone(values)
	sort.Strings(result)
	return slices.Compact(result)
}

func cloneFederationEntityRoute(route FederationEntityRoute) FederationEntityRoute {
	route.Keys = slices.Clone(route.Keys)
	route.Requires = slices.Clone(route.Requires)
	return route
}

func mergeFederationMetadata[T any](target map[string]T, values []T, serviceID, message string, identifier func(T) string) error {
	for _, value := range values {
		id := identifier(value)
		if existing, ok := target[id]; ok && !reflect.DeepEqual(existing, value) {
			return compositionError(CodeFederationTypeConflict, serviceID, id, message)
		}
		target[id] = value
	}
	return nil
}

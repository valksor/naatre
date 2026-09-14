package schema

// Visibility is an explicit allow-list of stable schema identities. A nil or
// missing map entry is hidden. Retired identities are separate because they
// are not active declarations.
type Visibility struct {
	Types            map[TypeID]bool `json:"types,omitempty"`
	Fields           map[string]bool `json:"fields,omitempty"`
	EnumValues       map[string]bool `json:"enumValues,omitempty"`
	Variants         map[string]bool `json:"variants,omitempty"`
	Operations       map[string]bool `json:"operations,omitempty"`
	Members          map[string]bool `json:"members,omitempty"`
	Directives       map[string]bool `json:"directives,omitempty"`
	Extensions       map[string]bool `json:"extensions,omitempty"`
	CollectionFields map[string]bool `json:"collectionFields,omitempty"`
	Retired          map[string]bool `json:"retired,omitempty"`
}

// Filter returns an immutable, self-contained discovery document containing
// only declarations explicitly allowed by visibility. Output declarations may
// be pruned. Input objects are retained only when every input field is visible,
// so filtering cannot silently change the accepted input contract.
func (d Document) Filter(visibility Visibility) (Document, error) {
	wire := cloneDocumentWire(d.wire)
	wire.Types = filterTypes(wire.Types, visibility)
	wire.Retired = filterRetired(wire.Retired, visibility.Retired)

	for changed := true; changed; {
		changed = pruneFilteredTypes(&wire.Types)
	}

	available, fields := filteredTypeIndex(wire.Types)
	wire.Operations = filterOperations(wire.Operations, visibility.Operations, visibility.CollectionFields, available)
	wire.Members = filterMembers(wire.Members, visibility.Members, visibility.CollectionFields, available, fields)
	wire.Directives = filterDirectives(wire.Directives, visibility.Directives, available)
	wire.Extensions = filterExtensions(wire.Extensions, visibility.Extensions, wire.Directives)

	return buildDocument(wire, ImportOptions{
		SupportedTraits: collectTraitIDs(wire.Types, wire.Operations, wire.Members, wire.Directives, wire.Traits),
	})
}

func filterExtensions(input []ExtensionDescriptor, visibility map[string]bool, directives []DirectiveDescriptor) []ExtensionDescriptor {
	visibleDirectiveNames := make(map[string]bool, len(directives))
	for _, directive := range directives {
		visibleDirectiveNames[directive.Name] = true
	}
	result := make([]ExtensionDescriptor, 0, len(input))
	for _, extension := range input {
		if !visibility[extension.ID] {
			continue
		}
		extension.Directives = filterVisible(extension.Directives, visibleDirectiveNames, func(value string) string { return value })
		extension.Before = filterVisible(extension.Before, visibility, func(value string) string { return value })
		extension.After = filterVisible(extension.After, visibility, func(value string) string { return value })
		extension.Conflicts = filterVisible(extension.Conflicts, visibility, func(value string) string { return value })
		result = append(result, extension)
	}
	return result
}

func filterDirectives(input []DirectiveDescriptor, visibility map[string]bool, available map[TypeID]bool) []DirectiveDescriptor {
	result := make([]DirectiveDescriptor, 0, len(input))
	for _, directive := range input {
		if !visibility[directive.ID] {
			continue
		}
		valid := true
		for _, argument := range directive.Arguments {
			if !typeReferenceAvailable(argument.Type, available) {
				valid = false
				break
			}
		}
		if valid {
			result = append(result, directive)
		}
	}
	return result
}

func filterTypes(input []TypeDeclaration, visibility Visibility) []TypeDeclaration {
	result := make([]TypeDeclaration, 0, len(input))
	for _, declaration := range input {
		if !visibility.Types[declaration.ID] {
			continue
		}
		if (declaration.Kind == InputObjectType || declaration.Kind == OneOfType) && !allFieldsVisible(declaration.Fields, visibility.Fields) {
			continue
		}
		declaration.Fields = filterFields(declaration.Fields, visibility.Fields)
		declaration.EnumMembers = filterEnumMembers(declaration.EnumMembers, visibility.EnumValues)
		declaration.VariantMembers = filterVariantMembers(declaration.VariantMembers, visibility.Variants)
		declaration.Retired = filterRetired(declaration.Retired, visibility.Retired)
		filterEntityKeys(&declaration)
		result = append(result, declaration)
	}
	return result
}

func allFieldsVisible(fields []FieldDeclaration, visibility map[string]bool) bool {
	for _, field := range fields {
		if !visibility[field.ID] {
			return false
		}
	}
	return true
}

func filterFields(input []FieldDeclaration, visibility map[string]bool) []FieldDeclaration {
	return filterVisible(input, visibility, func(field FieldDeclaration) string { return field.ID })
}

func filterEnumMembers(input []EnumMemberDescriptor, visibility map[string]bool) []EnumMemberDescriptor {
	return filterVisible(input, visibility, func(member EnumMemberDescriptor) string { return member.ID })
}

func filterVariantMembers(input []VariantMemberDescriptor, visibility map[string]bool) []VariantMemberDescriptor {
	return filterVisible(input, visibility, func(member VariantMemberDescriptor) string { return member.ID })
}

func filterRetired(input []RetiredIdentity, visibility map[string]bool) []RetiredIdentity {
	return filterVisible(input, visibility, func(retired RetiredIdentity) string { return retired.ID })
}

func filterVisible[T any](input []T, visibility map[string]bool, identifier func(T) string) []T {
	result := make([]T, 0, len(input))
	for _, value := range input {
		if visibility[identifier(value)] {
			result = append(result, value)
		}
	}
	return result
}

func filterEntityKeys(declaration *TypeDeclaration) {
	if declaration.Entity == nil {
		return
	}
	fields := make(map[string]bool, len(declaration.Fields))
	for _, field := range declaration.Fields {
		fields[field.Name] = true
	}
	keys := declaration.Entity.Keys[:0]
	for _, key := range declaration.Entity.Keys {
		if fields[key] {
			keys = append(keys, key)
		}
	}
	declaration.Entity.Keys = keys
	if len(keys) == 0 {
		declaration.Entity = nil
	}
}

func pruneFilteredTypes(types *[]TypeDeclaration) bool {
	available, _ := filteredTypeIndex(*types)
	changed := false
	result := (*types)[:0]
	for _, declaration := range *types {
		declaration, keep, pruned := pruneFilteredType(declaration, available)
		changed = changed || pruned
		if keep {
			result = append(result, declaration)
		} else {
			changed = true
		}
	}
	*types = result
	return changed
}

func pruneFilteredType(declaration TypeDeclaration, available map[TypeID]bool) (TypeDeclaration, bool, bool) {
	switch declaration.Kind {
	case ScalarType:
		return declaration, true, false
	case ListType, MapType:
		return declaration, typeReferenceAvailable(declaration.Element, available), false
	case InputObjectType, OneOfType:
		return declaration, allFieldTypesAvailable(declaration.Fields, available), false
	case ObjectType, InterfaceType:
		return pruneOutputType(declaration, available)
	case UnionType:
		var pruned bool
		declaration.VariantMembers, pruned = retainAvailable(declaration.VariantMembers, available, func(member VariantMemberDescriptor) TypeID { return member.Type })
		return declaration, len(declaration.VariantMembers) != 0, pruned
	case EnumType:
		return declaration, len(declaration.EnumMembers) != 0, false
	default:
		return declaration, false, false
	}
}

func allFieldTypesAvailable(fields []FieldDeclaration, available map[TypeID]bool) bool {
	for _, field := range fields {
		if !typeReferenceAvailable(field.Type, available) {
			return false
		}
	}
	return true
}

func pruneOutputType(declaration TypeDeclaration, available map[TypeID]bool) (TypeDeclaration, bool, bool) {
	var fieldsPruned bool
	declaration.Fields, fieldsPruned = retainAvailable(declaration.Fields, available, func(field FieldDeclaration) TypeID { return field.Type })
	filterEntityKeys(&declaration)
	keep := len(declaration.Fields) != 0
	variantsPruned := false
	if declaration.Kind == InterfaceType {
		declaration.VariantMembers, variantsPruned = retainAvailable(declaration.VariantMembers, available, func(member VariantMemberDescriptor) TypeID { return member.Type })
		keep = keep && len(declaration.VariantMembers) != 0
	}
	return declaration, keep, fieldsPruned || variantsPruned
}

func retainAvailable[T any](input []T, available map[TypeID]bool, reference func(T) TypeID) ([]T, bool) {
	result := input[:0]
	for _, value := range input {
		if typeReferenceAvailable(reference(value), available) {
			result = append(result, value)
		}
	}
	return result, len(result) != len(input)
}

func filteredTypeIndex(types []TypeDeclaration) (map[TypeID]bool, map[TypeID]map[string]bool) {
	available := make(map[TypeID]bool, len(types))
	fields := make(map[TypeID]map[string]bool)
	for _, declaration := range types {
		available[declaration.ID] = true
		owned := make(map[string]bool, len(declaration.Fields))
		for _, field := range declaration.Fields {
			owned[field.Name] = true
		}
		fields[declaration.ID] = owned
	}
	return available, fields
}

func typeReferenceAvailable(id TypeID, available map[TypeID]bool) bool {
	return id == "" || available[id] || knownScalar(ScalarKind(id))
}

func filterOperations(input []OperationDescriptor, visibility, collectionVisibility map[string]bool, available map[TypeID]bool) []OperationDescriptor {
	result := make([]OperationDescriptor, 0, len(input))
	for _, operation := range input {
		if visibility[operation.ID] && typeReferenceAvailable(operation.Input, available) && typeReferenceAvailable(operation.Output, available) {
			if filterCollectionQuery(operation.Collection, collectionVisibility, available) {
				operation.Capabilities = removeString(operation.Capabilities, CollectionQueryCapability)
			}
			result = append(result, operation)
		}
	}
	return result
}

func filterMembers(input []MemberDescriptor, visibility, collectionVisibility map[string]bool, available map[TypeID]bool, fields map[TypeID]map[string]bool) []MemberDescriptor {
	result := make([]MemberDescriptor, 0, len(input))
	for _, member := range input {
		if !visibility[member.ID] || !available[member.Owner] || !typeReferenceAvailable(member.Input, available) || !typeReferenceAvailable(member.Output, available) {
			continue
		}
		if member.Kind == "field" && !fields[member.Owner][member.Name] {
			continue
		}
		if filterCollectionQuery(member.Collection, collectionVisibility, available) {
			member.Capabilities = removeString(member.Capabilities, CollectionQueryCapability)
		}
		result = append(result, member)
	}
	return result
}

func filterCollectionQuery(collection *CollectionDescriptor, visibility map[string]bool, available map[TypeID]bool) bool {
	if collection == nil || collection.Query == nil {
		return false
	}
	fields := collection.Query.Fields[:0]
	for _, field := range collection.Query.Fields {
		if visibility[field.ID] && typeReferenceAvailable(field.Type, available) && typeReferenceAvailable(field.ElementType, available) {
			fields = append(fields, field)
		}
	}
	collection.Query.Fields = fields
	if len(fields) == 0 {
		collection.Query = nil
		return true
	}
	return false
}

func removeString(values []string, removed string) []string {
	result := values[:0]
	for _, value := range values {
		if value != removed {
			result = append(result, value)
		}
	}
	return result
}

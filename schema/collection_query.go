package schema

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

const (
	CollectionQueryCapability    = "collection.query-1"
	CollectionCaseFoldCapability = "collection.case-fold-1"
)

type FilterOperator string

const (
	FilterEqual        FilterOperator = "eq"
	FilterNotEqual     FilterOperator = "ne"
	FilterLess         FilterOperator = "lt"
	FilterLessEqual    FilterOperator = "lte"
	FilterGreater      FilterOperator = "gt"
	FilterGreaterEqual FilterOperator = "gte"
	FilterIn           FilterOperator = "in"
	FilterNotIn        FilterOperator = "not-in"
	FilterIsNull       FilterOperator = "is-null"
	FilterIsNotNull    FilterOperator = "is-not-null"
	FilterExists       FilterOperator = "exists"
	FilterNotExists    FilterOperator = "not-exists"
	FilterAny          FilterOperator = "any"
	FilterAll          FilterOperator = "all"
	FilterRegex        FilterOperator = "regex"
	FilterFullText     FilterOperator = "full-text"
	FilterGeoWithin    FilterOperator = "geo-within"
)

type SortDirection string

const (
	SortAscending  SortDirection = "asc"
	SortDescending SortDirection = "desc"
)

type NullPlacement string

const (
	NullsFirst NullPlacement = "first"
	NullsLast  NullPlacement = "last"
)

type CollectionSortDescriptor struct {
	Directions      []SortDirection `json:"directions"`
	Nulls           []NullPlacement `json:"nulls"`
	Collation       string          `json:"collation"`
	CaseSensitivity string          `json:"caseSensitivity"`
}

type CollectionQueryFieldDescriptor struct {
	ID           string                    `json:"id"`
	Path         []string                  `json:"path"`
	Type         TypeID                    `json:"type"`
	ElementType  TypeID                    `json:"elementType,omitempty"`
	Nullable     bool                      `json:"nullable,omitempty"`
	Missing      bool                      `json:"missing,omitempty"`
	Operators    []FilterOperator          `json:"operators,omitempty"`
	Cost         uint64                    `json:"cost"`
	Indexed      bool                      `json:"indexed,omitempty"`
	Capabilities []string                  `json:"capabilities,omitempty"`
	Sort         *CollectionSortDescriptor `json:"sort,omitempty"`
}

type CollectionTieBreakerDescriptor struct {
	Type            TypeID   `json:"type"`
	Collation       string   `json:"collation"`
	CaseSensitivity string   `json:"caseSensitivity"`
	Capabilities    []string `json:"capabilities,omitempty"`
}

type CollectionQueryLimits struct {
	MaxDepth      uint64 `json:"maxDepth"`
	MaxPredicates uint64 `json:"maxPredicates"`
	MaxMembership uint64 `json:"maxMembership"`
	MaxSortKeys   uint64 `json:"maxSortKeys"`
	MaxCost       uint64 `json:"maxCost"`
}

// CollectionQueryDescriptor is the authorized, provider-neutral filter and
// sort contract for one collection. Backend column and expression mappings are
// deliberately excluded from discovery.
type CollectionQueryDescriptor struct {
	Capability string                           `json:"capability"`
	Fields     []CollectionQueryFieldDescriptor `json:"fields"`
	TieBreaker CollectionTieBreakerDescriptor   `json:"tieBreaker"`
	Limits     CollectionQueryLimits            `json:"limits"`
}

func normalizeCollectionQuery(query *CollectionQueryDescriptor) {
	if query == nil {
		return
	}
	for index := range query.Fields {
		field := &query.Fields[index]
		slices.Sort(field.Operators)
		field.Operators = slices.Compact(field.Operators)
		field.Capabilities = sortedUnique(field.Capabilities)
		if field.Sort != nil {
			slices.Sort(field.Sort.Directions)
			field.Sort.Directions = slices.Compact(field.Sort.Directions)
			slices.Sort(field.Sort.Nulls)
			field.Sort.Nulls = slices.Compact(field.Sort.Nulls)
		}
	}
	query.TieBreaker.Capabilities = sortedUnique(query.TieBreaker.Capabilities)
	slices.SortFunc(query.Fields, func(left, right CollectionQueryFieldDescriptor) int {
		return strings.Compare(left.ID, right.ID)
	})
}

func cloneCollectionQuery(input *CollectionQueryDescriptor) *CollectionQueryDescriptor {
	if input == nil {
		return nil
	}
	result := *input
	result.TieBreaker.Capabilities = slices.Clone(input.TieBreaker.Capabilities)
	result.Fields = slices.Clone(input.Fields)
	for index := range result.Fields {
		result.Fields[index].Path = slices.Clone(input.Fields[index].Path)
		result.Fields[index].Operators = slices.Clone(input.Fields[index].Operators)
		result.Fields[index].Capabilities = slices.Clone(input.Fields[index].Capabilities)
		if input.Fields[index].Sort != nil {
			sortDescriptor := *input.Fields[index].Sort
			sortDescriptor.Directions = slices.Clone(sortDescriptor.Directions)
			sortDescriptor.Nulls = slices.Clone(sortDescriptor.Nulls)
			result.Fields[index].Sort = &sortDescriptor
		}
	}
	return &result
}

// CloneCollectionQueryDescriptor returns a deep copy suitable for crossing
// registry and discovery boundaries.
func CloneCollectionQueryDescriptor(input *CollectionQueryDescriptor) *CollectionQueryDescriptor {
	return cloneCollectionQuery(input)
}

func cloneCollectionDescriptor(input *CollectionDescriptor) *CollectionDescriptor {
	if input == nil {
		return nil
	}
	cloned := CollectionDescriptor{
		MaxPageSize: input.MaxPageSize, TotalCountCost: input.TotalCountCost,
		Query: CloneCollectionQueryDescriptor(input.Query),
	}
	return &cloned
}

// ValidateCollectionQueryDescriptor validates the provider-neutral shape. Type
// references are validated as part of the containing schema document.
func ValidateCollectionQueryDescriptor(query *CollectionQueryDescriptor) error {
	if query == nil {
		return nil
	}
	if query.Capability != CollectionQueryCapability {
		return fmt.Errorf("collection query requires capability %q", CollectionQueryCapability)
	}
	if len(query.Fields) == 0 {
		return errors.New("collection query requires at least one field")
	}
	limits := query.Limits
	if limits.MaxDepth == 0 || limits.MaxPredicates == 0 || limits.MaxMembership == 0 || limits.MaxSortKeys == 0 || limits.MaxCost == 0 {
		return errors.New("collection query requires positive query limits")
	}
	if limits.MaxCost > MaxDirectiveCost {
		return errors.New("collection query cost exceeds the portable maximum")
	}
	if query.TieBreaker.Type == "" || !validCollectionCollation(query.TieBreaker.Collation) || !validCaseSensitivity(query.TieBreaker.CaseSensitivity) {
		return errors.New("invalid collection query tie breaker")
	}
	if query.TieBreaker.CaseSensitivity == "insensitive" && !slices.Contains(query.TieBreaker.Capabilities, CollectionCaseFoldCapability) {
		return fmt.Errorf("collection query tie breaker case-insensitive sort requires capability %q", CollectionCaseFoldCapability)
	}
	seenIDs := make(map[string]bool, len(query.Fields))
	seenPaths := make(map[string]bool, len(query.Fields))
	for _, field := range query.Fields {
		if !typeIDPattern.MatchString(field.ID) || len(field.Path) == 0 || field.Type == "" || field.Cost == 0 || field.Cost > limits.MaxCost {
			return fmt.Errorf("invalid collection query field %q", field.ID)
		}
		if seenIDs[field.ID] {
			return fmt.Errorf("duplicate collection query field identity %q", field.ID)
		}
		seenIDs[field.ID] = true
		for _, segment := range field.Path {
			if !typeIDPattern.MatchString(segment) {
				return fmt.Errorf("invalid collection query field path for %q", field.ID)
			}
		}
		pathKey := strings.Join(field.Path, "\x00")
		if seenPaths[pathKey] {
			return fmt.Errorf("duplicate collection query field path %q", strings.Join(field.Path, "."))
		}
		seenPaths[pathKey] = true
		if len(field.Operators) == 0 && field.Sort == nil {
			return fmt.Errorf("collection query field %q is neither filterable nor sortable", field.ID)
		}
		if err := validateFilterOperators(field); err != nil {
			return err
		}
		if err := validateCollectionSort(field); err != nil {
			return err
		}
	}
	return nil
}

// ValidateCollectionQueryTypes binds a portable query contract to an exact
// immutable type snapshot before it is registered with a provider.
func ValidateCollectionQueryTypes(query *CollectionQueryDescriptor, snapshot Snapshot) error {
	return validateCollectionQueryTypeReferences(query, func(id TypeID) (collectionQueryType, bool) {
		descriptor, exists := snapshot.Lookup(id)
		return collectionQueryType{kind: descriptor.Kind, element: descriptor.Element}, exists
	})
}

func validateCollectionCapability(owner string, collection *CollectionDescriptor, capabilities []string) error {
	queryDeclared := collection != nil && collection.Query != nil
	capabilityDeclared := slices.Contains(capabilities, CollectionQueryCapability)
	if queryDeclared != capabilityDeclared {
		return fmt.Errorf("%s collection query descriptor and capability must be declared together", owner)
	}
	return nil
}

func validateFilterOperators(field CollectionQueryFieldDescriptor) error {
	seen := make(map[FilterOperator]bool, len(field.Operators))
	for _, operator := range field.Operators {
		if seen[operator] || !validFilterOperator(operator) {
			return fmt.Errorf("invalid collection filter operator %q", operator)
		}
		seen[operator] = true
		if (operator == FilterIsNull || operator == FilterIsNotNull) && !field.Nullable {
			return fmt.Errorf("collection filter operator %q requires nullable values", operator)
		}
		if (operator == FilterExists || operator == FilterNotExists) && !field.Missing {
			return fmt.Errorf("collection filter operator %q requires missing values", operator)
		}
		capability := operatorCapability(operator)
		if capability != "" && !slices.Contains(field.Capabilities, capability) {
			return fmt.Errorf("collection %s operator requires capability %q", operator, capability)
		}
	}
	return nil
}

func validFilterOperator(operator FilterOperator) bool {
	return slices.Contains([]FilterOperator{
		FilterEqual, FilterNotEqual, FilterLess, FilterLessEqual, FilterGreater, FilterGreaterEqual,
		FilterIn, FilterNotIn, FilterIsNull, FilterIsNotNull, FilterExists, FilterNotExists,
		FilterAny, FilterAll, FilterRegex, FilterFullText, FilterGeoWithin,
	}, operator)
}

func operatorCapability(operator FilterOperator) string {
	switch operator {
	case FilterRegex:
		return "collection.regex-1"
	case FilterFullText:
		return "collection.full-text-1"
	case FilterGeoWithin:
		return "collection.geospatial-1"
	case FilterEqual, FilterNotEqual, FilterLess, FilterLessEqual, FilterGreater, FilterGreaterEqual,
		FilterIn, FilterNotIn, FilterIsNull, FilterIsNotNull, FilterExists, FilterNotExists, FilterAny, FilterAll:
		return ""
	default:
		return ""
	}
}

func validateCollectionSort(field CollectionQueryFieldDescriptor) error {
	descriptor := field.Sort
	if descriptor == nil {
		return nil
	}
	if len(descriptor.Directions) == 0 || len(descriptor.Nulls) == 0 {
		return fmt.Errorf("collection sort field %q requires directions and null placement", field.ID)
	}
	for _, direction := range descriptor.Directions {
		if direction != SortAscending && direction != SortDescending {
			return fmt.Errorf("invalid collection sort direction %q", direction)
		}
	}
	for _, placement := range descriptor.Nulls {
		if placement != NullsFirst && placement != NullsLast {
			return fmt.Errorf("invalid collection null placement %q", placement)
		}
	}
	if !validCollectionCollation(descriptor.Collation) {
		return fmt.Errorf("invalid collection sort collation %q", descriptor.Collation)
	}
	if !validCaseSensitivity(descriptor.CaseSensitivity) {
		return fmt.Errorf("invalid collection sort case sensitivity %q", descriptor.CaseSensitivity)
	}
	if descriptor.CaseSensitivity == "insensitive" && !slices.Contains(field.Capabilities, CollectionCaseFoldCapability) {
		return fmt.Errorf("collection sort field %q requires capability %q", field.ID, CollectionCaseFoldCapability)
	}
	return nil
}

func validCollectionCollation(value string) bool {
	return value == "binary" || value == "numeric" || value == "unicode-code-point"
}

func validCaseSensitivity(value string) bool {
	return value == "sensitive" || value == "insensitive"
}

func validateCollectionQueryReferences(owner string, query *CollectionQueryDescriptor, types map[TypeID]schemaTypeReference) error {
	err := validateCollectionQueryTypeReferences(query, func(id TypeID) (collectionQueryType, bool) {
		reference, exists := types[id]
		return collectionQueryType{kind: reference.kind, element: reference.element}, exists
	})
	if err != nil {
		return fmt.Errorf("%s %w", owner, err)
	}
	return nil
}

type collectionQueryType struct {
	kind    TypeKind
	element TypeID
}

func validateCollectionQueryTypeReferences(query *CollectionQueryDescriptor, lookup func(TypeID) (collectionQueryType, bool)) error {
	if query == nil {
		return nil
	}
	tieBreaker, exists := lookup(query.TieBreaker.Type)
	if !exists {
		return errors.New("collection query tie breaker references an unknown type")
	}
	if !collectionComparableType(query.TieBreaker.Type, tieBreaker.kind) {
		return errors.New("collection query tie breaker requires a scalar or enum type")
	}
	if !collectionOrderPolicyCompatible(query.TieBreaker.Type, tieBreaker.kind, query.TieBreaker.Collation, query.TieBreaker.CaseSensitivity) {
		return errors.New("collection query tie breaker has an incompatible ordering policy")
	}
	for _, field := range query.Fields {
		reference, exists := lookup(field.Type)
		if !exists {
			return errors.New("collection query field references an unknown type")
		}
		if err := validateCollectionOperatorTypes(field, reference, lookup); err != nil {
			return err
		}
		if field.Sort != nil && !collectionOrderPolicyCompatible(field.Type, reference.kind, field.Sort.Collation, field.Sort.CaseSensitivity) {
			return fmt.Errorf("collection query field %q has an incompatible sort ordering policy", field.ID)
		}
		if reference.kind == ListType {
			if field.ElementType == "" || field.ElementType != reference.element {
				return fmt.Errorf("collection query list field %q requires matching element type", field.ID)
			}
			if field.Sort != nil {
				return fmt.Errorf("collection query list field %q cannot be sorted", field.ID)
			}
		} else if field.ElementType != "" {
			return fmt.Errorf("collection query scalar field %q cannot declare element type", field.ID)
		}
	}
	return nil
}

func validateCollectionOperatorTypes(field CollectionQueryFieldDescriptor, reference collectionQueryType, lookup func(TypeID) (collectionQueryType, bool)) error {
	if field.Sort != nil && !collectionOrderedType(field.Type, reference.kind) {
		return fmt.Errorf("collection query field %q has an incompatible sort type", field.ID)
	}
	if reference.kind == ListType && !slices.Contains(field.Operators, FilterAny) && !slices.Contains(field.Operators, FilterAll) {
		return fmt.Errorf("collection query list field %q requires any or all", field.ID)
	}
	for _, operator := range field.Operators {
		switch operator {
		case FilterEqual, FilterNotEqual, FilterIn, FilterNotIn:
			if !collectionComparableType(field.Type, reference.kind) {
				return fmt.Errorf("collection query field %q has an incompatible equality or membership type", field.ID)
			}
		case FilterLess, FilterLessEqual, FilterGreater, FilterGreaterEqual:
			if !collectionOrderedType(field.Type, reference.kind) {
				return fmt.Errorf("collection query field %q has an incompatible ordered comparison type", field.ID)
			}
		case FilterRegex, FilterFullText:
			if field.Type != TypeID(String) {
				return fmt.Errorf("collection query field %q has an incompatible text operator type", field.ID)
			}
		case FilterGeoWithin:
			if reference.kind != ScalarType || knownScalar(ScalarKind(field.Type)) {
				return fmt.Errorf("collection query field %q has an incompatible geospatial operator type", field.ID)
			}
		case FilterAny, FilterAll:
			if reference.kind != ListType {
				return fmt.Errorf("collection query field %q has incompatible list operators", field.ID)
			}
		case FilterIsNull, FilterIsNotNull, FilterExists, FilterNotExists:
			continue
		default:
			return fmt.Errorf("collection query field %q has an invalid operator", field.ID)
		}
	}
	if reference.kind == ListType {
		element, exists := lookup(reference.element)
		if !exists || !collectionComparableType(reference.element, element.kind) {
			return fmt.Errorf("collection query list field %q requires a scalar or enum element", field.ID)
		}
	} else if slices.Contains(field.Operators, FilterAny) || slices.Contains(field.Operators, FilterAll) {
		return fmt.Errorf("collection query scalar field %q cannot use any or all", field.ID)
	}
	return nil
}

func collectionComparableType(id TypeID, kind TypeKind) bool {
	return kind == ScalarType || kind == EnumType || knownScalar(ScalarKind(id))
}

func collectionOrderedType(id TypeID, kind TypeKind) bool {
	if kind == EnumType {
		return true
	}
	return slices.Contains([]ScalarKind{String, ID, Int32, Float64, Int64, UInt64, BigInt, Decimal, Timestamp, Duration, UUID}, ScalarKind(id))
}

func collectionOrderPolicyCompatible(id TypeID, kind TypeKind, collation, caseSensitivity string) bool {
	if kind == EnumType || id == TypeID(String) || id == TypeID(ID) {
		return (collation == "binary" || collation == "unicode-code-point") && (caseSensitivity == "sensitive" || caseSensitivity == "insensitive")
	}
	if slices.Contains([]ScalarKind{Int32, Float64, Int64, UInt64, BigInt, Decimal, Duration}, ScalarKind(id)) {
		return collation == "numeric" && caseSensitivity == "sensitive"
	}
	if id == TypeID(Timestamp) || id == TypeID(UUID) {
		return (collation == "binary" || collation == "unicode-code-point") && caseSensitivity == "sensitive"
	}
	return false
}

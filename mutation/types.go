// Package mutation provides provider-neutral conditional mutation primitives.
// Application storage providers remain responsible for checking revisions and
// committing writes atomically at their own storage boundary.
package mutation

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/valksor/naatre/schema"
)

const (
	TypedUpdateCapability     = "mutation.typed-update-1"
	JSONPatchCapability       = "mutation.json-patch-1"
	ReadConsistencyCapability = "mutation.read-consistency-1"
)

const (
	CodePreconditionRequired        = "PRECONDITION_REQUIRED"
	CodeRevisionConflict            = "REVISION_CONFLICT"
	CodeEntityNotFound              = "ENTITY_NOT_FOUND"
	CodePreconditionUnsupported     = "PRECONDITION_UNSUPPORTED"
	CodeUnauthorized                = "UNAUTHORIZED"
	CodeForbiddenProperty           = "FORBIDDEN_PROPERTY"
	CodeInvalidPatchPath            = "INVALID_PATCH_PATH"
	CodeRequiredField               = "REQUIRED_FIELD"
	CodeNonNullableField            = "NON_NULLABLE_FIELD"
	CodeImmutableField              = "IMMUTABLE_FIELD"
	CodeOneOfViolation              = "ONE_OF_VIOLATION"
	CodeDuplicateEdit               = "DUPLICATE_EDIT"
	CodeListIndexConflict           = "LIST_INDEX_CONFLICT"
	CodePatchTestFailed             = "PATCH_TEST_FAILED"
	CodeInvalidUpdate               = "INVALID_UPDATE"
	CodeUpdateCapabilityUnsupported = "UPDATE_CAPABILITY_UNSUPPORTED"
	CodeReadConsistencyUnsupported  = "READ_CONSISTENCY_UNSUPPORTED"
	CodeProviderFailed              = "MUTATION_PROVIDER_FAILED"
)

// Error is safe to return across a protocol boundary. It never includes the
// current entity value or revision. Path identifies only caller-supplied input.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    []any  `json:"path,omitempty"`
	cause   error
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }
func (e *Error) Unwrap() error { return e.cause }

func failure(code, message string, path []any, cause error) error {
	result := new(Error)
	result.Code = code
	result.Message = message
	result.Path = slices.Clone(path)
	result.cause = cause
	return result
}

// Revision is an opaque application revision token. Callers may retain and
// compare it for equality but must not parse or order it.
type Revision string

func ParseRevision(value string) (Revision, error) {
	if len(value) < 16 || len(value) > 256 || strings.TrimSpace(value) != value {
		return "", errors.New("invalid opaque revision token")
	}
	return Revision(value), nil
}

type FieldShape string

const (
	ScalarField FieldShape = "scalar"
	ListField   FieldShape = "list"
	MapField    FieldShape = "map"
)

// FieldDescriptor is the complete writable allowlist for one mutation. Path
// is an exact RFC 6901 pointer and never a provider column or reflection path.
type FieldDescriptor struct {
	ID        string        `json:"id"`
	Path      string        `json:"path"`
	Type      schema.TypeID `json:"type"`
	Shape     FieldShape    `json:"shape"`
	Required  bool          `json:"required,omitempty"`
	Nullable  bool          `json:"nullable,omitempty"`
	Immutable bool          `json:"immutable,omitempty"`
	OneOf     string        `json:"oneOf,omitempty"`
}

type Descriptor struct {
	Capability      string            `json:"capability"`
	RequireRevision bool              `json:"requireRevision,omitempty"`
	JSONPatch       bool              `json:"jsonPatch,omitempty"`
	MaxOperations   uint64            `json:"maxOperations"`
	Fields          []FieldDescriptor `json:"fields"`
}

var identityPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)

// NewDescriptor validates and clones a mutation contract.
func NewDescriptor(input Descriptor) (Descriptor, error) {
	if input.Capability != TypedUpdateCapability || input.MaxOperations == 0 || input.MaxOperations > 1<<20 || len(input.Fields) == 0 {
		return Descriptor{}, errors.New("invalid typed update descriptor")
	}
	result := cloneDescriptor(input)
	seenIDs := make(map[string]bool, len(result.Fields))
	seenPaths := make(map[string]bool, len(result.Fields))
	for _, field := range result.Fields {
		if !identityPattern.MatchString(field.ID) || !validPointer(field.Path) || field.Type == "" {
			return Descriptor{}, fmt.Errorf("invalid update field %q", field.ID)
		}
		if field.Shape != ScalarField && field.Shape != ListField && field.Shape != MapField {
			return Descriptor{}, fmt.Errorf("update field %q has invalid shape", field.ID)
		}
		if seenIDs[field.ID] || seenPaths[field.Path] {
			return Descriptor{}, fmt.Errorf("duplicate update field %q", field.ID)
		}
		if field.OneOf != "" && !identityPattern.MatchString(field.OneOf) {
			return Descriptor{}, fmt.Errorf("update field %q has invalid one-of identity", field.ID)
		}
		seenIDs[field.ID], seenPaths[field.Path] = true, true
	}
	slices.SortFunc(result.Fields, func(left, right FieldDescriptor) int {
		return strings.Compare(left.ID, right.ID)
	})
	return result, nil
}

func cloneDescriptor(input Descriptor) Descriptor {
	result := input
	result.Fields = slices.Clone(input.Fields)
	return result
}

func validPointer(value string) bool {
	if value == "" || value[0] != '/' || strings.HasSuffix(value, "/") || strings.Contains(value[1:], "/") {
		return false
	}
	for index := 1; index < len(value); index++ {
		if value[index] != '~' {
			continue
		}
		if index+1 >= len(value) || (value[index+1] != '0' && value[index+1] != '1') {
			return false
		}
		index++
	}
	return true
}

func fieldName(path string) string {
	segment := strings.TrimPrefix(path, "/")
	segment = strings.ReplaceAll(segment, "~1", "/")
	return strings.ReplaceAll(segment, "~0", "~")
}

type EditAction string

const (
	EditSet         EditAction = "set"
	EditRemove      EditAction = "remove"
	EditListAppend  EditAction = "list-append"
	EditListInsert  EditAction = "list-insert"
	EditListReplace EditAction = "list-replace"
	EditListRemove  EditAction = "list-remove"
	EditMapSet      EditAction = "map-set"
	EditMapRemove   EditAction = "map-remove"
)

// Edit is explicitly tagged. An omitted field has no Edit, while Set with a
// nil Value is explicit null and Remove is application-level absence. Values
// crossing this API must already be coerced to the FieldDescriptor schema type.
type Edit struct {
	Field  string     `json:"field"`
	Action EditAction `json:"action"`
	Value  any        `json:"value,omitempty"`
	Index  *int       `json:"index,omitempty"`
	Key    *string    `json:"key,omitempty"`
}

type UpdateInput struct {
	Edits []Edit `json:"edits"`
}

type PatchOperation struct {
	Operation string `json:"op"`
	Path      string `json:"path"`
	Value     any    `json:"value,omitempty"`
}

// FieldAuthorizer returns whether the current principal may edit a stable
// registered field identity. A nil authorizer permits every registered field.
type FieldAuthorizer func(fieldID string) bool

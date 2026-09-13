package reflectadapter

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

const tagName = "naatre"

// Binding explicitly allowlists one exported ordinary method.
type Binding struct {
	GoName     string
	Descriptor runtime.Descriptor
}

// Options supplies the immutable schema and explicit exposure configuration.
// Descriptors is keyed by the name in a `naatre` field tag. Only is an
// optional Go-field-name filter useful when several tagged profiles share one
// carrier type; an empty slice compiles every tagged field.
type Options struct {
	Types       schema.Snapshot
	Descriptors map[string]runtime.Descriptor
	Allowlist   []Binding
	Only        []string
}

// Compiled is an immutable set of ordinary runtime definitions.
type Compiled struct {
	definitions []runtime.Definition
}

// Definitions returns an isolated copy of the compiled definitions.
func (c *Compiled) Definitions() []runtime.Definition {
	if c == nil {
		return nil
	}
	return slices.Clone(c.definitions)
}

// Register submits every compiled definition to the ordinary runtime registry.
func (c *Compiled) Register(registry *runtime.Registry) error {
	if c == nil {
		return errors.New("reflectadapter: compiled adapter is nil")
	}
	if registry == nil {
		return errors.New("reflectadapter: runtime registry is nil")
	}
	for index, definition := range c.definitions {
		if err := registry.Register(definition); err != nil {
			return fmt.Errorf("reflectadapter: register definition %d: %w", index, err)
		}
	}
	return nil
}

// Compile resolves tags, allowlisted methods, signatures, and converters once.
func Compile(target any, options Options) (*Compiled, error) {
	targetValue := reflect.ValueOf(target)
	if !targetValue.IsValid() {
		return nil, errors.New("reflectadapter: target is nil")
	}
	targetType, err := targetStructType(targetValue.Type())
	if err != nil {
		return nil, err
	}
	only := makeStringSet(options.Only)
	definitions := make([]runtime.Definition, 0, len(options.Descriptors)+len(options.Allowlist))
	keys := make(map[string]string)

	tagged, err := taggedFields(targetType, only)
	if err != nil {
		return nil, err
	}
	for _, field := range tagged {
		descriptor, ok := options.Descriptors[field.tag]
		if !ok {
			return nil, fmt.Errorf("reflectadapter: tagged field %s references missing descriptor %q", field.path, field.tag)
		}
		definition, compileErr := compileTaggedField(targetValue, targetType, field, descriptor, options.Types)
		if compileErr != nil {
			return nil, compileErr
		}
		if err := addDefinition(&definitions, keys, field.path, descriptor, definition); err != nil {
			return nil, err
		}
	}

	seenMethods := make(map[string]bool, len(options.Allowlist))
	for _, binding := range options.Allowlist {
		if binding.GoName == "" {
			return nil, errors.New("reflectadapter: allowlisted method requires a Go name")
		}
		if seenMethods[binding.GoName] {
			return nil, fmt.Errorf("reflectadapter: allowlisted method %q is duplicated", binding.GoName)
		}
		seenMethods[binding.GoName] = true
		definition, compileErr := compileMethod(targetValue, targetType, binding, options.Types)
		if compileErr != nil {
			return nil, compileErr
		}
		if err := addDefinition(&definitions, keys, binding.GoName, binding.Descriptor, definition); err != nil {
			return nil, err
		}
	}

	if len(definitions) == 0 {
		return nil, errors.New("reflectadapter: target exposes no tagged fields or allowlisted methods")
	}
	validator := runtime.NewRegistry(options.Types)
	for index, definition := range definitions {
		if err := validator.Register(definition); err != nil {
			return nil, fmt.Errorf("reflectadapter: compiled definition %d is invalid: %w", index, err)
		}
	}
	return &Compiled{definitions: definitions}, nil
}

func targetStructType(input reflect.Type) (reflect.Type, error) {
	current := input
	for current.Kind() == reflect.Pointer {
		current = current.Elem()
	}
	if current.Kind() != reflect.Struct {
		return nil, fmt.Errorf("reflectadapter: target %s must be a struct or pointer to struct", input)
	}
	return current, nil
}

func makeStringSet(values []string) map[string]bool {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]bool, len(values))
	for _, value := range values {
		if value != "" {
			result[value] = true
		}
	}
	return result
}

func addDefinition(definitions *[]runtime.Definition, keys map[string]string, origin string, descriptor runtime.Descriptor, definition runtime.Definition) error {
	key := definitionKey(descriptor)
	if prior, exists := keys[key]; exists {
		return fmt.Errorf("reflectadapter: %s and %s collide on runtime definition %s", prior, origin, key)
	}
	keys[key] = origin
	*definitions = append(*definitions, definition)
	return nil
}

func definitionKey(descriptor runtime.Descriptor) string {
	return strings.Join([]string{string(descriptor.Scope), string(descriptor.Kind), string(descriptor.Owner), string(descriptor.Member), descriptor.Name}, ":")
}

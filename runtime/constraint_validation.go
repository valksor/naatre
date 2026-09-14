package runtime

import (
	"errors"
	"fmt"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

func (v *planValidator) addCoercionError(defaultCode, defaultClause, context string, err error, source protocol.Source) {
	var constraints *schema.ConstraintError
	if !errors.As(err, &constraints) {
		v.add(defaultCode, defaultClause, fmt.Sprintf("%s: %v", context, err), source)
		return
	}
	for _, violation := range constraints.Violations() {
		located := source
		located.Pointer += violation.Path
		v.add(violation.Code, "VALID-200", fmt.Sprintf("%s violates constraint %q", context, violation.ID), located)
	}
}

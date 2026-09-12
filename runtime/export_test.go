package runtime

import (
	"context"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

// ExecuteDefinitionsForTest exercises invocation containment and output
// completion independently of document composition.
func ExecuteDefinitionsForTest(ctx context.Context, types schema.Snapshot, definitions []Definition) Outcome {
	selections := make([]plannedSelection, len(definitions))
	for index, definition := range definitions {
		selections[index] = plannedSelection{
			definition: definition,
			outputName: definition.descriptor.Name,
			source:     protocol.Source{},
		}
	}
	return (&Plan{operationName: "test", kind: protocol.Query, selections: selections, types: types}).executeFlat(ctx)
}

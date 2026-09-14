package collectionquery

import (
	"math"
	"testing"

	"github.com/valksor/naatre/schema"
)

func TestChargeRejectsIntegerOverflow(t *testing.T) {
	t.Parallel()
	state := prepareState{query: schema.CollectionQueryDescriptor{Limits: schema.CollectionQueryLimits{MaxCost: math.MaxUint64}}, cost: math.MaxUint64 - 1}
	if _, err := state.charge(preparedExpression{}, 2); err == nil {
		t.Fatal("charge accepted overflowing cost")
	}
}

package conformance_test

import (
	"encoding/json"
	"testing"

	"github.com/valksor/naatre/schema"
)

func TestPortableInputValueVectors(t *testing.T) {
	t.Parallel()
	var fixture struct {
		Profile string                  `json:"profile"`
		Types   []schema.TypeDescriptor `json:"types"`
		Vectors []inputValueVector      `json:"vectors"`
	}
	readFixture(t, "values.json", &fixture)
	if fixture.Profile != "core.value-1" || len(fixture.Types) == 0 || len(fixture.Vectors) == 0 {
		t.Fatal("value fixture requires its exact profile, types, and vectors")
	}
	catalog := schema.NewCatalog()
	for _, descriptor := range fixture.Types {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatalf("Register(%s): %v", descriptor.ID, err)
		}
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	runCases(t, "", fixture.Vectors, func(vector inputValueVector) string { return vector.Name }, func(t *testing.T, vector inputValueVector) {
		assertInputValueVector(t, types, vector)
	})
}

type inputValueVector struct {
	Name         string        `json:"name"`
	Type         schema.TypeID `json:"type"`
	Input        string        `json:"input"`
	Valid        *bool         `json:"valid"`
	Canonical    string        `json:"canonical"`
	ActiveMember string        `json:"activeMember"`
	EnumKnown    *bool         `json:"enumKnown"`
}

func assertInputValueVector(t *testing.T, types schema.Snapshot, vector inputValueVector) {
	t.Helper()
	if vector.Name == "" || vector.Type == "" || vector.Input == "" || vector.Valid == nil || (*vector.Valid && vector.Canonical == "") {
		t.Fatal("value vector is incomplete")
	}
	value, err := schema.CoerceInput(types, vector.Type, json.RawMessage(vector.Input), false)
	if !*vector.Valid {
		if err == nil {
			t.Fatal("accepted invalid value vector")
		}
		return
	}
	if err != nil {
		t.Fatalf("CoerceInput: %v", err)
	}
	canonical, err := value.MarshalJSON()
	if err != nil || string(canonical) != vector.Canonical {
		t.Fatalf("canonical = %s, %v; want %s", canonical, err, vector.Canonical)
	}
	if vector.ActiveMember != "" && value.ActiveMember() != vector.ActiveMember {
		t.Fatalf("active member = %q, want %q", value.ActiveMember(), vector.ActiveMember)
	}
	if vector.EnumKnown != nil {
		_, known, ok := value.Enum()
		if !ok || known != *vector.EnumKnown {
			t.Fatalf("enum known = %t, %t; want %t", known, ok, *vector.EnumKnown)
		}
	}
}

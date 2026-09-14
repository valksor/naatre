package generator_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/valksor/naatre/generator"
)

func TestGenerateProducesDeterministicVersionedBundle(t *testing.T) {
	t.Parallel()
	input := validModel()
	first, err := generator.Generate(input)
	if err != nil {
		t.Fatalf("Generate first: %v", err)
	}
	second, err := generator.Generate(input)
	if err != nil {
		t.Fatalf("Generate second: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("Generate is nondeterministic:\n%s\n%s", first, second)
	}

	var output struct {
		ModelVersion     string `json:"modelVersion"`
		GeneratorVersion string `json:"generatorVersion"`
		ProtocolVersion  string `json:"protocolVersion"`
		CanonicalVersion string `json:"canonicalVersion"`
		Schema           struct {
			Algorithm        string `json:"algorithm"`
			CanonicalVersion string `json:"canonicalVersion"`
			Digest           string `json:"digest"`
		} `json:"schema"`
		Operations []struct {
			Name      string         `json:"name"`
			Symbol    string         `json:"symbol"`
			Artifact  string         `json:"artifact"`
			Persisted map[string]any `json:"persisted"`
			Request   struct {
				Variables map[string]any `json:"variables"`
			} `json:"request"`
			Result map[string]any `json:"result"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(first, &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if output.ModelVersion != generator.ModelVersion || output.GeneratorVersion != generator.GeneratorVersion || output.ProtocolVersion != "1" || output.CanonicalVersion != "c14n-1" {
		t.Fatalf("output versions = %#v", output)
	}
	if output.Schema.Algorithm != "sha-256" || output.Schema.CanonicalVersion != "c14n-1" || len(output.Schema.Digest) != 64 {
		t.Fatalf("schema digest = %#v", output.Schema)
	}
	if len(output.Operations) != 1 || output.Operations[0].Name != "class" || output.Operations[0].Symbol != "Class_" || output.Operations[0].Artifact != "operations/Class_.json" {
		t.Fatalf("operations = %#v", output.Operations)
	}
	requestVariables := output.Operations[0].Request.Variables
	if value, present := requestVariables["nickname"]; !present || value != nil {
		t.Fatalf("explicit null was not preserved: %#v", requestVariables)
	}
	if list, ok := requestVariables["tags"].([]any); !ok || len(list) != 0 {
		t.Fatalf("empty list was not preserved: %#v", requestVariables)
	}
	if object, ok := requestVariables["filter"].(map[string]any); !ok || len(object) != 0 {
		t.Fatalf("empty object was not preserved: %#v", requestVariables)
	}
	if _, absent := requestVariables["omitted"]; absent {
		t.Fatalf("missing value became present: %#v", requestVariables)
	}
	if output.Operations[0].Persisted["digest"] == "" || output.Operations[0].Result["kind"] != "operation-result" {
		t.Fatalf("persisted/result output = %#v / %#v", output.Operations[0].Persisted, output.Operations[0].Result)
	}
}

func TestGenerateRejectsUnmappedCustomScalarsAndSymbolCollisions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   []byte
		code    string
		pointer string
	}{
		{name: "unmapped scalar", input: bytes.Replace(validModel(), []byte(`"scalarMappings":{"Money":"string"}`), []byte(`"scalarMappings":{}`), 1), code: "GENERATOR_UNMAPPED_SCALAR", pointer: "/configuration/scalarMappings/Money"},
		{name: "symbol collision", input: collisionModel(), code: "GENERATOR_SYMBOL_COLLISION", pointer: "/operations/1/name"},
		{name: "duplicate operation", input: duplicateOperationModel(), code: "GENERATOR_INVALID_IDENTIFIER", pointer: "/operations/1/name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := generator.Generate(test.input)
			var generationError *generator.Error
			if !errors.As(err, &generationError) {
				t.Fatalf("Generate error = %v", err)
			}
			diagnostics := generationError.Diagnostics()
			if len(diagnostics) != 1 || diagnostics[0].Code != test.code || diagnostics[0].Pointer != test.pointer {
				t.Fatalf("diagnostics = %#v", diagnostics)
			}
			if bytes.Contains([]byte(err.Error()), []byte("Money")) || bytes.Contains([]byte(err.Error()), []byte("class_")) {
				t.Fatalf("public error leaked rejected input: %v", err)
			}
		})
	}
}

func duplicateOperationModel() []byte {
	return []byte(`{
		"profile":"sdk.generation-1",
		"version":"naatre.generator-model-1",
		"protocolVersion":"1",
		"canonicalVersion":"c14n-1",
		"schema":{"revision":"schema-r1","types":[]},
		"configuration":{"scalarMappings":{}},
		"operations":[
			{"name":"same","document":{"operations":[{"name":"same","kind":"query","select":[]}]},"variables":[],"requestVariables":{},"result":{"kind":"object","fields":[]}},
			{"name":"same","document":{"operations":[{"name":"same","kind":"query","select":[]}]},"variables":[],"requestVariables":{},"result":{"kind":"object","fields":[]}}
		]
	}`)
}

func collisionModel() []byte {
	return []byte(`{
		"profile":"sdk.generation-1",
		"version":"naatre.generator-model-1",
		"protocolVersion":"1",
		"canonicalVersion":"c14n-1",
		"schema":{"revision":"schema-r1","types":[]},
		"configuration":{"scalarMappings":{}},
		"operations":[
			{"name":"class","document":{"operations":[{"name":"class","kind":"query","select":[]}]},"variables":[],"requestVariables":{},"result":{"kind":"object","fields":[]}},
			{"name":"class_","document":{"operations":[{"name":"class_","kind":"query","select":[]}]},"variables":[],"requestVariables":{},"result":{"kind":"object","fields":[]}}
		]
	}`)
}

func TestWriteArtifactsStaysInsideOutputRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := generator.WriteArtifacts(root, []generator.Artifact{{Path: "operations/GetUser.json", Content: []byte("{}\n")}}); err != nil {
		t.Fatalf("WriteArtifacts valid: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(root, "operations", "GetUser.json"))
	if err != nil || string(content) != "{}\n" {
		t.Fatalf("written artifact = %q, %v", content, err)
	}
	for _, path := range []string{"../escape", "/tmp/escape", "operations/../../escape"} {
		err := generator.WriteArtifacts(root, []generator.Artifact{{Path: path, Content: []byte("blocked")}})
		var generationError *generator.Error
		if !errors.As(err, &generationError) || generationError.Diagnostics()[0].Code != "GENERATOR_PATH_ESCAPE" {
			t.Fatalf("WriteArtifacts(%q) error = %v", path, err)
		}
	}
	symlink := filepath.Join(root, "linked")
	if err := os.Symlink(t.TempDir(), symlink); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	err = generator.WriteArtifacts(root, []generator.Artifact{{Path: "linked/escape", Content: []byte("blocked")}})
	var generationError *generator.Error
	if !errors.As(err, &generationError) || generationError.Diagnostics()[0].Code != "GENERATOR_PATH_ESCAPE" {
		t.Fatalf("WriteArtifacts symlink error = %v", err)
	}
}

func validModel() []byte {
	return []byte(`{
		"profile":"sdk.generation-1",
		"version":"naatre.generator-model-1",
		"protocolVersion":"1",
		"canonicalVersion":"c14n-1",
		"schema":{"revision":"schema-r1","types":[{"id":"Money","name":"Money","kind":"scalar","input":true,"output":true}]},
		"configuration":{"scalarMappings":{"Money":"string"}},
		"operations":[{
			"name":"class",
			"description":"untrusted </script> content",
			"document":{"operations":[{"name":"class","kind":"query","select":[]}]},
			"variables":[
				{"name":"id","type":"ID","required":true,"nullable":false},
				{"name":"nickname","type":"String","required":false,"nullable":true},
				{"name":"tags","type":"StringList","required":false,"nullable":false},
				{"name":"filter","type":"StringMap","required":false,"nullable":false}
			],
			"requestVariables":{"nickname":null,"tags":[],"filter":{}},
			"result":{"kind":"object","fields":[
				{"name":"profile","presence":"required","result":{"kind":"object","fields":[
					{"name":"display","presence":"required","result":{"kind":"scalar","type":"String"}},
					{"name":"nickname","presence":"optional","result":{"kind":"scalar","type":"String","nullable":true}}
				]}},
				{"name":"later","presence":"pending","result":{"kind":"scalar","type":"String"}}
			]}
		}]
	}`)
}

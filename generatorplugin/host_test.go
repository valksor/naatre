package generatorplugin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/valksor/naatre/generator"
	"github.com/valksor/naatre/generatorplugin"
)

func TestJavaScriptFixtureIsReproducibleAndTreatsModelTextAsData(t *testing.T) {
	t.Parallel()
	plugin := fixturePlugin(t)
	host := fixtureHost(t, generatorplugin.DefaultLimits())
	model := fixtureModel(t)
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	if err := host.Generate(context.Background(), plugin, model, nil, firstRoot); err != nil {
		t.Fatalf("Generate first: %v", err)
	}
	if err := host.Generate(context.Background(), plugin, model, json.RawMessage(`{"mode":"generate"}`), secondRoot); err != nil {
		t.Fatalf("Generate second: %v", err)
	}
	first := readGenerated(t, firstRoot)
	second := readGenerated(t, secondRoot)
	if !bytes.Equal(first, second) {
		t.Fatalf("plugin output is not reproducible:\n%s\n%s", first, second)
	}
	if bytes.Contains(first, []byte("\nexport const injected =")) || !bytes.Contains(first, []byte(`\nexport const injected = process.env.PROTECTED_CREDENTIAL`)) {
		t.Fatalf("hostile schema description escaped its string literal: %s", first)
	}
	command := exec.Command(fixtureRuntime(t), "--check", filepath.Join(firstRoot, "generated", "fixture.mjs"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated JavaScript is invalid: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(firstRoot), "injected.mjs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hostile model created an outside artifact: %v", err)
	}
}

func TestDiscoveryUsesOnlyExplicitLocationsAndRejectsBoundaryEscapes(t *testing.T) {
	t.Parallel()
	fixtureDirectory := fixtureDirectory(t)
	plugins, err := generatorplugin.Discover([]string{fixtureDirectory})
	if err != nil || len(plugins) != 1 || plugins[0].Manifest.ID != "fixture.javascript-typescript" {
		t.Fatalf("Discover fixture = %#v, %v", plugins, err)
	}

	outside := t.TempDir()
	copyFile(t, filepath.Join(fixtureDirectory, "plugin.mjs"), filepath.Join(outside, "plugin.mjs"))
	copyFile(t, filepath.Join(fixtureDirectory, "plugin.naatre-generator.json"), filepath.Join(outside, "outside.naatre-generator.json"))
	plugins, err = generatorplugin.Discover([]string{filepath.Join(fixtureDirectory, "plugin.naatre-generator.json")})
	if err != nil || len(plugins) != 1 {
		t.Fatalf("explicit manifest discovery = %#v, %v", plugins, err)
	}

	manifest := strings.Replace(
		string(readFile(t, filepath.Join(fixtureDirectory, "plugin.naatre-generator.json"))),
		`"entrypoint": "plugin.mjs"`,
		`"entrypoint": "../plugin.mjs"`,
		1,
	)
	escapeDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(escapeDirectory, "escape.naatre-generator.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	assertCode(t, discoverError(filepath.Join(escapeDirectory, "escape.naatre-generator.json")), "GENERATOR_PLUGIN_MANIFEST_INVALID")

	if runtime.GOOS != "windows" {
		symlink := filepath.Join(t.TempDir(), "plugin.naatre-generator.json")
		if err := os.Symlink(filepath.Join(fixtureDirectory, "plugin.naatre-generator.json"), symlink); err != nil {
			t.Fatal(err)
		}
		assertCode(t, discoverError(symlink), "GENERATOR_PLUGIN_NOT_FOUND")
	}
}

func TestHostRejectsPathEscapeAndNeverPublishesPluginDetails(t *testing.T) {
	t.Parallel()
	plugin := fixturePlugin(t)
	host := fixtureHost(t, generatorplugin.DefaultLimits())
	root := t.TempDir()
	err := host.Generate(context.Background(), plugin, fixtureModel(t), json.RawMessage(`{"mode":"path-escape"}`), root)
	assertCode(t, err, "GENERATOR_PATH_ESCAPE")
	if entries, readErr := os.ReadDir(root); readErr != nil || len(entries) != 0 {
		t.Fatalf("path escape left output behind: entries=%v err=%v", entries, readErr)
	}

	err = host.Generate(context.Background(), plugin, fixtureModel(t), json.RawMessage(`{"mode":"reject"}`), root)
	assertCode(t, err, "GENERATOR_PLUGIN_EXECUTION_FAILED")
	public := err.Error() + diagnosticsText(t, err)
	for _, secret := range []string{"PROTECTED_CREDENTIAL", "HostileFixture", fixtureDirectory(t), "private model"} {
		if strings.Contains(public, secret) {
			t.Fatalf("public failure leaked %q: %q", secret, public)
		}
	}
}

func TestHostCancellationAndResourceLimitsAreStable(t *testing.T) {
	t.Parallel()
	plugin := fixturePlugin(t)
	model := fixtureModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := fixtureHost(t, generatorplugin.DefaultLimits()).Generate(ctx, plugin, model, json.RawMessage(`{"mode":"hang"}`), t.TempDir())
	assertCode(t, err, "GENERATOR_CANCELLED")

	limits := generatorplugin.DefaultLimits()
	limits.StdoutBytes = 1024
	err = fixtureHost(t, limits).Generate(context.Background(), plugin, model, json.RawMessage(`{"mode":"oversized"}`), t.TempDir())
	assertCode(t, err, "GENERATOR_RESOURCE_LIMIT")

	limits = generatorplugin.DefaultLimits()
	limits.ModelBytes = len(model) - 1
	err = fixtureHost(t, limits).Generate(context.Background(), plugin, model, nil, t.TempDir())
	assertCode(t, err, "GENERATOR_RESOURCE_LIMIT")
}

func fixturePlugin(t *testing.T) generatorplugin.Plugin {
	t.Helper()
	plugins, err := generatorplugin.Discover([]string{fixtureDirectory(t)})
	if err != nil || len(plugins) != 1 {
		t.Fatalf("Discover fixture: %#v, %v", plugins, err)
	}
	return plugins[0]
}

func fixtureHost(t *testing.T, limits generatorplugin.Limits) generatorplugin.Host {
	t.Helper()
	return generatorplugin.Host{Runtimes: map[string]string{"node": fixtureRuntime(t)}, Limits: limits}
}

func fixtureRuntime(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("Node runtime: %v", err)
	}
	return path
}

func fixtureDirectory(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "conformance", "third_party", "generator-plugin", "javascript"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func fixtureModel(t *testing.T) []byte {
	t.Helper()
	return readFile(t, filepath.Join("..", "conformance", "v1", "generator-plugin-host-model.json"))
}

func readGenerated(t *testing.T, root string) []byte {
	t.Helper()
	return readFile(t, filepath.Join(root, "generated", "fixture.mjs"))
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func copyFile(t *testing.T, source, destination string) {
	t.Helper()
	if err := os.WriteFile(destination, readFile(t, source), 0o600); err != nil {
		t.Fatal(err)
	}
}

func discoverError(location string) error {
	_, err := generatorplugin.Discover([]string{location})
	return err
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	var typed interface {
		Diagnostics() []generator.Diagnostic
	}
	if !errors.As(err, &typed) {
		t.Fatalf("error = %v, want diagnostic error", err)
	}
	diagnostics := typed.Diagnostics()
	if len(diagnostics) != 1 || diagnostics[0].Code != code {
		t.Fatalf("diagnostics = %#v, want %s", diagnostics, code)
	}
}

func diagnosticsText(t *testing.T, err error) string {
	t.Helper()
	var typed *generatorplugin.Error
	if !errors.As(err, &typed) {
		t.Fatalf("error = %v, want generatorplugin.Error", err)
	}
	encoded, encodeErr := json.Marshal(typed.Diagnostics())
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	return string(encoded)
}

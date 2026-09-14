package conformancerunner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/valksor/naatre/generator"
	"github.com/valksor/naatre/generatorplugin"
)

const generatorPluginHostProfile = generatorplugin.HostProfile

type generatorPluginEvidence struct {
	Profile      string `json:"profile"`
	FixtureSuite string `json:"fixtureSuite"`
	Dependencies []struct {
		Name     string `json:"name"`
		Path     string `json:"path"`
		SHA256   string `json:"sha256"`
		Revision string `json:"revision"`
	} `json:"dependencies"`
	Plugin struct {
		Manifest string `json:"manifest"`
		Source   string `json:"source"`
	} `json:"plugin"`
	Support struct {
		Platforms     []string `json:"platforms"`
		Runtime       string   `json:"runtime"`
		RuntimeMajors []int    `json:"runtimeMajors"`
	} `json:"support"`
	Cases []struct {
		Name string `json:"name"`
		Mode string `json:"mode"`
		Code string `json:"code"`
	} `json:"cases"`
}

func (r *Runner) verifyGeneratorPluginHost(ctx context.Context, _ Request) Result {
	fixture := "v1/generator-plugin-host.json"
	run, code := r.prepareGeneratorPluginHost(fixture)
	if code == "" {
		code = verifyPluginReproducibility(ctx, run)
	}
	if code == "" {
		code = verifyPluginCases(ctx, run)
	}
	if code != "" {
		return failureResult(generatorPluginHostProfile, code, fixture)
	}
	return Result{Status: "passed", Capabilities: []string{generatorPluginHostProfile}, Evidence: run.evidence}
}

type generatorPluginRun struct {
	contract generatorPluginEvidence
	evidence []Evidence
	node     string
	plugin   generatorplugin.Plugin
	model    []byte
	host     generatorplugin.Host
}

func (r *Runner) prepareGeneratorPluginHost(fixture string) (generatorPluginRun, string) {
	content, err := os.ReadFile(filepath.Join(r.conformanceRoot, fixture))
	if err != nil {
		return generatorPluginRun{}, "GENERATOR_PLUGIN_CONFORMANCE_FAILED"
	}
	var contract generatorPluginEvidence
	if json.Unmarshal(content, &contract) != nil || contract.Profile != generatorPluginHostProfile || contract.FixtureSuite != r.manifest.FixtureVersion {
		return generatorPluginRun{}, "GENERATOR_PLUGIN_CONFORMANCE_FAILED"
	}
	repositoryRoot := filepath.Dir(r.conformanceRoot)
	evidence := []Evidence{{Fixture: fixture, SHA256: digestBytes(content)}}
	for _, dependency := range contract.Dependencies {
		dependencyContent, readErr := os.ReadFile(filepath.Join(repositoryRoot, filepath.FromSlash(dependency.Path)))
		if readErr != nil || digestBytes(dependencyContent) != dependency.SHA256 || dependency.Revision == "" {
			return generatorPluginRun{}, "GENERATOR_PLUGIN_CONFORMANCE_FAILED"
		}
		evidence = append(evidence, Evidence{Fixture: dependency.Path, SHA256: dependency.SHA256})
	}
	platform := runtime.GOOS + "-" + runtime.GOARCH
	if !slices.Contains(contract.Support.Platforms, platform) {
		return generatorPluginRun{}, "GENERATOR_PLUGIN_PLATFORM_UNSUPPORTED"
	}
	node, err := exec.LookPath(contract.Support.Runtime)
	if err != nil || !supportedNodeMajor(node, contract.Support.RuntimeMajors) {
		return generatorPluginRun{}, "GENERATOR_PLUGIN_RUNTIME_UNSUPPORTED"
	}
	manifest := filepath.Join(repositoryRoot, filepath.FromSlash(contract.Plugin.Manifest))
	plugins, err := generatorplugin.Discover([]string{manifest})
	if err != nil || len(plugins) != 1 {
		return generatorPluginRun{}, "GENERATOR_PLUGIN_CONFORMANCE_FAILED"
	}
	modelPath := dependencyPath(contract.Dependencies, "generator-model")
	model, err := os.ReadFile(filepath.Join(repositoryRoot, filepath.FromSlash(modelPath)))
	if err != nil {
		return generatorPluginRun{}, "GENERATOR_PLUGIN_CONFORMANCE_FAILED"
	}
	host := generatorplugin.Host{Runtimes: map[string]string{contract.Support.Runtime: node}}
	return generatorPluginRun{contract: contract, evidence: evidence, node: node, plugin: plugins[0], model: model, host: host}, ""
}

func verifyPluginReproducibility(ctx context.Context, run generatorPluginRun) string {
	firstRoot, err := os.MkdirTemp("", "naatre-generator-conformance-")
	if err != nil {
		return "GENERATOR_PLUGIN_CONFORMANCE_FAILED"
	}
	defer func() { _ = os.RemoveAll(firstRoot) }()
	secondRoot, err := os.MkdirTemp("", "naatre-generator-conformance-")
	if err != nil {
		return "GENERATOR_PLUGIN_CONFORMANCE_FAILED"
	}
	defer func() { _ = os.RemoveAll(secondRoot) }()
	if run.host.Generate(ctx, run.plugin, run.model, nil, firstRoot) != nil || run.host.Generate(ctx, run.plugin, run.model, json.RawMessage(`{"mode":"generate"}`), secondRoot) != nil {
		return "GENERATOR_PLUGIN_CONFORMANCE_FAILED"
	}
	first, firstErr := os.ReadFile(filepath.Join(firstRoot, "generated", "fixture.mjs"))
	second, secondErr := os.ReadFile(filepath.Join(secondRoot, "generated", "fixture.mjs"))
	if firstErr != nil || secondErr != nil || !bytes.Equal(first, second) || bytes.Contains(first, []byte("\nexport const injected =")) {
		return "GENERATOR_PLUGIN_OUTPUT_DRIFT"
	}
	if checked := exec.CommandContext(ctx, run.node, "--check", filepath.Join(firstRoot, "generated", "fixture.mjs")); checked.Run() != nil {
		return "GENERATOR_PLUGIN_SOURCE_INJECTION"
	}
	return ""
}

func verifyPluginCases(ctx context.Context, run generatorPluginRun) string {
	for _, testCase := range run.contract.Cases {
		if testCase.Mode == "generate" || testCase.Mode == "hostile-source" {
			continue
		}
		caseCtx := ctx
		cancel := func() {}
		if testCase.Mode == "hang" {
			caseCtx, cancel = context.WithTimeout(ctx, time.Second)
		}
		caseRoot, makeErr := os.MkdirTemp("", "naatre-generator-case-")
		if makeErr != nil {
			cancel()
			return "GENERATOR_PLUGIN_CONFORMANCE_FAILED"
		}
		err := run.host.Generate(caseCtx, run.plugin, run.model, json.RawMessage(`{"mode":`+strconv.Quote(testCase.Mode)+`}`), caseRoot)
		cancel()
		_ = os.RemoveAll(caseRoot)
		if diagnosticCode(err) != testCase.Code {
			return "GENERATOR_PLUGIN_CONFORMANCE_FAILED"
		}
	}
	return ""
}

func dependencyPath(dependencies []struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
	Revision string `json:"revision"`
}, name string) string {
	for _, dependency := range dependencies {
		if dependency.Name == name {
			return dependency.Path
		}
	}
	return ""
}

func supportedNodeMajor(node string, allowed []int) bool {
	command := exec.Command(node, "--version")
	command.Env = []string{"LANG=C", "LC_ALL=C"}
	output, err := command.Output()
	if err != nil {
		return false
	}
	version := strings.TrimPrefix(strings.TrimSpace(string(output)), "v")
	major, err := strconv.Atoi(strings.SplitN(version, ".", 2)[0])
	if err != nil {
		return false
	}
	for _, candidate := range allowed {
		if candidate == major {
			return true
		}
	}
	return false
}

func diagnosticCode(err error) string {
	var diagnostic interface {
		Diagnostics() []generator.Diagnostic
	}
	if !errors.As(err, &diagnostic) {
		return ""
	}
	diagnostics := diagnostic.Diagnostics()
	if len(diagnostics) != 1 {
		return ""
	}
	return diagnostics[0].Code
}

func digestBytes(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

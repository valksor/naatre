// Package pluginhost discovers and executes generator plugins without granting
// them the caller's environment, working directory, or final output root.
package generatorplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/valksor/naatre/generator"
)

const (
	ManifestVersion = "naatre.generator-plugin-manifest-1"
	HostProfile     = "sdk.generator-plugin-host-1"
	maxManifestSize = 64 * 1024
)

var portableName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,127}$`)

// Manifest is the installed plugin declaration. It describes integration
// metadata only; generator-model-1 remains the sole schema authority.
type Manifest struct {
	Version               string          `json:"version"`
	ID                    string          `json:"id"`
	AlgorithmVersion      string          `json:"algorithmVersion"`
	AcceptedModelVersions []string        `json:"acceptedModelVersions"`
	TargetLanguage        string          `json:"targetLanguage"`
	OutputKinds           []string        `json:"outputKinds"`
	ConfigurationSchema   json.RawMessage `json:"configurationSchema"`
	Runtime               string          `json:"runtime"`
	Entrypoint            string          `json:"entrypoint"`
}

// Plugin is a validated manifest and its contained entrypoint.
type Plugin struct {
	Manifest   Manifest
	entrypoint string
}

// Limits bounds every byte channel controlled by the caller or plugin.
type Limits struct {
	ModelBytes         int
	ConfigurationBytes int
	StdoutBytes        int
	StderrBytes        int
	ArtifactCount      int
	ArtifactBytes      int
	TotalArtifactBytes int
}

// DefaultLimits returns the sdk.generator-plugin-host-1 resource profile.
func DefaultLimits() Limits {
	return Limits{
		ModelBytes:         4 * 1024 * 1024,
		ConfigurationBytes: 256 * 1024,
		StdoutBytes:        8 * 1024 * 1024,
		StderrBytes:        64 * 1024,
		ArtifactCount:      1024,
		ArtifactBytes:      4 * 1024 * 1024,
		TotalArtifactBytes: 8 * 1024 * 1024,
	}
}

// Host resolves runtimes only from this explicit allowlist.
type Host struct {
	Runtimes map[string]string
	Limits   Limits
}

// Error contains stable, safe public diagnostics and never wraps an
// implementation error or plugin-controlled text.
type Error struct {
	diagnostics []generator.Diagnostic
}

func (e *Error) Error() string { return "generator plugin host failed" }

// Diagnostics returns a copy of the public diagnostics.
func (e *Error) Diagnostics() []generator.Diagnostic {
	return slices.Clone(e.diagnostics)
}

// Discover loads plugin manifests from the exact files or non-recursive
// directories supplied by the caller. It never searches PATH or the working
// directory.
func Discover(locations []string) ([]Plugin, error) {
	if len(locations) == 0 {
		return nil, hostError("GENERATOR_PLUGIN_NOT_FOUND", "/locations", "no plugin location was configured")
	}
	paths := make([]string, 0, len(locations))
	for _, location := range locations {
		discovered, err := manifestPaths(location)
		if err != nil {
			return nil, err
		}
		paths = append(paths, discovered...)
	}
	slices.Sort(paths)
	plugins := make([]Plugin, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		plugin, err := loadPlugin(path)
		if err != nil {
			return nil, err
		}
		if seen[plugin.Manifest.ID] {
			return nil, hostError("GENERATOR_PLUGIN_DUPLICATE", "/id", "plugin identifier is duplicated")
		}
		seen[plugin.Manifest.ID] = true
		plugins = append(plugins, plugin)
	}
	slices.SortFunc(plugins, func(left, right Plugin) int {
		return strings.Compare(left.Manifest.ID, right.Manifest.ID)
	})
	return plugins, nil
}

// Generate invokes one plugin, validates its bounded response, and commits
// validated artifacts through generator.WriteArtifacts.
func (h Host) Generate(ctx context.Context, plugin Plugin, model []byte, configuration json.RawMessage, outputRoot string) error {
	invocation, err := h.prepareInvocation(plugin, model, configuration)
	if err != nil {
		return err
	}
	output, err := invokePlugin(ctx, plugin.entrypoint, invocation)
	if err != nil {
		return err
	}
	response, err := decodeResponse(output)
	if err != nil {
		return err
	}
	if response.Status != "ok" {
		return hostError("GENERATOR_PLUGIN_REJECTED", "/plugin", "plugin rejected the generator model")
	}
	artifacts, err := validateArtifacts(response.Artifacts, invocation.limits)
	if err != nil {
		return err
	}
	return generator.WriteArtifacts(outputRoot, artifacts)
}

type invocation struct {
	runtimePath string
	request     []byte
	limits      Limits
}

func (h Host) prepareInvocation(plugin Plugin, model []byte, configuration json.RawMessage) (invocation, error) {
	limits, err := normalizedLimits(h.Limits)
	if err != nil {
		return invocation{}, err
	}
	if len(model) == 0 || len(model) > limits.ModelBytes {
		return invocation{}, hostError("GENERATOR_RESOURCE_LIMIT", "/model", "generator model exceeds the configured limit")
	}
	if len(configuration) == 0 {
		configuration = json.RawMessage(`{}`)
	}
	if len(configuration) > limits.ConfigurationBytes || !validJSONObject(configuration) {
		return invocation{}, hostError("GENERATOR_CONFIGURATION_INVALID", "/configuration", "plugin configuration is invalid")
	}
	if !slices.Contains(plugin.Manifest.AcceptedModelVersions, generator.ModelVersion) {
		return invocation{}, hostError("GENERATOR_VERSION_UNSUPPORTED", "/model/version", "plugin does not accept this model version")
	}
	if _, err := generator.Generate(model); err != nil {
		return invocation{}, hostError("GENERATOR_MODEL_INVALID", "/model", "generator model is invalid")
	}
	runtimePath, ok := h.Runtimes[plugin.Manifest.Runtime]
	if !ok || runtimePath == "" {
		return invocation{}, hostError("GENERATOR_RUNTIME_UNSUPPORTED", "/runtime", "plugin runtime is not configured")
	}
	runtimePath, err = validatedRuntime(runtimePath)
	if err != nil {
		return invocation{}, err
	}
	request, err := json.Marshal(pluginRequest{
		Profile:       HostProfile,
		Model:         json.RawMessage(model),
		Configuration: configuration,
	})
	if err != nil {
		return invocation{}, hostError("GENERATOR_MODEL_INVALID", "/model", "generator model is invalid")
	}
	return invocation{runtimePath: runtimePath, request: append(request, '\n'), limits: limits}, nil
}

func invokePlugin(ctx context.Context, entrypoint string, invocation invocation) ([]byte, error) {
	sandbox, err := os.MkdirTemp("", "naatre-generator-plugin-")
	if err != nil {
		return nil, hostError("GENERATOR_PLUGIN_EXECUTION_FAILED", "/plugin", "plugin sandbox could not be created")
	}
	defer func() { _ = os.RemoveAll(sandbox) }()
	command := exec.CommandContext(ctx, invocation.runtimePath, entrypoint)
	command.Dir = sandbox
	command.Env = []string{"LANG=C", "LC_ALL=C", "TZ=UTC", "SOURCE_DATE_EPOCH=0"}
	command.Stdin = bytes.NewReader(invocation.request)
	stdout := &limitWriter{maximum: invocation.limits.StdoutBytes}
	stderr := &limitWriter{maximum: invocation.limits.StderrBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	runErr := command.Run()
	if ctx.Err() != nil {
		return nil, hostError("GENERATOR_CANCELLED", "/plugin", "plugin execution was cancelled")
	}
	if stdout.exceeded || stderr.exceeded {
		return nil, hostError("GENERATOR_RESOURCE_LIMIT", "/plugin", "plugin output exceeds the configured limit")
	}
	if runErr != nil {
		return nil, hostError("GENERATOR_PLUGIN_EXECUTION_FAILED", "/plugin", "plugin execution failed")
	}
	return bytes.Clone(stdout.Bytes()), nil
}

type pluginRequest struct {
	Profile       string          `json:"profile"`
	Model         json.RawMessage `json:"model"`
	Configuration json.RawMessage `json:"configuration"`
}

type pluginResponse struct {
	Profile   string `json:"profile"`
	Status    string `json:"status"`
	Artifacts []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"artifacts"`
}

func manifestPaths(location string) ([]string, error) {
	info, err := os.Lstat(location)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return nil, hostError("GENERATOR_PLUGIN_NOT_FOUND", "/locations", "configured plugin location is unavailable")
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() || !strings.HasSuffix(info.Name(), ".naatre-generator.json") {
			return nil, hostError("GENERATOR_PLUGIN_MANIFEST_INVALID", "/locations", "configured plugin manifest is invalid")
		}
		return []string{location}, nil
	}
	entries, err := os.ReadDir(location)
	if err != nil {
		return nil, hostError("GENERATOR_PLUGIN_NOT_FOUND", "/locations", "configured plugin location is unavailable")
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".naatre-generator.json") {
			paths = append(paths, filepath.Join(location, entry.Name()))
		}
	}
	if len(paths) == 0 {
		return nil, hostError("GENERATOR_PLUGIN_NOT_FOUND", "/locations", "configured plugin location contains no manifests")
	}
	return paths, nil
}

func loadPlugin(path string) (Plugin, error) {
	content, err := readBounded(path, maxManifestSize)
	if err != nil {
		return Plugin{}, hostError("GENERATOR_PLUGIN_MANIFEST_INVALID", "/manifest", "plugin manifest is invalid")
	}
	var manifest Manifest
	if err := decodeStrict(content, &manifest); err != nil || !validManifest(manifest) {
		return Plugin{}, hostError("GENERATOR_PLUGIN_MANIFEST_INVALID", "/manifest", "plugin manifest is invalid")
	}
	entrypoint, err := containedRegularFile(filepath.Dir(path), manifest.Entrypoint)
	if err != nil {
		return Plugin{}, hostError("GENERATOR_PLUGIN_MANIFEST_INVALID", "/entrypoint", "plugin entrypoint is invalid")
	}
	return Plugin{Manifest: manifest, entrypoint: entrypoint}, nil
}

func validManifest(manifest Manifest) bool {
	if manifest.Version != ManifestVersion || !portableName.MatchString(manifest.ID) || !portableName.MatchString(manifest.AlgorithmVersion) || !portableName.MatchString(manifest.TargetLanguage) || !portableName.MatchString(manifest.Runtime) {
		return false
	}
	if len(manifest.AcceptedModelVersions) == 0 || len(manifest.OutputKinds) == 0 || !slices.Contains(manifest.AcceptedModelVersions, generator.ModelVersion) {
		return false
	}
	for _, value := range append(slices.Clone(manifest.AcceptedModelVersions), manifest.OutputKinds...) {
		if !portableName.MatchString(value) {
			return false
		}
	}
	return validJSONObject(manifest.ConfigurationSchema)
}

func containedRegularFile(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || filepath.Clean(relative) != relative || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("unsafe path")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, relative)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("entrypoint escaped manifest directory")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("entrypoint is not a regular file")
	}
	return resolved, nil
}

func validatedRuntime(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", hostError("GENERATOR_RUNTIME_UNSUPPORTED", "/runtime", "plugin runtime is unavailable")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", hostError("GENERATOR_RUNTIME_UNSUPPORTED", "/runtime", "plugin runtime is unavailable")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", hostError("GENERATOR_RUNTIME_UNSUPPORTED", "/runtime", "plugin runtime is unavailable")
	}
	return resolved, nil
}

func normalizedLimits(limits Limits) (Limits, error) {
	if limits == (Limits{}) {
		return DefaultLimits(), nil
	}
	if limits.ModelBytes <= 0 || limits.ConfigurationBytes <= 0 || limits.StdoutBytes <= 0 || limits.StderrBytes <= 0 || limits.ArtifactCount <= 0 || limits.ArtifactBytes <= 0 || limits.TotalArtifactBytes <= 0 {
		return Limits{}, hostError("GENERATOR_RESOURCE_LIMIT", "/limits", "plugin host limits are invalid")
	}
	return limits, nil
}

func validJSONObject(content []byte) bool {
	var value map[string]json.RawMessage
	return decodeStrict(content, &value) == nil && value != nil
}

func decodeResponse(content []byte) (pluginResponse, error) {
	var response pluginResponse
	if decodeStrict(content, &response) != nil || response.Profile != HostProfile || (response.Status != "ok" && response.Status != "failed") {
		return pluginResponse{}, hostError("GENERATOR_PLUGIN_RESPONSE_INVALID", "/plugin", "plugin response is invalid")
	}
	if response.Status == "ok" && len(response.Artifacts) == 0 {
		return pluginResponse{}, hostError("GENERATOR_PLUGIN_RESPONSE_INVALID", "/plugin", "plugin response is invalid")
	}
	if response.Status == "failed" && len(response.Artifacts) != 0 {
		return pluginResponse{}, hostError("GENERATOR_PLUGIN_RESPONSE_INVALID", "/plugin", "plugin response is invalid")
	}
	return response, nil
}

func validateArtifacts(values []struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}, limits Limits) ([]generator.Artifact, error) {
	if len(values) > limits.ArtifactCount {
		return nil, hostError("GENERATOR_RESOURCE_LIMIT", "/artifacts", "plugin artifacts exceed the configured limit")
	}
	total := 0
	artifacts := make([]generator.Artifact, 0, len(values))
	for index, value := range values {
		if len(value.Content) > limits.ArtifactBytes || total > limits.TotalArtifactBytes-len(value.Content) {
			return nil, hostError("GENERATOR_RESOURCE_LIMIT", fmt.Sprintf("/artifacts/%d", index), "plugin artifacts exceed the configured limit")
		}
		total += len(value.Content)
		artifacts = append(artifacts, generator.Artifact{Path: value.Path, Content: []byte(value.Content)})
	}
	return artifacts, nil
}

func decodeStrict(content []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing JSON value")
	}
	return nil
}

func readBounded(path string, maximum int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil || len(content) > maximum {
		return nil, errors.New("file exceeds limit")
	}
	return content, nil
}

func hostError(code, pointer, message string) error {
	return &Error{diagnostics: []generator.Diagnostic{{Code: code, Pointer: pointer, Message: message}}}
}

type limitWriter struct {
	buffer   bytes.Buffer
	maximum  int
	exceeded bool
}

func (w *limitWriter) Write(content []byte) (int, error) {
	remaining := w.maximum - w.buffer.Len()
	if len(content) > remaining {
		if remaining > 0 {
			_, _ = w.buffer.Write(content[:remaining])
		}
		w.exceeded = true
		return max(remaining, 0), errors.New("bounded plugin stream exceeded")
	}
	return w.buffer.Write(content)
}

func (w *limitWriter) Bytes() []byte { return w.buffer.Bytes() }

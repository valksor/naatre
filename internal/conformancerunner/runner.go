// Package conformancerunner implements the language-neutral runner protocol.
// It deliberately depends only on the standard library and checked-in JSON
// fixtures; profile implementations are registered explicitly by New.
package conformancerunner

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	Protocol       = "naatre.conformance.runner-1"
	ReportProtocol = "naatre.conformance.report-1"
	MaxInputBytes  = 1024 * 1024
)

var ErrRequiredProfileFailed = errors.New("one or more required conformance profiles did not pass")

type Endpoint struct {
	Kind     string `json:"kind"`
	Language string `json:"language"`
	Profile  string `json:"profile,omitempty"`
}

type Path struct {
	Source      Endpoint `json:"source"`
	Destination Endpoint `json:"destination"`
}

type Request struct {
	Protocol string   `json:"protocol"`
	ID       string   `json:"id"`
	Command  string   `json:"command"`
	Path     *Path    `json:"path,omitempty"`
	Profiles []string `json:"profiles,omitempty"`
}

type Diagnostic struct {
	Phase   string         `json:"phase"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Source  map[string]any `json:"source"`
	Path    []any          `json:"path"`
}

type Evidence struct {
	Fixture string `json:"fixture"`
	SHA256  string `json:"sha256"`
}

type Result struct {
	Profile        string         `json:"profile"`
	Status         string         `json:"status"`
	Capabilities   []string       `json:"capabilities"`
	Phase          string         `json:"phase"`
	Code           string         `json:"code"`
	Source         map[string]any `json:"source"`
	Path           []any          `json:"path"`
	Data           any            `json:"data"`
	Errors         []any          `json:"errors"`
	CanonicalBytes string         `json:"canonicalBytes"`
	Diagnostics    []Diagnostic   `json:"diagnostics"`
	Evidence       []Evidence     `json:"evidence"`
	Reason         string         `json:"reason,omitempty"`
}

type Profile struct {
	Profile string `json:"profile"`
	Status  string `json:"status"`
}

type Identity struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Language string `json:"language"`
	Platform string `json:"platform"`
}

type Response struct {
	Protocol       string    `json:"protocol"`
	ID             string    `json:"id"`
	FixtureVersion string    `json:"fixtureVersion"`
	Runner         Identity  `json:"runner"`
	Path           *Path     `json:"path,omitempty"`
	Profiles       []Profile `json:"profiles"`
	Results        []Result  `json:"results"`
}

type fixtureFile struct {
	Path    string `json:"path"`
	Profile string `json:"profile"`
	SHA256  string `json:"sha256"`
}

type manifest struct {
	FixtureVersion         string                 `json:"fixtureVersion"`
	SpecVersion            string                 `json:"specVersion"`
	RunnerProtocol         string                 `json:"runnerProtocol"`
	ProfileRegistryVersion string                 `json:"profileRegistryVersion"`
	ReportProtocol         string                 `json:"reportProtocol"`
	Files                  []fixtureFile          `json:"files"`
	NormativeSources       []suiteNormativeSource `json:"normativeSources"`
	Languages              []string               `json:"languages"`
}

type Handler func(context.Context, Request) Result

func loadProfileFixture[T any](runner *Runner, path string, valid func(T) bool) (T, Evidence, error) {
	var fixture T
	_, evidence, err := runner.loadPinnedFixture(path, &fixture)
	if err != nil {
		return *new(T), Evidence{}, err
	}
	if !valid(fixture) {
		return *new(T), Evidence{}, errors.New("incompatible profile fixture")
	}
	return fixture, evidence, nil
}

type Runner struct {
	manifest        manifest
	conformanceRoot string
	handlers        map[string]Handler
}

func New(suitePath string) (*Runner, error) {
	content, err := os.ReadFile(suitePath)
	if err != nil {
		return nil, fmt.Errorf("read suite manifest: %w", err)
	}
	var suite manifest
	if err := json.Unmarshal(content, &suite); err != nil {
		return nil, fmt.Errorf("decode suite manifest: %w", err)
	}
	if suite.FixtureVersion == "" || suite.SpecVersion == "" || suite.RunnerProtocol != Protocol ||
		suite.ProfileRegistryVersion == "" || suite.ReportProtocol != ReportProtocol || len(suite.Files) == 0 {
		return nil, errors.New("suite manifest has incompatible metadata")
	}
	root := filepath.Dir(filepath.Dir(suitePath))
	runner := &Runner{manifest: suite, conformanceRoot: root, handlers: make(map[string]Handler)}
	runner.handlers["suite.contract-1"] = runner.verifySuite
	runner.handlers["suite.profiles-1"] = runner.verifyProfileRegistry
	runner.handlers[coreHTTPProfile] = runner.verifyGoHTTP
	runner.handlers[goClientProfile] = runner.verifyGoClient
	runner.handlers[goSDKProfile] = runner.verifyGoSDK
	runner.handlers[toolingProfile] = runner.verifyTooling
	runner.handlers[planCacheProfile] = runner.verifyPlanCache
	runner.handlers[generatorPluginHostProfile] = runner.verifyGeneratorPluginHost
	runner.handlers[goQualityProfile] = runner.verifyGoQuality
	runner.handlers[operationsProfile] = runner.verifyOperations
	return runner, nil
}

func (r *Runner) Register(profile string, handler Handler) error {
	if !validBoundedString(profile, 128) || handler == nil {
		return errors.New("profile and handler are required")
	}
	if _, exists := r.handlers[profile]; exists {
		return fmt.Errorf("profile %q is already registered", profile)
	}
	r.handlers[profile] = handler
	return nil
}

func (r *Runner) Handle(ctx context.Context, request Request) Response {
	if err := validateRequest(request); err != nil {
		return r.errorResponse(request.ID, err)
	}
	response := r.response(request.ID, request.Path)
	if request.Command == "discover" {
		return response
	}
	response.Results = make([]Result, 0, len(request.Profiles))
	for _, profile := range request.Profiles {
		handler := r.handlers[profile]
		if handler == nil {
			response.Results = append(response.Results, emptyResult(profile, "unsupported", "profile is not implemented by this runner"))
			continue
		}
		result := invokeHandler(ctx, handler, request)
		result.Profile = profile
		normalizeResult(&result)
		if !validResult(result) {
			result = failureResult(profile, "INVALID_RUNNER_RESULT", "")
			result.Status = "infrastructure-failure"
		}
		response.Results = append(response.Results, result)
	}
	return response
}

func (r *Runner) ServeNDJSON(ctx context.Context, input io.Reader, output io.Writer) error {
	return r.ServeNDJSONWithOptions(ctx, input, output, NDJSONOptions{})
}

type NDJSONOptions struct {
	RequirePass bool
}

func (r *Runner) ServeNDJSONWithOptions(ctx context.Context, input io.Reader, output io.Writer, options NDJSONOptions) error {
	reader := bufio.NewReader(input)
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	requirementFailed := false
	line := make([]byte, 0, 64*1024)
	tooLarge := false
	for {
		fragment, continued, readErr := reader.ReadLine()
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return fmt.Errorf("read runner request: %w", readErr)
		}
		if !tooLarge {
			if len(line)+len(fragment) > MaxInputBytes {
				tooLarge = true
				line = line[:0]
			} else {
				line = append(line, fragment...)
			}
		}
		if continued {
			continue
		}
		if tooLarge {
			response := r.errorResponse("invalid", &protocolError{code: "RUNNER_REQUEST_TOO_LARGE", message: "runner request exceeds one MiB"})
			if err := encoder.Encode(response); err != nil {
				return fmt.Errorf("encode runner response: %w", err)
			}
			requirementFailed = requirementFailed || options.RequirePass
			tooLarge = false
			continue
		}
		if len(line) == 0 {
			continue
		}
		request, err := decodeRequest(line)
		response := r.Handle(ctx, request)
		if err != nil {
			response = r.errorResponse(request.ID, err)
		}
		if err := encoder.Encode(response); err != nil {
			return fmt.Errorf("encode runner response: %w", err)
		}
		if options.RequirePass && !responsePassed(response) {
			requirementFailed = true
		}
		line = line[:0]
	}
	if requirementFailed {
		return ErrRequiredProfileFailed
	}
	return nil
}

func (r *Runner) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/conformance/profiles", func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, r.Handle(request.Context(), Request{Protocol: Protocol, ID: "http-discover", Command: "discover"}))
	})
	mux.HandleFunc("POST /v1/conformance/run", func(writer http.ResponseWriter, request *http.Request) {
		mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if err != nil || !strings.EqualFold(mediaType, "application/json") || !validJSONMediaParameters(parameters) {
			writeJSON(writer, http.StatusUnsupportedMediaType, map[string]string{"code": "UNSUPPORTED_MEDIA_TYPE"})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, MaxInputBytes))
		if err != nil {
			writeJSON(writer, http.StatusRequestEntityTooLarge, map[string]string{"code": "RUNNER_REQUEST_TOO_LARGE"})
			return
		}
		decoded, err := decodeRequest(body)
		if err == nil {
			err = validateRequest(decoded)
		}
		if err == nil && decoded.Command != "run" {
			err = &protocolError{code: "INVALID_RUNNER_REQUEST", message: "HTTP run endpoint requires the run command"}
		}
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, r.errorResponse(decoded.ID, err))
			return
		}
		writeJSON(writer, http.StatusOK, r.Handle(request.Context(), decoded))
	})
	return mux
}

func (r *Runner) verifySuite(_ context.Context, _ Request) Result {
	result := emptyResult("suite.contract-1", "passed", "")
	result.Capabilities = []string{"suite.contract-1"}
	for _, file := range r.manifest.Files {
		path, err := r.fixturePath(file.Path)
		if err != nil {
			return failureResult("suite.contract-1", "FIXTURE_PATH_INVALID", file.Path)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return failureResult("suite.contract-1", "FIXTURE_UNAVAILABLE", file.Path)
		}
		digest := sha256.Sum256(content)
		actual := hex.EncodeToString(digest[:])
		result.Evidence = append(result.Evidence, Evidence{Fixture: file.Path, SHA256: actual})
		if actual != file.SHA256 {
			return failureResult("suite.contract-1", "FIXTURE_DIGEST_MISMATCH", file.Path)
		}
	}
	return result
}

func (r *Runner) fixturePath(relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return "", errors.New("fixture path must be relative")
	}
	root, err := filepath.Abs(r.conformanceRoot)
	if err != nil {
		return "", err
	}
	candidate, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		return "", err
	}
	if candidate == root || !strings.HasPrefix(candidate, root+string(filepath.Separator)) {
		return "", errors.New("fixture path escapes suite root")
	}
	return candidate, nil
}

func (r *Runner) response(id string, path *Path) Response {
	return Response{
		Protocol: Protocol, ID: id, FixtureVersion: r.manifest.FixtureVersion, Path: path,
		Runner:   Identity{Name: "naatre-go", Version: "1.0.0", Language: "go", Platform: runtime.GOOS + "-" + runtime.GOARCH + "-go-" + runtime.Version()},
		Profiles: r.profileInventory(), Results: []Result{},
	}
}

func (r *Runner) profileInventory() []Profile {
	profiles := make(map[string]string)
	for _, file := range r.manifest.Files {
		profiles[file.Profile] = "unsupported"
	}
	for profile := range r.handlers {
		profiles[profile] = "supported"
	}
	names := make([]string, 0, len(profiles))
	for profile := range profiles {
		names = append(names, profile)
	}
	slices.Sort(names)
	result := make([]Profile, 0, len(names))
	for _, profile := range names {
		result = append(result, Profile{Profile: profile, Status: profiles[profile]})
	}
	return result
}

func (r *Runner) errorResponse(id string, err error) Response {
	if !validBoundedString(id, 128) {
		id = "invalid"
	}
	code := "RUNNER_INTERNAL"
	message := "runner failed"
	var protocolFailure *protocolError
	if errors.As(err, &protocolFailure) {
		code = protocolFailure.code
		message = protocolFailure.message
	}
	result := failureResult("runner", code, "")
	if protocolFailure == nil {
		result.Status = "infrastructure-failure"
	}
	result.Diagnostics[0].Message = message
	response := r.response(id, nil)
	response.Results = []Result{result}
	return response
}

type protocolError struct {
	code    string
	message string
}

func (e *protocolError) Error() string { return e.message }

func decodeRequest(input []byte) (Request, error) {
	if len(input) > MaxInputBytes {
		return Request{}, &protocolError{code: "RUNNER_REQUEST_TOO_LARGE", message: "runner request exceeds one MiB"}
	}
	if !utf8.Valid(input) {
		return Request{}, &protocolError{code: "MALFORMED_RUNNER_JSON", message: "runner request is not valid UTF-8"}
	}
	if !validJSONSurrogates(input) {
		return Request{}, &protocolError{code: "MALFORMED_RUNNER_JSON", message: "runner request contains an unpaired Unicode surrogate"}
	}
	if !json.Valid(input) {
		return Request{}, &protocolError{code: "MALFORMED_RUNNER_JSON", message: "runner request is not valid JSON"}
	}
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, &protocolError{code: "INVALID_RUNNER_REQUEST", message: "runner request does not match the protocol schema"}
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return request, &protocolError{code: "MALFORMED_RUNNER_JSON", message: "runner request contains trailing data"}
	}
	if err := validateEncodedOptionals(input); err != nil {
		return request, err
	}
	return request, nil
}

func validateEncodedOptionals(input []byte) error {
	var raw struct {
		Path     json.RawMessage `json:"path"`
		Profiles json.RawMessage `json:"profiles"`
	}
	if err := json.Unmarshal(input, &raw); err != nil {
		return &protocolError{code: "INVALID_RUNNER_REQUEST", message: "runner request does not match the protocol schema"}
	}
	if raw.Path != nil && bytes.Equal(bytes.TrimSpace(raw.Path), []byte("null")) {
		return &protocolError{code: "INVALID_RUNNER_REQUEST", message: "path must be an object"}
	}
	if raw.Profiles != nil && bytes.Equal(bytes.TrimSpace(raw.Profiles), []byte("null")) {
		return &protocolError{code: "INVALID_RUNNER_REQUEST", message: "profiles must be an array"}
	}
	if raw.Path == nil {
		return nil
	}
	var path struct {
		Source      map[string]json.RawMessage `json:"source"`
		Destination map[string]json.RawMessage `json:"destination"`
	}
	if err := json.Unmarshal(raw.Path, &path); err != nil {
		return &protocolError{code: "INVALID_RUNNER_REQUEST", message: "path must be an object"}
	}
	for _, endpoint := range []map[string]json.RawMessage{path.Source, path.Destination} {
		profile, present := endpoint["profile"]
		if !present {
			continue
		}
		var value string
		if err := json.Unmarshal(profile, &value); err != nil || !validBoundedString(value, 128) {
			return &protocolError{code: "INVALID_RUNNER_PATH", message: "runner path endpoint profile is invalid"}
		}
	}
	return nil
}

func responsePassed(response Response) bool {
	for _, result := range response.Results {
		if result.Status != "passed" {
			return false
		}
	}
	return true
}

func validateRequest(request Request) error {
	if request.Protocol != Protocol {
		return &protocolError{code: "UNSUPPORTED_RUNNER_PROTOCOL", message: "unsupported runner protocol"}
	}
	if !validBoundedString(request.ID, 128) {
		return &protocolError{code: "INVALID_RUNNER_REQUEST", message: "id must be a bounded non-empty string"}
	}
	if request.Command != "discover" && request.Command != "run" {
		return &protocolError{code: "INVALID_RUNNER_REQUEST", message: "command must be discover or run"}
	}
	if request.Command == "run" && (request.Path == nil || len(request.Profiles) == 0) {
		return &protocolError{code: "INVALID_RUNNER_REQUEST", message: "run requires a path and profiles"}
	}
	if request.Path != nil {
		if err := validateEndpoint(request.Path.Source); err != nil {
			return err
		}
		if err := validateEndpoint(request.Path.Destination); err != nil {
			return err
		}
	}
	if request.Profiles != nil {
		if len(request.Profiles) == 0 || len(request.Profiles) > 128 {
			return &protocolError{code: "INVALID_RUNNER_REQUEST", message: "run requires a path and profiles"}
		}
		seen := make(map[string]bool, len(request.Profiles))
		for _, profile := range request.Profiles {
			if !validBoundedString(profile, 128) || seen[profile] {
				return &protocolError{code: "INVALID_RUNNER_REQUEST", message: "profiles must be bounded and unique"}
			}
			seen[profile] = true
		}
	}
	return nil
}

func validateEndpoint(endpoint Endpoint) error {
	if !slices.Contains([]string{"sdk", "codec", "gateway", "worker", "http-server", "native-runtime"}, endpoint.Kind) || !validBoundedString(endpoint.Language, 64) || utf8.RuneCountInString(endpoint.Profile) > 128 {
		return &protocolError{code: "INVALID_RUNNER_PATH", message: "runner path endpoint is invalid"}
	}
	return nil
}

func validBoundedString(value string, maximum int) bool {
	length := utf8.RuneCountInString(value)
	return utf8.ValidString(value) && length > 0 && length <= maximum
}

func validJSONSurrogates(input []byte) bool {
	for index := 0; index < len(input); index++ {
		if input[index] != '"' {
			continue
		}
		for index++; index < len(input) && input[index] != '"'; index++ {
			if input[index] != '\\' {
				continue
			}
			index++
			if index >= len(input) {
				return false
			}
			if input[index] != 'u' {
				continue
			}
			value, ok := decodeHexQuad(input, index+1)
			if !ok {
				return false
			}
			index += 4
			if value >= 0xdc00 && value <= 0xdfff {
				return false
			}
			if value < 0xd800 || value > 0xdbff {
				continue
			}
			if index+6 >= len(input) || input[index+1] != '\\' || input[index+2] != 'u' {
				return false
			}
			low, ok := decodeHexQuad(input, index+3)
			if !ok || low < 0xdc00 || low > 0xdfff {
				return false
			}
			index += 6
		}
	}
	return true
}

func decodeHexQuad(input []byte, start int) (uint16, bool) {
	if start+4 > len(input) {
		return 0, false
	}
	var value uint16
	for _, digit := range input[start : start+4] {
		value *= 16
		switch {
		case digit >= '0' && digit <= '9':
			value += uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value += uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			value += uint16(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func validJSONMediaParameters(parameters map[string]string) bool {
	for name, value := range parameters {
		if !strings.EqualFold(name, "charset") || !strings.EqualFold(value, "utf-8") {
			return false
		}
	}
	return true
}

func validResult(result Result) bool {
	if !validBoundedString(result.Profile, 128) || !slices.Contains([]string{"passed", "failed", "unsupported", "invalid-skip", "infrastructure-failure"}, result.Status) || !utf8.ValidString(result.Reason) || utf8.RuneCountInString(result.Reason) > 1024 || !utf8.ValidString(result.CanonicalBytes) {
		return false
	}
	if _, err := json.Marshal(result); err != nil || !allStringsValid(reflect.ValueOf(result), make(map[visit]bool)) {
		return false
	}
	if result.Phase != "" && !slices.Contains([]string{"runner", "transport", "decode", "validate", "plan", "authorize", "execute", "complete", "serialize", "canonicalize"}, result.Phase) {
		return false
	}
	if result.Code != "" && !validCode(result.Code) {
		return false
	}
	seenCapabilities := make(map[string]bool, len(result.Capabilities))
	for _, capability := range result.Capabilities {
		if !validBoundedString(capability, 128) || seenCapabilities[capability] {
			return false
		}
		seenCapabilities[capability] = true
	}
	if !validSource(result.Source) || !validResponsePath(result.Path, 128) {
		return false
	}
	for _, diagnostic := range result.Diagnostics {
		if !validDiagnostic(diagnostic) {
			return false
		}
	}
	for _, evidence := range result.Evidence {
		if !validBoundedString(evidence.Fixture, 1024) || len(evidence.SHA256) != 64 {
			return false
		}
		for _, digit := range evidence.SHA256 {
			if (digit < '0' || digit > '9') && (digit < 'a' || digit > 'f') {
				return false
			}
		}
	}
	return true
}

func validDiagnostic(diagnostic Diagnostic) bool {
	if !slices.Contains([]string{"runner", "transport", "decode", "validate", "plan", "authorize", "execute", "complete", "serialize", "canonicalize"}, diagnostic.Phase) || !validCode(diagnostic.Code) || !utf8.ValidString(diagnostic.Message) || utf8.RuneCountInString(diagnostic.Message) > 1024 || !validSource(diagnostic.Source) || !validResponsePath(diagnostic.Path, 128) {
		return false
	}
	return true
}

func validSource(source map[string]any) bool {
	for name, value := range source {
		switch name {
		case "pointer":
			text, ok := value.(string)
			if !ok || utf8.RuneCountInString(text) > 1024 {
				return false
			}
		case "offset":
			if !nonNegativeInteger(value, true) {
				return false
			}
		case "line", "column":
			if !nonNegativeInteger(value, false) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validCode(code string) bool {
	if len(code) == 0 || len(code) > 128 || code[0] < 'A' || code[0] > 'Z' {
		return false
	}
	for _, character := range code[1:] {
		if (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func validResponsePath(path []any, maximum int) bool {
	if maximum > 0 && len(path) > maximum {
		return false
	}
	for _, segment := range path {
		switch value := segment.(type) {
		case string:
			if !utf8.ValidString(value) {
				return false
			}
		case int:
			if value < 0 || uint64(value) > 9007199254740991 {
				return false
			}
		case int8:
			if value < 0 {
				return false
			}
		case int16:
			if value < 0 {
				return false
			}
		case int32:
			if value < 0 {
				return false
			}
		case int64:
			if value < 0 || value > 9007199254740991 {
				return false
			}
		case uint:
			if uint64(value) > 9007199254740991 {
				return false
			}
		case uint8, uint16, uint32:
		case uint64:
			if value > 9007199254740991 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func nonNegativeInteger(value any, allowZero bool) bool {
	minimum := int64(1)
	if allowZero {
		minimum = 0
	}
	switch number := value.(type) {
	case int:
		return int64(number) >= minimum && uint64(number) <= 9007199254740991
	case int64:
		return number >= minimum && number <= 9007199254740991
	case float64:
		return number >= float64(minimum) && number <= 9007199254740991 && number == float64(int64(number))
	default:
		return false
	}
}

type visit struct {
	typeOf reflect.Type
	ptr    uintptr
}

func allStringsValid(value reflect.Value, seen map[visit]bool) bool {
	switch value.Kind() {
	case reflect.Invalid:
		return true
	case reflect.Interface, reflect.Pointer:
		if value.IsNil() {
			return true
		}
		if value.Kind() == reflect.Pointer {
			key := visit{typeOf: value.Type(), ptr: value.Pointer()}
			if seen[key] {
				return true
			}
			seen[key] = true
		}
		return allStringsValid(value.Elem(), seen)
	case reflect.String:
		return utf8.ValidString(value.String())
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			if value.Type().Field(index).IsExported() && !allStringsValid(value.Field(index), seen) {
				return false
			}
		}
	case reflect.Map:
		if value.IsNil() {
			return true
		}
		key := visit{typeOf: value.Type(), ptr: value.Pointer()}
		if seen[key] {
			return true
		}
		seen[key] = true
		iterator := value.MapRange()
		for iterator.Next() {
			if !allStringsValid(iterator.Key(), seen) || !allStringsValid(iterator.Value(), seen) {
				return false
			}
		}
	case reflect.Slice:
		if value.IsNil() {
			return true
		}
		key := visit{typeOf: value.Type(), ptr: value.Pointer()}
		if seen[key] {
			return true
		}
		seen[key] = true
		fallthrough
	case reflect.Array:
		for index := 0; index < value.Len(); index++ {
			if !allStringsValid(value.Index(index), seen) {
				return false
			}
		}
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return true
	case reflect.Complex64, reflect.Complex128, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return false
	}
	return true
}

func emptyResult(profile, status, reason string) Result {
	return Result{Profile: profile, Status: status, Capabilities: []string{}, Source: map[string]any{}, Path: []any{}, Errors: []any{}, Diagnostics: []Diagnostic{}, Evidence: []Evidence{}, Reason: reason}
}

func failureResult(profile, code, fixture string) Result {
	result := emptyResult(profile, "failed", "")
	result.Phase = "runner"
	result.Code = code
	result.Diagnostics = []Diagnostic{{Phase: "runner", Code: code, Message: "conformance profile failed", Source: map[string]any{}, Path: []any{}}}
	if fixture != "" {
		result.Diagnostics[0].Message += ": " + fixture
	}
	return result
}

func normalizeResult(result *Result) {
	if result.Status != "passed" && result.Status != "failed" && result.Status != "unsupported" && result.Status != "invalid-skip" && result.Status != "infrastructure-failure" {
		*result = failureResult(result.Profile, "INVALID_RUNNER_RESULT", "")
		result.Status = "infrastructure-failure"
	}
	if result.Capabilities == nil {
		result.Capabilities = []string{}
	}
	if result.Source == nil {
		result.Source = map[string]any{}
	}
	if result.Path == nil {
		result.Path = []any{}
	}
	if result.Errors == nil {
		result.Errors = []any{}
	}
	if result.Diagnostics == nil {
		result.Diagnostics = []Diagnostic{}
	}
	if result.Evidence == nil {
		result.Evidence = []Evidence{}
	}
}

func invokeHandler(ctx context.Context, handler Handler, request Request) (result Result) {
	defer func() {
		if recover() != nil {
			result = failureResult("", "PROFILE_PANIC", "")
			result.Status = "infrastructure-failure"
		}
	}()
	return handler(ctx, request)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

package conformance_test

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
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/internal/conformancerunner"
	"github.com/valksor/naatre/protocol"
	naatreruntime "github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

type suiteManifest struct {
	FixtureVersion   string            `json:"fixtureVersion"`
	SpecVersion      string            `json:"specVersion"`
	RunnerProtocol   string            `json:"runnerProtocol"`
	DigestAlgorithm  string            `json:"digestAlgorithm"`
	Ownership        map[string]int    `json:"ownership"`
	Files            []suiteFile       `json:"files"`
	NormativeSources []normativeSource `json:"normativeSources"`
	IssueMappings    []issueMapping    `json:"issueMappings"`
	Languages        []string          `json:"languages"`
	ReportFields     []string          `json:"reportFields"`
	Categories       []string          `json:"categories"`
}

type suiteFile struct {
	Path     string `json:"path"`
	Profile  string `json:"profile"`
	Category string `json:"category"`
	SHA256   string `json:"sha256"`
}

type normativeSource struct {
	CaseID      string       `json:"caseId"`
	Spec        string       `json:"spec"`
	Clauses     []string     `json:"clauses"`
	FixtureRefs []fixtureRef `json:"fixtureRefs"`
}

type fixtureRef struct {
	Path     string `json:"path"`
	Pointer  string `json:"pointer"`
	Polarity string `json:"polarity"`
}

type issueMapping struct {
	CaseID      string       `json:"caseId"`
	Issue       int          `json:"issue"`
	FixtureRefs []fixtureRef `json:"fixtureRefs"`
}

var clauseDefinition = regexp.MustCompile(`(?m)(?:^- \*\*|^#{2,3} |^\| )([A-Z][A-Z0-9]*-[0-9]{3})(?:\b|[: ])`)
var mustKeyword = regexp.MustCompile(`\bMUST(?: NOT)?\b`)

func TestConformanceSuiteContract(t *testing.T) {
	t.Parallel()
	var manifest suiteManifest
	readFixture(t, "suite.json", &manifest)
	if !regexp.MustCompile(`^[1-9][0-9]*\.[0-9]+\.[0-9]+$`).MatchString(manifest.FixtureVersion) {
		t.Fatalf("fixtureVersion = %q, want an independent semantic version", manifest.FixtureVersion)
	}
	if manifest.SpecVersion != "1" || manifest.RunnerProtocol != "naatre.conformance.runner-1" {
		t.Fatalf("suite versions = spec %q, runner %q", manifest.SpecVersion, manifest.RunnerProtocol)
	}

	requiredCategories := []string{"wire", "document", "schema", "execution", "transport", "canonical", "stream", "security", "evolution", "interaction", "adversarial", "benchmark"}
	for _, category := range requiredCategories {
		if !slices.Contains(manifest.Categories, category) {
			t.Errorf("suite does not declare %q fixture category", category)
		}
	}
	for _, field := range []string{"phase", "code", "source", "path", "data", "errors", "canonicalBytes", "capabilities"} {
		if !slices.Contains(manifest.ReportFields, field) {
			t.Errorf("suite report fields omit %q", field)
		}
	}
	for _, language := range []string{"go", "javascript-typescript", "php", "python", "rust", "jvm", "dotnet", "swift", "dart", "ruby"} {
		if !slices.Contains(manifest.Languages, language) {
			t.Errorf("suite language vectors omit %q", language)
		}
	}

	assertFixtureInventory(t, manifest)
	assertNormativeCoverage(t, manifest)
	assertIssueCoverage(t, manifest)
}

func TestInteractionAdversarialAndBenchmarkContracts(t *testing.T) {
	t.Parallel()
	assertCaseCatalog(t, "interactions.json", []string{
		"authorization-plus-cache", "conditionals-plus-fragments", "sequential-writes-plus-loaders",
		"partial-data-plus-generated-types", "remote-workers-plus-cancellation",
		"result-truncation-after-commit", "stale-fencing-owner", "supervised-uncooperative-process",
	})
	assertInteractionScenarios(t)
	assertCaseCatalog(t, "adversarial.json", []string{
		"depth-exhaustion", "alias-collision", "cost-overflow", "directive-abuse", "compression-bomb",
		"federation-cycle", "malformed-stream-frame", "ambiguous-literal-expression", "unicode-sort-difference",
		"serial-loader-deadlock",
	})
	assertAdversarialFixtureRefs(t)

	var benchmarks struct {
		Profile     string   `json:"profile"`
		Description string   `json:"description"`
		Environment []string `json:"environment"`
		Metrics     []string `json:"metrics"`
		Rules       any      `json:"rules"`
		Workloads   []struct {
			Name            string         `json:"name"`
			Category        string         `json:"category"`
			Fixture         string         `json:"fixture"`
			Cache           string         `json:"cache"`
			Scale           map[string]int `json:"scale"`
			RequiredMetrics map[string]int `json:"requiredMetrics"`
		} `json:"workloads"`
	}
	readFixture(t, "benchmarks.json", &benchmarks)
	for _, field := range []string{"os", "architecture", "cpu", "goVersion", "fixtureVersion", "revision"} {
		if !slices.Contains(benchmarks.Environment, field) {
			t.Errorf("benchmark environment omits %q", field)
		}
	}
	for _, metric := range []string{"ns/op", "B/op", "allocs/op", "p50", "p95", "p99", "upstreamCalls"} {
		if !slices.Contains(benchmarks.Metrics, metric) {
			t.Errorf("benchmark metrics omit %q", metric)
		}
	}
	required := map[string]bool{"parsing": false, "planning": false, "execution": false, "collection-expansion": false, "batching": false, "canonicalization": false, "streaming": false}
	for _, workload := range benchmarks.Workloads {
		if workload.Name == "" || workload.Fixture == "" {
			t.Error("benchmark workload requires a stable name and fixture")
		}
		if _, ok := required[workload.Category]; ok {
			required[workload.Category] = true
		}
	}
	for category, present := range required {
		if !present {
			t.Errorf("benchmark workloads omit %q", category)
		}
	}
}

func TestIndependentRunnerProtocol(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("../../conformance/runner-protocol.schema.json"); err != nil {
		t.Fatalf("runner protocol schema: %v", err)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("the independently pinned JavaScript runner requires Node: %v", err)
	}
	request := strings.Join([]string{
		`{"protocol":"naatre.conformance.runner-1","id":"discover","command":"discover"}`,
		`{"protocol":"naatre.conformance.runner-1","id":"run","command":"run","path":{"source":{"kind":"sdk","language":"javascript-typescript"},"destination":{"kind":"native-runtime","language":"javascript-typescript"}},"profiles":["suite.contract-1","suite.supervision-1","core.scalar.c14n-1","core.interop.c14n-1","core.http-1"]}`,
		`{"protocol":`,
		`{"protocol":"naatre.conformance.runner-1","id":"unknown","command":"discover","extra":true}`,
	}, "\n") + "\n"
	command := exec.Command("node", "../../conformance/independent/runner.mjs")
	command.Stdin = strings.NewReader(request)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("independent runner: %v\n%s", err, output)
	}

	scanner := bufio.NewScanner(bytes.NewReader(output))
	var responses []struct {
		Protocol       string `json:"protocol"`
		ID             string `json:"id"`
		FixtureVersion string `json:"fixtureVersion"`
		Results        []struct {
			Profile  string                       `json:"profile"`
			Status   string                       `json:"status"`
			Code     string                       `json:"code"`
			Evidence []conformancerunner.Evidence `json:"evidence"`
		} `json:"results"`
		Profiles []struct {
			Profile string `json:"profile"`
			Status  string `json:"status"`
		} `json:"profiles"`
	}
	for scanner.Scan() {
		var response struct {
			Protocol       string `json:"protocol"`
			ID             string `json:"id"`
			FixtureVersion string `json:"fixtureVersion"`
			Results        []struct {
				Profile  string                       `json:"profile"`
				Status   string                       `json:"status"`
				Code     string                       `json:"code"`
				Evidence []conformancerunner.Evidence `json:"evidence"`
			} `json:"results"`
			Profiles []struct {
				Profile string `json:"profile"`
				Status  string `json:"status"`
			} `json:"profiles"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			t.Fatalf("runner emitted non-protocol output %q: %v", scanner.Text(), err)
		}
		responses = append(responses, response)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(responses) != 4 || responses[0].ID != "discover" || responses[1].ID != "run" {
		t.Fatalf("runner responses = %#v", responses)
	}
	if responses[0].Protocol != "naatre.conformance.runner-1" || responses[0].FixtureVersion == "" {
		t.Fatal("discovery response does not bind protocol and fixture version")
	}
	statuses := make(map[string]string)
	var supervisionEvidence []conformancerunner.Evidence
	for _, result := range responses[1].Results {
		statuses[result.Profile] = result.Status
		if result.Profile == "suite.supervision-1" {
			supervisionEvidence = result.Evidence
		}
	}
	if statuses["suite.contract-1"] != "passed" || statuses["suite.supervision-1"] != "passed" || statuses["core.scalar.c14n-1"] != "passed" || statuses["core.interop.c14n-1"] != "passed" || statuses["core.http-1"] != "unsupported" {
		t.Fatalf("runner statuses = %#v", statuses)
	}
	var manifest suiteManifest
	readFixture(t, "suite.json", &manifest)
	var interactionsDigest string
	for _, file := range manifest.Files {
		if file.Path == "v1/interactions.json" {
			interactionsDigest = file.SHA256
			break
		}
	}
	if len(supervisionEvidence) != 1 || supervisionEvidence[0].Fixture != "v1/interactions.json" || supervisionEvidence[0].SHA256 != interactionsDigest {
		t.Fatalf("supervision evidence = %#v, want interactions digest %q", supervisionEvidence, interactionsDigest)
	}
	if responses[2].Results[0].Code != "MALFORMED_RUNNER_JSON" || responses[3].ID != "unknown" || responses[3].Results[0].Code != "INVALID_RUNNER_REQUEST" {
		t.Fatalf("runner error parity = %#v %#v", responses[2], responses[3])
	}
	assertProfileInventory(t, responses[0].Profiles)
	assertProfileSchema(t)

	after := []byte(`{"protocol":"naatre.conformance.runner-1","id":"after","command":"discover"}` + "\n")
	oversized := append(bytes.Repeat([]byte("x"), conformancerunner.MaxInputBytes+1), '\n')
	oversized = append(oversized, after...)
	command = exec.Command("node", "../../conformance/independent/runner.mjs")
	command.Stdin = bytes.NewReader(oversized)
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatalf("independent oversized-line recovery: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte(`"code":"RUNNER_REQUEST_TOO_LARGE"`)) || !bytes.Contains(output, []byte(`"id":"after"`)) {
		t.Fatalf("independent runner did not recover after oversized line: %s", output)
	}

	invalidUTF8 := append([]byte(`{"protocol":"naatre.conformance.runner-1","id":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`","command":"discover"}`+"\n")...)
	invalidUTF8 = append(invalidUTF8, after...)
	command = exec.Command("node", "../../conformance/independent/runner.mjs")
	command.Stdin = bytes.NewReader(invalidUTF8)
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatalf("independent invalid-UTF-8 recovery: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte(`"code":"MALFORMED_RUNNER_JSON"`)) || !bytes.Contains(output, []byte(`"id":"after"`)) {
		t.Fatalf("independent runner did not reject invalid UTF-8 and recover: %s", output)
	}

	failureRequest := `{"protocol":"naatre.conformance.runner-1","id":"required","command":"run","path":{"source":{"kind":"sdk","language":"javascript-typescript"},"destination":{"kind":"native-runtime","language":"javascript-typescript"}},"profiles":["core.http-1"]}` + "\n"
	required := exec.Command("node", "../../conformance/independent/runner.mjs", "--require-pass")
	required.Stdin = strings.NewReader(failureRequest)
	requiredOutput, err := required.CombinedOutput()
	if err == nil {
		t.Fatalf("independent runner --require-pass exited successfully: %s", requiredOutput)
	}
	var requiredResponse struct {
		Results []struct {
			Profile string `json:"profile"`
			Status  string `json:"status"`
		} `json:"results"`
	}
	if decodeErr := json.Unmarshal(bytes.TrimSpace(requiredOutput), &requiredResponse); decodeErr != nil {
		t.Fatalf("decode independent --require-pass response: %v: %s", decodeErr, requiredOutput)
	}
	if len(requiredResponse.Results) != 1 || requiredResponse.Results[0].Profile != "core.http-1" || requiredResponse.Results[0].Status != "unsupported" {
		t.Fatalf("independent runner --require-pass result = %#v", requiredResponse.Results)
	}
}

func assertProfileInventory(t *testing.T, profiles []struct {
	Profile string `json:"profile"`
	Status  string `json:"status"`
}) {
	t.Helper()
	if len(profiles) == 0 {
		t.Fatal("runner discovery profile inventory is empty")
	}
	seen := make(map[string]bool, len(profiles))
	for _, profile := range profiles {
		if profile.Profile == "" || seen[profile.Profile] || (profile.Status != "supported" && profile.Status != "unsupported") {
			t.Errorf("invalid discovery profile entry %#v", profile)
		}
		seen[profile.Profile] = true
	}
}

func assertProfileSchema(t *testing.T) {
	t.Helper()
	content, err := os.ReadFile("../../conformance/runner-protocol.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Definitions map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(content, &schema); err != nil {
		t.Fatal(err)
	}
	var profile struct {
		AdditionalProperties *bool    `json:"additionalProperties"`
		Required             []string `json:"required"`
		Properties           map[string]struct {
			Type      string   `json:"type"`
			MinLength int      `json:"minLength"`
			MaxLength int      `json:"maxLength"`
			Enum      []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema.Definitions["profile"], &profile); err != nil {
		t.Fatalf("decode runner profile schema: %v", err)
	}
	if profile.AdditionalProperties == nil || *profile.AdditionalProperties || !slices.Equal(profile.Required, []string{"profile", "status"}) {
		t.Fatalf("runner profile schema does not require a closed profile/status object")
	}
	name := profile.Properties["profile"]
	status := profile.Properties["status"]
	if name.Type != "string" || name.MinLength != 1 || name.MaxLength != 128 || !slices.Equal(status.Enum, []string{"supported", "unsupported"}) {
		t.Fatalf("runner profile schema fields = %#v %#v", name, status)
	}
}

func TestIndependentRunnerHTTPMediaType(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("the independently pinned JavaScript runner requires Node: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	command := exec.CommandContext(ctx, "node", "../../conformance/independent/runner.mjs", "--http=127.0.0.1:0")
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = command.Wait()
	})
	scanner := bufio.NewScanner(stderr)
	var address string
	for scanner.Scan() {
		const prefix = "naatre-conformance-listening "
		if strings.HasPrefix(scanner.Text(), prefix) {
			address = strings.TrimPrefix(scanner.Text(), prefix)
			break
		}
	}
	if address == "" {
		t.Fatalf("independent HTTP runner did not publish its binding: %v", scanner.Err())
	}
	validBody := `{"protocol":"naatre.conformance.runner-1","id":"http","command":"run","path":{"source":{"kind":"sdk","language":"javascript-typescript"},"destination":{"kind":"native-runtime","language":"javascript-typescript"}},"profiles":["suite.contract-1"]}`
	emptyProfileBody := `{"protocol":"naatre.conformance.runner-1","id":"empty-profile","command":"run","path":{"source":{"kind":"sdk","language":"javascript-typescript","profile":""},"destination":{"kind":"native-runtime","language":"javascript-typescript"}},"profiles":["suite.contract-1"]}`
	unicodeIDBody := `{"protocol":"naatre.conformance.runner-1","id":"` + strings.Repeat("é", 100) + `","command":"run","path":{"source":{"kind":"sdk","language":"javascript-typescript"},"destination":{"kind":"native-runtime","language":"javascript-typescript"}},"profiles":["suite.contract-1"]}`
	astralLanguageBody := `{"protocol":"naatre.conformance.runner-1","id":"astral-language","command":"run","path":{"source":{"kind":"sdk","language":"` + strings.Repeat("😀", 64) + `"},"destination":{"kind":"native-runtime","language":"javascript-typescript"}},"profiles":["suite.contract-1"]}`
	overlongIDBody := `{"protocol":"naatre.conformance.runner-1","id":"` + strings.Repeat("😀", 129) + `","command":"discover"}`
	invalidUTF8Body := `{"protocol":"naatre.conformance.runner-1","id":"` + string([]byte{0xff}) + `","command":"discover"}`
	oversizedBody := strings.Repeat("x", conformancerunner.MaxInputBytes+1)
	for _, testCase := range []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
		wantCode    string
		wantID      string
	}{
		{name: "missing-media-type", body: validBody, wantStatus: http.StatusUnsupportedMediaType, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "wrong-media-type", body: validBody, contentType: "text/plain", wantStatus: http.StatusUnsupportedMediaType, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "non-utf8-charset", body: validBody, contentType: "application/json; charset=iso-8859-1", wantStatus: http.StatusUnsupportedMediaType, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "unknown-media-parameter", body: validBody, contentType: "application/json; charset=utf-8; mode=runner", wantStatus: http.StatusUnsupportedMediaType, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "oversized-body", body: oversizedBody, contentType: "application/json", wantStatus: http.StatusRequestEntityTooLarge, wantCode: "RUNNER_REQUEST_TOO_LARGE"},
		{name: "json", body: validBody, contentType: "application/json; charset=utf-8", wantStatus: http.StatusOK},
		{name: "json-without-charset", body: validBody, contentType: "application/json", wantStatus: http.StatusOK},
		{name: "malformed", body: `{"protocol":`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "MALFORMED_RUNNER_JSON"},
		{name: "invalid-utf8", body: invalidUTF8Body, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "MALFORMED_RUNNER_JSON"},
		{name: "unpaired-surrogate", body: `{"protocol":"naatre.conformance.runner-1","id":"\ud800","command":"discover"}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "MALFORMED_RUNNER_JSON", wantID: "invalid"},
		{name: "unpaired-surrogate-key", body: `{"protocol":"naatre.conformance.runner-1","id":"surrogate-key","command":"discover","\ud800":true}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "MALFORMED_RUNNER_JSON", wantID: "invalid"},
		{name: "unknown-field", body: `{"protocol":"naatre.conformance.runner-1","id":"unknown","command":"discover","extra":true}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_RUNNER_REQUEST"},
		{name: "invalid-command", body: `{"protocol":"naatre.conformance.runner-1","id":"invalid","command":"other"}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_RUNNER_REQUEST"},
		{name: "discover-on-run-endpoint", body: `{"protocol":"naatre.conformance.runner-1","id":"discover","command":"discover"}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_RUNNER_REQUEST"},
		{name: "empty-endpoint-profile", body: emptyProfileBody, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_RUNNER_PATH"},
		{name: "discover-invalid-path", body: `{"protocol":"naatre.conformance.runner-1","id":"bad-path","command":"discover","path":"not-an-object"}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_RUNNER_REQUEST"},
		{name: "discover-invalid-profiles", body: `{"protocol":"naatre.conformance.runner-1","id":"bad-profiles","command":"discover","profiles":[null]}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_RUNNER_REQUEST"},
		{name: "unicode-code-point-id", body: unicodeIDBody, contentType: "application/json", wantStatus: http.StatusOK},
		{name: "astral-code-point-language", body: astralLanguageBody, contentType: "application/json", wantStatus: http.StatusOK},
		{name: "overlong-astral-id", body: overlongIDBody, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_RUNNER_REQUEST", wantID: "invalid"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+address+"/v1/conformance/run", strings.NewReader(testCase.body))
			if err != nil {
				t.Fatal(err)
			}
			if testCase.contentType != "" {
				request.Header.Set("Content-Type", testCase.contentType)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			responseBody, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != testCase.wantStatus || (testCase.wantCode != "" && !bytes.Contains(responseBody, []byte(`"code":"`+testCase.wantCode+`"`))) {
				t.Fatalf("status = %d body = %s, want %d %s", response.StatusCode, responseBody, testCase.wantStatus, testCase.wantCode)
			}
			if testCase.wantStatus == http.StatusBadRequest && (!bytes.Contains(responseBody, []byte(`"fixtureVersion":"1.0.0"`)) || !bytes.Contains(responseBody, []byte(`"results":[`))) {
				t.Fatalf("HTTP 400 is not a complete runner response: %s", responseBody)
			}
			if testCase.wantID != "" && !bytes.Contains(responseBody, []byte(`"id":"`+testCase.wantID+`"`)) {
				t.Fatalf("HTTP response ID = %s, want %q", responseBody, testCase.wantID)
			}
		})
	}
}

func TestGoRunnerProtocol(t *testing.T) {
	t.Parallel()
	runner, err := conformancerunner.New("../../conformance/v1/suite.json")
	if err != nil {
		t.Fatal(err)
	}
	request := conformancerunner.Request{
		Protocol: conformancerunner.Protocol,
		ID:       "go-run",
		Command:  "run",
		Path: &conformancerunner.Path{
			Source:      conformancerunner.Endpoint{Kind: "sdk", Language: "go"},
			Destination: conformancerunner.Endpoint{Kind: "native-runtime", Language: "go"},
		},
		Profiles: []string{"suite.contract-1", "core.http-1"},
	}
	response := runner.Handle(context.Background(), request)
	if response.Protocol != conformancerunner.Protocol || response.FixtureVersion == "" || len(response.Results) != 2 {
		t.Fatalf("Go runner response = %#v", response)
	}
	if response.Results[0].Status != "passed" || response.Results[1].Status != "unsupported" {
		t.Fatalf("Go runner results = %#v", response.Results)
	}

	recorder := httptest.NewRecorder()
	httpRequest := httptest.NewRequest(http.MethodGet, "/v1/conformance/profiles", nil)
	runner.HTTPHandler().ServeHTTP(recorder, httpRequest)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"fixtureVersion":"1.0.0"`) {
		t.Fatalf("Go HTTP discovery = %d %s", recorder.Code, recorder.Body.String())
	}

	httpBody := `{"protocol":"naatre.conformance.runner-1","id":"http","command":"run","path":{"source":{"kind":"sdk","language":"go"},"destination":{"kind":"native-runtime","language":"go"}},"profiles":["suite.contract-1"]}`
	emptyProfileBody := `{"protocol":"naatre.conformance.runner-1","id":"empty-profile","command":"run","path":{"source":{"kind":"sdk","language":"go","profile":""},"destination":{"kind":"native-runtime","language":"go"}},"profiles":["suite.contract-1"]}`
	unicodeIDBody := `{"protocol":"naatre.conformance.runner-1","id":"` + strings.Repeat("é", 100) + `","command":"run","path":{"source":{"kind":"sdk","language":"go"},"destination":{"kind":"native-runtime","language":"go"}},"profiles":["suite.contract-1"]}`
	astralLanguageBody := `{"protocol":"naatre.conformance.runner-1","id":"astral-language","command":"run","path":{"source":{"kind":"sdk","language":"` + strings.Repeat("😀", 64) + `"},"destination":{"kind":"native-runtime","language":"go"}},"profiles":["suite.contract-1"]}`
	overlongIDBody := `{"protocol":"naatre.conformance.runner-1","id":"` + strings.Repeat("😀", 129) + `","command":"discover"}`
	invalidUTF8Body := `{"protocol":"naatre.conformance.runner-1","id":"` + string([]byte{0xff}) + `","command":"discover"}`
	oversizedHTTPBody := strings.Repeat("x", conformancerunner.MaxInputBytes+1)
	for _, testCase := range []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
		wantCode    string
		wantID      string
	}{
		{name: "missing-media-type", body: httpBody, wantStatus: http.StatusUnsupportedMediaType, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "wrong-media-type", body: httpBody, contentType: "text/plain", wantStatus: http.StatusUnsupportedMediaType, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "non-utf8-charset", body: httpBody, contentType: "application/json; charset=iso-8859-1", wantStatus: http.StatusUnsupportedMediaType, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "unknown-media-parameter", body: httpBody, contentType: "application/json; charset=utf-8; mode=runner", wantStatus: http.StatusUnsupportedMediaType, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "oversized-body", body: oversizedHTTPBody, contentType: "application/json", wantStatus: http.StatusRequestEntityTooLarge, wantCode: "RUNNER_REQUEST_TOO_LARGE"},
		{name: "json", body: httpBody, contentType: "application/json; charset=utf-8", wantStatus: http.StatusOK},
		{name: "json-without-charset", body: httpBody, contentType: "application/json", wantStatus: http.StatusOK},
		{name: "malformed", body: `{"protocol":`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "MALFORMED_RUNNER_JSON"},
		{name: "invalid-utf8", body: invalidUTF8Body, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "MALFORMED_RUNNER_JSON"},
		{name: "unpaired-surrogate", body: `{"protocol":"naatre.conformance.runner-1","id":"\ud800","command":"discover"}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "MALFORMED_RUNNER_JSON", wantID: "invalid"},
		{name: "unpaired-surrogate-key", body: `{"protocol":"naatre.conformance.runner-1","id":"surrogate-key","command":"discover","\ud800":true}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "MALFORMED_RUNNER_JSON", wantID: "invalid"},
		{name: "unknown-field", body: `{"protocol":"naatre.conformance.runner-1","id":"unknown","command":"discover","extra":true}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_RUNNER_REQUEST"},
		{name: "invalid-command", body: `{"protocol":"naatre.conformance.runner-1","id":"invalid","command":"other"}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_RUNNER_REQUEST"},
		{name: "discover-on-run-endpoint", body: `{"protocol":"naatre.conformance.runner-1","id":"discover","command":"discover"}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_RUNNER_REQUEST"},
		{name: "empty-endpoint-profile", body: emptyProfileBody, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_RUNNER_PATH"},
		{name: "discover-invalid-path", body: `{"protocol":"naatre.conformance.runner-1","id":"bad-path","command":"discover","path":"not-an-object"}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_RUNNER_REQUEST"},
		{name: "discover-invalid-profiles", body: `{"protocol":"naatre.conformance.runner-1","id":"bad-profiles","command":"discover","profiles":[null]}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_RUNNER_REQUEST"},
		{name: "unicode-code-point-id", body: unicodeIDBody, contentType: "application/json", wantStatus: http.StatusOK},
		{name: "astral-code-point-language", body: astralLanguageBody, contentType: "application/json", wantStatus: http.StatusOK},
		{name: "overlong-astral-id", body: overlongIDBody, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_RUNNER_REQUEST", wantID: "invalid"},
	} {
		t.Run("http-"+testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/conformance/run", strings.NewReader(testCase.body))
			if testCase.contentType != "" {
				request.Header.Set("Content-Type", testCase.contentType)
			}
			runner.HTTPHandler().ServeHTTP(recorder, request)
			if recorder.Code != testCase.wantStatus || (testCase.wantCode != "" && !strings.Contains(recorder.Body.String(), `"code":"`+testCase.wantCode+`"`)) {
				t.Fatalf("Go HTTP = %d %s, want %d %s", recorder.Code, recorder.Body.String(), testCase.wantStatus, testCase.wantCode)
			}
			if testCase.wantStatus == http.StatusBadRequest && (!strings.Contains(recorder.Body.String(), `"fixtureVersion":"1.0.0"`) || !strings.Contains(recorder.Body.String(), `"results":[`)) {
				t.Fatalf("Go HTTP 400 is not a complete runner response: %s", recorder.Body.String())
			}
			if testCase.wantID != "" && !strings.Contains(recorder.Body.String(), `"id":"`+testCase.wantID+`"`) {
				t.Fatalf("Go HTTP response ID = %s, want %q", recorder.Body.String(), testCase.wantID)
			}
		})
	}

	var output bytes.Buffer
	input := "{\"protocol\":\n" + `{"protocol":"naatre.conformance.runner-1","id":"unknown","command":"discover","extra":true}` + "\n"
	if err := runner.ServeNDJSON(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"code":"MALFORMED_RUNNER_JSON"`) || !strings.Contains(output.String(), `"code":"INVALID_RUNNER_REQUEST"`) {
		t.Fatalf("Go NDJSON error parity = %s", output.String())
	}

	output.Reset()
	after := []byte(`{"protocol":"naatre.conformance.runner-1","id":"after","command":"discover"}` + "\n")
	oversized := append(bytes.Repeat([]byte("x"), conformancerunner.MaxInputBytes+1), '\n')
	oversized = append(oversized, after...)
	if err := runner.ServeNDJSON(context.Background(), bytes.NewReader(oversized), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"code":"RUNNER_REQUEST_TOO_LARGE"`) || !strings.Contains(output.String(), `"id":"after"`) {
		t.Fatalf("Go runner did not recover after oversized line: %s", output.String())
	}

	output.Reset()
	requiredInput := `{"protocol":"naatre.conformance.runner-1","id":"required","command":"run","path":{"source":{"kind":"sdk","language":"go"},"destination":{"kind":"native-runtime","language":"go"}},"profiles":["core.http-1"]}` + "\n"
	options := conformancerunner.NDJSONOptions{RequirePass: true}
	err = runner.ServeNDJSONWithOptions(context.Background(), strings.NewReader(requiredInput), &output, options)
	if !errors.Is(err, conformancerunner.ErrRequiredProfileFailed) {
		t.Fatalf("Go runner require-pass error = %v: %s", err, output.String())
	}
	var requiredResponse struct {
		Results []struct {
			Profile string `json:"profile"`
			Status  string `json:"status"`
		} `json:"results"`
	}
	if decodeErr := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &requiredResponse); decodeErr != nil {
		t.Fatalf("decode Go require-pass response: %v: %s", decodeErr, output.String())
	}
	if len(requiredResponse.Results) != 1 || requiredResponse.Results[0].Profile != "core.http-1" || requiredResponse.Results[0].Status != "unsupported" {
		t.Fatalf("Go runner require-pass result = %#v", requiredResponse.Results)
	}
}

func TestGoRunnerRejectsInvalidHandlerResults(t *testing.T) {
	t.Parallel()
	runner, err := conformancerunner.New("../../conformance/v1/suite.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Register(strings.Repeat("😀", 129), func(context.Context, conformancerunner.Request) conformancerunner.Result {
		return conformancerunner.Result{}
	}); err == nil {
		t.Fatal("registered an overlong profile name")
	}
	if err := runner.Register("invalid-result-1", func(context.Context, conformancerunner.Request) conformancerunner.Result {
		return conformancerunner.Result{
			Status:       "passed",
			Capabilities: []string{"duplicate", "duplicate"},
			Phase:        "nonsense",
			Path:         []any{true},
			Data:         func() {},
			Diagnostics: []conformancerunner.Diagnostic{{
				Phase: "nonsense", Code: "lowercase", Message: strings.Repeat("x", 1025),
				Source: map[string]any{"secret": "value"}, Path: []any{false},
			}},
			Evidence: []conformancerunner.Evidence{{Fixture: "fixture", SHA256: "not-a-digest"}},
		}
	}); err != nil {
		t.Fatal(err)
	}
	response := runner.Handle(context.Background(), conformancerunner.Request{
		Protocol: conformancerunner.Protocol,
		ID:       "invalid-result",
		Command:  "run",
		Path: &conformancerunner.Path{
			Source:      conformancerunner.Endpoint{Kind: "sdk", Language: "go"},
			Destination: conformancerunner.Endpoint{Kind: "native-runtime", Language: "go"},
		},
		Profiles: []string{"invalid-result-1"},
	})
	if len(response.Results) != 1 || response.Results[0].Status != "error" || response.Results[0].Code != "INVALID_RUNNER_RESULT" {
		t.Fatalf("invalid handler result was emitted: %#v", response)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(encoded) || bytes.Contains(encoded, []byte("nonsense")) || bytes.Contains(encoded, []byte("not-a-digest")) {
		t.Fatalf("normalized response is not a safe protocol response: %s", encoded)
	}
}

func TestGoRunnerParallelCancellationProfile(t *testing.T) {
	t.Parallel()
	runner, err := conformancerunner.New("../../conformance/v1/suite.json")
	if err != nil {
		t.Fatal(err)
	}
	handler, started := parallelCancellationHandler(t)
	if err := runner.Register("core.parallel-cancellation-1", handler); err != nil {
		t.Fatal(err)
	}
	request := conformancerunner.Request{
		Protocol: conformancerunner.Protocol,
		ID:       "parallel-cancel",
		Command:  "run",
		Path: &conformancerunner.Path{
			Source:      conformancerunner.Endpoint{Kind: "sdk", Language: "go"},
			Destination: conformancerunner.Endpoint{Kind: "native-runtime", Language: "go"},
		},
		Profiles: []string{"core.parallel-cancellation-1"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	responses := make(chan conformancerunner.Response, 1)
	go func() { responses <- runner.Handle(ctx, request) }()
	<-started
	cancel()
	var response conformancerunner.Response
	select {
	case response = <-responses:
	case <-time.After(time.Second):
		t.Fatal("Go runner did not terminate the cancelled parallel fixture")
	}
	if len(response.Results) != 1 || response.Results[0].Status != "passed" || response.Results[0].Code != "CANCELLED" {
		t.Fatalf("cancelled profile = %#v", response.Results)
	}

	const parallelRuns = 8
	results := make(chan conformancerunner.Response, parallelRuns)
	for index := 0; index < parallelRuns; index++ {
		go func(index int) {
			parallel := request
			parallel.ID = fmt.Sprintf("parallel-%d", index)
			parallel.Profiles = []string{"suite.contract-1"}
			results <- runner.Handle(context.Background(), parallel)
		}(index)
	}
	for range parallelRuns {
		parallel := <-results
		if len(parallel.Results) != 1 || parallel.Results[0].Status != "passed" {
			t.Fatalf("parallel profile = %#v", parallel.Results)
		}
	}
}

func parallelCancellationHandler(t *testing.T) (conformancerunner.Handler, <-chan struct{}) {
	t.Helper()
	types, err := schema.NewCatalog().Freeze()
	if err != nil {
		t.Fatal(err)
	}
	registry := naatreruntime.NewRegistry(types)
	started := make(chan struct{})
	var startedOnce sync.Once
	var active, maximum atomic.Int64
	descriptor := naatreruntime.Descriptor{
		Name: "block", Scope: naatreruntime.RootScope, Kind: protocol.Query, Member: naatreruntime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String),
		Metadata: naatreruntime.Metadata{
			Effect: naatreruntime.ReadEffect, Deterministic: true, Cacheable: true, RetrySafe: true,
			ThreadSafety: naatreruntime.ThreadSafe, Batching: naatreruntime.BatchIneligible,
			Transaction: naatreruntime.TransactionNone, AuthorizationPolicy: "conformance",
		},
	}
	if err := registry.Register(naatreruntime.BindInvocation[string](descriptor, func(ctx context.Context, _ naatreruntime.Invocation) (string, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for observed := maximum.Load(); current > observed && !maximum.CompareAndSwap(observed, current); observed = maximum.Load() {
		}
		startedOnce.Do(func() { close(started) })
		<-ctx.Done()
		return "", ctx.Err()
	})); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	selections := make([]any, 32)
	for index := range selections {
		selections[index] = map[string]any{"$call": map[string]any{"name": "block", "as": fmt.Sprintf("branch%d", index)}}
	}
	envelope, err := json.Marshal(map[string]any{"version": "1", "document": map[string]any{"operations": []any{map[string]any{
		"name": "Q", "kind": "query", "select": []any{map[string]any{"$parallel": map[string]any{"select": selections}}},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	request, err := protocol.DecodeRequest(envelope, protocol.DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := naatreruntime.Prepare(snapshot, request)
	if err != nil {
		t.Fatal(err)
	}
	return func(ctx context.Context, _ conformancerunner.Request) conformancerunner.Result {
		outcome := plan.Execute(ctx)
		if active.Load() != 0 || maximum.Load() > 8 || len(outcome.Errors) == 0 {
			return conformancerunner.Result{Status: "failed", Phase: "runner", Code: "PARALLEL_CANCELLATION_FAILED"}
		}
		for _, failure := range outcome.Errors {
			if failure.Code != "CANCELLED" {
				return conformancerunner.Result{Status: "failed", Phase: "runner", Code: "PARALLEL_CANCELLATION_FAILED"}
			}
		}
		return conformancerunner.Result{Status: "passed", Capabilities: []string{"core.parallel-cancellation-1"}, Phase: "execute", Code: "CANCELLED"}
	}, started
}

func assertFixtureInventory(t *testing.T, manifest suiteManifest) {
	t.Helper()
	seen := make(map[string]bool)
	for _, file := range manifest.Files {
		if file.Path == "" || file.Profile == "" || file.Category == "" || seen[file.Path] {
			t.Errorf("invalid or duplicate suite file %#v", file)
			continue
		}
		seen[file.Path] = true
		content, err := os.ReadFile(filepath.Join("../../conformance", filepath.FromSlash(file.Path)))
		if err != nil {
			t.Errorf("read suite file %s: %v", file.Path, err)
			continue
		}
		digest := sha256.Sum256(content)
		if actual := hex.EncodeToString(digest[:]); actual != file.SHA256 {
			t.Errorf("suite file %s digest = %s, want %s", file.Path, actual, file.SHA256)
		}
		var header struct {
			Profile string `json:"profile"`
		}
		if err := json.Unmarshal(content, &header); err != nil || header.Profile != file.Profile {
			t.Errorf("suite file %s profile = %q, %v; want %q", file.Path, header.Profile, err, file.Profile)
		}
	}
	entries, err := filepath.Glob("../../conformance/v1/*.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		path := "v1/" + filepath.Base(entry)
		if path != "v1/suite.json" && !seen[path] {
			t.Errorf("fixture %s is absent from suite inventory", path)
		}
	}
}

func assertNormativeCoverage(t *testing.T, manifest suiteManifest) {
	t.Helper()
	mapped := make(map[string]normativeSource)
	coveredClauses := make(map[string]string)
	caseIDs := make(map[string]bool)
	for _, source := range manifest.NormativeSources {
		if source.CaseID == "" || caseIDs[source.CaseID] || source.Spec == "" || len(source.Clauses) == 0 || len(source.FixtureRefs) == 0 {
			t.Errorf("incomplete normative source %#v", source)
			continue
		}
		caseIDs[source.CaseID] = true
		mapped[source.Spec] = source
		assertFixtureRefs(t, source.FixtureRefs)
		if len(source.Clauses) > 5 {
			t.Errorf("normative mapping %s is too coarse: %d clauses", source.CaseID, len(source.Clauses))
		}
		for _, reference := range source.FixtureRefs {
			content, err := os.ReadFile(filepath.Join("../../conformance", filepath.FromSlash(reference.Path)))
			if err != nil {
				continue
			}
			var fixture any
			if json.Unmarshal(content, &fixture) != nil {
				continue
			}
			resolved, err := resolveJSONPointer(fixture, reference.Pointer)
			if err == nil {
				if _, exact := resolved.(map[string]any); !exact {
					t.Errorf("normative mapping %s fixture %s%s does not select an exact object", source.CaseID, reference.Path, reference.Pointer)
				}
			}
		}
		content, err := os.ReadFile(filepath.Join("../..", filepath.FromSlash(source.Spec)))
		if err != nil {
			t.Errorf("read normative source %s: %v", source.Spec, err)
			continue
		}
		declared := make(map[string]bool)
		for _, match := range clauseDefinition.FindAllStringSubmatch(string(content), -1) {
			declared[match[1]] = true
		}
		for _, clause := range source.Clauses {
			if !declared[clause] {
				t.Errorf("mapping %s cites undeclared clause %s", source.CaseID, clause)
			}
			if previous := coveredClauses[clause]; previous != "" {
				t.Errorf("clause %s is mapped by both %s and %s", clause, previous, source.CaseID)
			}
			coveredClauses[clause] = source.CaseID
		}
	}

	specFiles, err := filepath.Glob("../../spec/v1/*.md")
	if err != nil {
		t.Fatal(err)
	}
	mustClauses := make(map[string]bool)
	for _, specFile := range specFiles {
		if filepath.Base(specFile) == "README.md" {
			continue
		}
		relative := "spec/v1/" + filepath.Base(specFile)
		if _, ok := mapped[relative]; !ok {
			t.Errorf("normative source %s has no fixture mapping", relative)
		}
		content, readErr := os.ReadFile(specFile)
		if readErr != nil {
			t.Fatal(readErr)
		}
		matches := clauseDefinition.FindAllStringSubmatch(string(content), -1)
		if len(matches) == 0 {
			t.Errorf("normative source %s has no stable clauses", relative)
		}
		for _, match := range matches {
			if coveredClauses[match[1]] == "" {
				t.Errorf("normative clause %s has no positive or negative fixture", match[1])
			}
		}
		for clause, block := range declaredClauseBlocks(string(content)) {
			if mustKeyword.MatchString(block) {
				mustClauses[clause] = true
			}
		}
	}
	if len(coveredClauses) != 492 {
		t.Errorf("mapped normative clauses = %d, want 492", len(coveredClauses))
	}
	if len(mustClauses) != 215 {
		t.Errorf("mapped MUST/MUST NOT clause blocks = %d, want 215", len(mustClauses))
	}
	for clause := range mustClauses {
		if coveredClauses[clause] == "" {
			t.Errorf("MUST/MUST NOT clause %s has no fixture mapping", clause)
		}
	}
}

func declaredClauseBlocks(content string) map[string]string {
	matches := clauseDefinition.FindAllStringSubmatchIndex(content, -1)
	blocks := make(map[string]string, len(matches))
	for index, match := range matches {
		clause := content[match[2]:match[3]]
		if _, exists := blocks[clause]; exists {
			continue
		}
		end := len(content)
		if index+1 < len(matches) {
			end = matches[index+1][0]
		}
		blocks[clause] = content[match[0]:end]
	}
	return blocks
}

func assertIssueCoverage(t *testing.T, manifest suiteManifest) {
	t.Helper()
	roadmapContent, err := os.ReadFile("../../conformance/v1/roadmap.json")
	if err != nil {
		t.Fatal(err)
	}
	var roadmap any
	if err := json.Unmarshal(roadmapContent, &roadmap); err != nil {
		t.Fatal(err)
	}
	covered := make(map[int]bool)
	caseIDs := make(map[string]bool)
	for _, mapping := range manifest.IssueMappings {
		if mapping.CaseID == "" || caseIDs[mapping.CaseID] || mapping.Issue < 1 || covered[mapping.Issue] {
			t.Errorf("invalid or duplicate issue mapping %d", mapping.Issue)
		}
		caseIDs[mapping.CaseID] = true
		covered[mapping.Issue] = true
		assertFixtureRefs(t, mapping.FixtureRefs)
		for _, reference := range mapping.FixtureRefs {
			if reference.Path != "v1/roadmap.json" {
				continue
			}
			resolved, err := resolveJSONPointer(roadmap, reference.Pointer)
			entry, ok := resolved.(map[string]any)
			if err != nil || !ok || entry["issue"] != float64(mapping.Issue) {
				t.Errorf("issue #%d roadmap reference %s does not select its exact record", mapping.Issue, reference.Pointer)
			}
		}
	}
	for issue := 1; issue <= 110; issue++ {
		if !covered[issue] {
			t.Errorf("repository issue #%d has no fixture or explicit deferred-owner mapping", issue)
		}
	}
}

func assertFixtureRefs(t *testing.T, references []fixtureRef) {
	t.Helper()
	for _, reference := range references {
		if reference.Polarity != "positive" && reference.Polarity != "negative" && reference.Polarity != "mixed" {
			t.Errorf("fixture reference %s%s has invalid polarity %q", reference.Path, reference.Pointer, reference.Polarity)
			continue
		}
		content, err := os.ReadFile(filepath.Join("../../conformance", filepath.FromSlash(reference.Path)))
		if err != nil {
			t.Errorf("read fixture reference %s: %v", reference.Path, err)
			continue
		}
		var value any
		if err := json.Unmarshal(content, &value); err != nil {
			t.Errorf("parse fixture reference %s: %v", reference.Path, err)
			continue
		}
		resolved, err := resolveJSONPointer(value, reference.Pointer)
		if err != nil {
			t.Errorf("fixture reference %s%s: %v", reference.Path, reference.Pointer, err)
			continue
		}
		if emptyJSONValue(resolved) {
			t.Errorf("fixture reference %s%s selects an empty value", reference.Path, reference.Pointer)
		}
	}
}

func resolveJSONPointer(value any, pointer string) (any, error) {
	if pointer == "" {
		return value, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("invalid JSON pointer")
	}
	current := value
	for _, raw := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		part := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		switch container := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = container[part]
			if !ok {
				return nil, fmt.Errorf("member %q does not exist", part)
			}
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(container) {
				return nil, fmt.Errorf("array index %q does not exist", part)
			}
			current = container[index]
		default:
			return nil, fmt.Errorf("%q does not select a container", part)
		}
	}
	return current, nil
}

func emptyJSONValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return typed == ""
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	default:
		return false
	}
}

func assertInteractionScenarios(t *testing.T) {
	t.Helper()
	content, err := os.ReadFile("../../conformance/v1/interactions.json")
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Scenarios map[string]struct {
			FixtureRefs []struct {
				Path    string `json:"path"`
				Pointer string `json:"pointer"`
			} `json:"fixtureRefs"`
			Setup    map[string]any `json:"setup"`
			Schedule []struct {
				Sequence int    `json:"sequence"`
				Actor    string `json:"actor"`
				Action   string `json:"action"`
			} `json:"schedule"`
		} `json:"scenarios"`
		Cases []struct {
			Name        string `json:"name"`
			ScenarioRef string `json:"scenarioRef"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(content, &catalog); err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(content, &document); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range catalog.Cases {
		scenario, ok := catalog.Scenarios[testCase.Name]
		if !ok || testCase.ScenarioRef != "#/scenarios/"+testCase.Name {
			t.Errorf("interaction case %s has no exact scenario reference", testCase.Name)
			continue
		}
		resolved, err := resolveJSONPointer(document, strings.TrimPrefix(testCase.ScenarioRef, "#"))
		if err != nil || emptyJSONValue(resolved) {
			t.Errorf("interaction case %s resolves scenario: %v", testCase.Name, err)
		}
		if len(scenario.FixtureRefs) == 0 || len(scenario.Setup) == 0 || len(scenario.Schedule) == 0 {
			t.Errorf("interaction case %s does not define portable fixtures, setup, and schedule", testCase.Name)
		}
		for index, step := range scenario.Schedule {
			if step.Sequence != index+1 || step.Actor == "" || step.Action == "" {
				t.Errorf("interaction case %s has invalid schedule step %#v", testCase.Name, step)
			}
		}
		for _, reference := range scenario.FixtureRefs {
			fixtureContent, err := os.ReadFile(filepath.Join("../../conformance", filepath.FromSlash(reference.Path)))
			if err != nil {
				t.Errorf("interaction case %s reads fixture %s: %v", testCase.Name, reference.Path, err)
				continue
			}
			var fixture any
			if err := json.Unmarshal(fixtureContent, &fixture); err != nil {
				t.Errorf("interaction case %s parses fixture %s: %v", testCase.Name, reference.Path, err)
				continue
			}
			resolved, err := resolveJSONPointer(fixture, reference.Pointer)
			if err != nil {
				t.Errorf("interaction case %s resolves fixture %s%s: %v", testCase.Name, reference.Path, reference.Pointer, err)
			} else if _, exact := resolved.(map[string]any); !exact {
				t.Errorf("interaction case %s fixture %s%s does not select an exact object", testCase.Name, reference.Path, reference.Pointer)
			}
		}
	}
}

func assertCaseCatalog(t *testing.T, name string, required []string) {
	t.Helper()
	content, err := os.ReadFile("../../conformance/v1/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Profile string                       `json:"profile"`
		Cases   []map[string]json.RawMessage `json:"cases"`
	}
	if err := json.Unmarshal(content, &catalog); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	if catalog.Profile == "" {
		t.Errorf("%s has no profile", name)
	}
	names := make(map[string]bool)
	for _, raw := range catalog.Cases {
		var caseID string
		var clauses, capabilities []string
		var issues []int
		if err := json.Unmarshal(raw["name"], &caseID); err != nil {
			t.Errorf("%s contains case without name", name)
			continue
		}
		_ = json.Unmarshal(raw["clauses"], &clauses)
		_ = json.Unmarshal(raw["issues"], &issues)
		_ = json.Unmarshal(raw["capabilities"], &capabilities)
		if caseID == "" || names[caseID] || len(clauses) == 0 || len(issues) == 0 || len(capabilities) == 0 {
			t.Errorf("%s contains incomplete case %q", name, caseID)
		}
		names[caseID] = true
		var expected map[string]json.RawMessage
		if err := json.Unmarshal(raw["expected"], &expected); err != nil {
			t.Errorf("%s case %s omits expected result", name, caseID)
			continue
		}
		for _, field := range []string{"phase", "code", "source", "path", "data", "errors", "canonicalBytes"} {
			if _, present := expected[field]; !present {
				t.Errorf("%s case %s omits expected.%s", name, caseID, field)
			}
		}
		var dataObject map[string]json.RawMessage
		if json.Unmarshal(expected["data"], &dataObject) == nil {
			if _, nested := dataObject["canonicalBytes"]; nested {
				t.Errorf("%s case %s nests canonicalBytes under expected.data", name, caseID)
			}
		}
	}
	for _, expected := range required {
		if !names[expected] {
			t.Errorf("%s omits required case %q", name, expected)
		}
	}
}

func assertAdversarialFixtureRefs(t *testing.T) {
	t.Helper()
	var catalog struct {
		Profile        string          `json:"profile"`
		ResourcePolicy json.RawMessage `json:"resourcePolicy"`
		Cases          []struct {
			Name       string `json:"name"`
			FixtureRef string `json:"fixtureRef"`
			Budget     struct {
				Limit json.RawMessage `json:"limit"`
			} `json:"budget"`
		} `json:"cases"`
	}
	content, err := os.ReadFile("../../conformance/v1/adversarial.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &catalog); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range catalog.Cases {
		path, pointer, ok := strings.Cut(testCase.FixtureRef, "#")
		if !ok || path == "" || pointer == "" {
			t.Errorf("adversarial case %s has invalid fixtureRef %q", testCase.Name, testCase.FixtureRef)
			continue
		}
		content, err := os.ReadFile(filepath.Join("../../conformance", filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("adversarial case %s reads fixtureRef %q: %v", testCase.Name, testCase.FixtureRef, err)
			continue
		}
		var fixture any
		if err := json.Unmarshal(content, &fixture); err != nil {
			t.Errorf("adversarial case %s parses fixtureRef %q: %v", testCase.Name, testCase.FixtureRef, err)
			continue
		}
		resolved, err := resolveJSONPointer(fixture, pointer)
		if err != nil {
			t.Errorf("adversarial case %s resolves fixtureRef %q: %v", testCase.Name, testCase.FixtureRef, err)
			continue
		}
		resolvedObject, ok := resolved.(map[string]any)
		if !ok || len(resolvedObject) == 0 {
			t.Errorf("adversarial case %s fixtureRef %q does not select an exact object", testCase.Name, testCase.FixtureRef)
			continue
		}
		if path == "v1/adversarial.json" && resolvedObject["name"] != testCase.Name {
			t.Errorf("adversarial case %s self-reference selects case %v", testCase.Name, resolvedObject["name"])
		}
		if testCase.Name == "cost-overflow" {
			var limit string
			if err := json.Unmarshal(testCase.Budget.Limit, &limit); err != nil || limit != "18446744073709551615" {
				t.Errorf("cost-overflow budget limit must be an exact decimal string, got %s", testCase.Budget.Limit)
			}
		}
	}
}

package conformancerunner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/valksor/naatre/client"
	"github.com/valksor/naatre/protocol"
)

const goClientProfile = "sdk.go.client-1"

type goClientFixture struct {
	Profile        string `json:"profile"`
	FixtureSuite   string `json:"fixtureSuite"`
	Implementation struct {
		Module    string         `json:"module"`
		MinimumGo string         `json:"minimumGo"`
		GoMod     evidenceFile   `json:"goMod"`
		Files     []evidenceFile `json:"files"`
	} `json:"implementation"`
	Sources struct {
		Model  evidenceFile `json:"model"`
		Output evidenceFile `json:"output"`
	} `json:"sources"`
	Transport struct {
		RequestMediaType       string `json:"requestMediaType"`
		ResponseMediaType      string `json:"responseMediaType"`
		ProblemMediaType       string `json:"problemMediaType"`
		CompressedBytes        int64  `json:"compressedBytes"`
		DecompressedBytes      int64  `json:"decompressedBytes"`
		Redirects              int    `json:"redirects"`
		CrossOriginCredentials string `json:"crossOriginCredentials"`
		Cancellation           string `json:"cancellation"`
	} `json:"transport"`
	Vectors     []string `json:"vectors"`
	Unsupported []string `json:"unsupported"`
}

type evidenceFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type generatorClientModel struct {
	Operations []struct {
		Name             string                     `json:"name"`
		Document         json.RawMessage            `json:"document"`
		RequestVariables map[string]json.RawMessage `json:"requestVariables"`
	} `json:"operations"`
}

type generatorClientOutput struct {
	Operations []struct {
		Persisted protocol.Digest `json:"persisted"`
		Request   json.RawMessage `json:"request"`
	} `json:"operations"`
}

func (r *Runner) verifyGoClient(ctx context.Context, _ Request) Result {
	fixture, fixtureEvidence, err := r.loadGoClientFixture()
	if err != nil {
		return failureResult(goClientProfile, "GO_CLIENT_FIXTURE_INVALID", "v1/go-client.json")
	}
	evidence, contents, err := r.verifyGoClientEvidence(fixture)
	if err != nil {
		return failureResult(goClientProfile, "GO_CLIENT_EVIDENCE_MISMATCH", "v1/go-client.json")
	}
	if err := verifyGoClientRequest(contents[fixture.Sources.Model.Path], contents[fixture.Sources.Output.Path]); err != nil {
		return failureResult(goClientProfile, "GO_CLIENT_REQUEST_FAILED", "v1/go-client.json")
	}
	if err := verifyGoClientTransport(ctx, fixture); err != nil {
		return failureResult(goClientProfile, "GO_CLIENT_TRANSPORT_FAILED", "v1/go-client.json")
	}
	result := emptyResult(goClientProfile, "passed", "")
	result.Capabilities = []string{goClientProfile}
	result.Evidence = append([]Evidence{fixtureEvidence}, evidence...)
	return result
}

func (r *Runner) loadGoClientFixture() (goClientFixture, Evidence, error) {
	return loadProfileFixture(r, "v1/go-client.json", func(fixture goClientFixture) bool {
		return fixture.Profile == goClientProfile && fixture.FixtureSuite == r.manifest.FixtureVersion &&
			fixture.Implementation.Module == "github.com/valksor/naatre/client" && fixture.Implementation.MinimumGo == "1.27"
	})
}

func (r *Runner) verifyGoClientEvidence(fixture goClientFixture) ([]Evidence, map[string][]byte, error) {
	files := append([]evidenceFile{fixture.Implementation.GoMod, fixture.Sources.Model, fixture.Sources.Output}, fixture.Implementation.Files...)
	return r.verifyEvidenceFiles(files)
}

func (r *Runner) loadPinnedFixture(relative string, target any) ([]byte, Evidence, error) {
	path, err := r.fixturePath(relative)
	if err != nil {
		return nil, Evidence{}, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, Evidence{}, err
	}
	if err := json.Unmarshal(content, target); err != nil {
		return nil, Evidence{}, err
	}
	digest := sha256.Sum256(content)
	actual := hex.EncodeToString(digest[:])
	for _, file := range r.manifest.Files {
		if file.Path == relative && file.SHA256 == actual {
			return content, Evidence{Fixture: file.Path, SHA256: actual}, nil
		}
	}
	return nil, Evidence{}, errors.New("fixture is not pinned by suite")
}

func (r *Runner) verifyEvidenceFiles(files []evidenceFile) ([]Evidence, map[string][]byte, error) {
	evidence := make([]Evidence, 0, len(files))
	contents := make(map[string][]byte, len(files))
	repositoryRoot := filepath.Dir(r.conformanceRoot)
	for _, file := range files {
		clean := filepath.Clean(file.Path)
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, nil, errors.New("evidence path escapes repository")
		}
		content, err := os.ReadFile(filepath.Join(repositoryRoot, clean))
		if err != nil {
			return nil, nil, err
		}
		digest := sha256.Sum256(content)
		actual := hex.EncodeToString(digest[:])
		if actual != file.SHA256 {
			return nil, nil, errors.New("evidence digest mismatch")
		}
		evidence = append(evidence, Evidence{Fixture: clean, SHA256: actual})
		contents[clean] = content
	}
	return evidence, contents, nil
}

func verifyGoClientRequest(modelBytes, outputBytes []byte) error {
	var model generatorClientModel
	var output generatorClientOutput
	if err := json.Unmarshal(modelBytes, &model); err != nil || len(model.Operations) != 1 {
		return errors.New("invalid generator model")
	}
	if err := json.Unmarshal(outputBytes, &output); err != nil || len(output.Operations) != 1 {
		return errors.New("invalid generator output")
	}
	document, err := protocol.DecodeDocument(model.Operations[0].Document, protocol.Limits{})
	if err != nil {
		return err
	}
	inline, err := client.NewRequest(document, model.Operations[0].Name)
	if err != nil {
		return err
	}
	digest, err := inline.DocumentDigest()
	if err != nil || digest != output.Operations[0].Persisted {
		return errors.New("document digest mismatch")
	}
	persisted, err := client.NewPersistedRequest(protocol.PersistedReference{
		Algorithm: digest.Algorithm, CanonicalVersion: digest.CanonicalVersion, Digest: digest.Hex,
	}, model.Operations[0].Name)
	if err != nil {
		return err
	}
	for name, value := range model.Operations[0].RequestVariables {
		persisted, err = persisted.WithVariable(name, value)
		if err != nil {
			return err
		}
	}
	actual, err := persisted.CanonicalJSON()
	if err != nil {
		return err
	}
	expected, err := protocol.CanonicalizeJSON(output.Operations[0].Request, protocol.Limits{})
	if err != nil || !bytes.Equal(actual, expected) {
		return errors.New("canonical request mismatch")
	}
	return nil
}

func verifyGoClientTransport(ctx context.Context, fixture goClientFixture) error {
	if fixture.Transport.RequestMediaType != client.RequestMediaType || fixture.Transport.ResponseMediaType != client.ResponseMediaType || fixture.Transport.ProblemMediaType != client.ProblemMediaType || fixture.Transport.CrossOriginCredentials != "strip" || fixture.Transport.Cancellation != "active-abort" || fixture.Transport.Redirects != 5 {
		return errors.New("transport contract mismatch")
	}
	document, err := protocol.DecodeDocument([]byte(`{"operations":[{"name":"Ping","kind":"query","select":[{"$call":{"name":"ping"}}]}]}`), protocol.Limits{})
	if err != nil {
		return err
	}
	request, err := client.NewRequest(document, "Ping")
	if err != nil {
		return err
	}
	transport := goClientRoundTrip(func(outbound *http.Request) (*http.Response, error) {
		if outbound.Header.Get("Content-Type") != client.RequestMediaType || outbound.Header.Get("Authorization") != "Bearer profile" {
			return nil, errors.New("client request headers mismatch")
		}
		body := `{"requestId":"profile","data":{"ok":true},"errors":[{"code":"PARTIAL","message":"partial","path":["later"],"retryable":false}],"capabilities":[],"extensions":{}}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{client.ResponseMediaType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	httpClient, err := client.New(client.Config{
		Endpoint: "https://example.invalid/v1/execute", HTTPClient: &http.Client{Transport: transport},
		MaxCompressedBytes: fixture.Transport.CompressedBytes, MaxDecompressedBytes: fixture.Transport.DecompressedBytes,
		Authenticate: func(_ context.Context, request *http.Request) error {
			request.Header.Set("Authorization", "Bearer profile")
			return nil
		},
	})
	if err != nil {
		return err
	}
	result, err := httpClient.Execute(ctx, request)
	if err != nil {
		return err
	}
	data, present := result.Data()
	if !present || len(result.Errors()) != 1 || !bytes.Equal(data, []byte(`{"ok":true}`)) {
		return errors.New("partial response was not preserved")
	}
	return nil
}

type goClientRoundTrip func(*http.Request) (*http.Response, error)

func (function goClientRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

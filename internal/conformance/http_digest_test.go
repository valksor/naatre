package conformance_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/valksor/naatre/protocol"
	transporthttp "github.com/valksor/naatre/transport/http"
)

type httpDigestFixture struct {
	Profile        string `json:"profile"`
	Specifications struct {
		DigestFields     string `json:"digestFields"`
		StructuredFields string `json:"structuredFields"`
	} `json:"specifications"`
	Fields []string `json:"fields"`
	Policy struct {
		AllowedAlgorithms               []string `json:"allowedAlgorithms"`
		DefaultAlgorithm                string   `json:"defaultAlgorithm"`
		EqualWeightPreference           []string `json:"equalWeightPreference"`
		MaximumFieldBytes               int      `json:"maximumFieldBytes"`
		VerifyEveryPresentAllowedDigest bool     `json:"verifyEveryPresentAllowedDigest"`
		DigestFieldsInHeaders           bool     `json:"digestFieldsInHeaders"`
		TrailerVerificationSupported    bool     `json:"trailerVerificationSupported"`
		RequiresTrailerPreservation     bool     `json:"requiresTrailerPreservation"`
	} `json:"policy"`
	ExactByteVectors []struct {
		Name            string `json:"name"`
		Status          int    `json:"status"`
		ContentCoding   string `json:"contentCoding"`
		TransferFraming string `json:"transferFraming"`
		ContentRange    string `json:"contentRange"`
		Representation  []byte `json:"representationBase64"`
		Content         []byte `json:"contentBase64"`
		FramedBody      []byte `json:"framedBodyBase64"`
		ContentDigest   string `json:"contentDigest"`
		ReprDigest      string `json:"reprDigest"`
	} `json:"exactByteVectors"`
	NegotiationCases []struct {
		Name      string   `json:"name"`
		Fields    []string `json:"fields"`
		Algorithm string   `json:"algorithm"`
		Accepted  bool     `json:"accepted"`
		Code      string   `json:"code"`
	} `json:"negotiationCases"`
	FailureCases []struct {
		Name           string   `json:"name"`
		Fields         []string `json:"fields"`
		Input          []byte   `json:"inputBase64"`
		MaximumBytes   int64    `json:"maximumBytes"`
		ExpectedLength int64    `json:"expectedLength"`
		Code           string   `json:"code"`
	} `json:"failureCases"`
	IntegrityOutcomes []struct {
		Name           string `json:"name"`
		Content        string `json:"content"`
		Representation string `json:"representation"`
		Gate           string `json:"gate"`
		Outcome        string `json:"outcome"`
	} `json:"integrityOutcomes"`
	Webhook struct {
		Body                []byte   `json:"bodyBase64"`
		ContentDigest       string   `json:"contentDigest"`
		CoveredComponents   []string `json:"coveredComponents"`
		SignatureBase       []byte   `json:"signatureBaseBase64"`
		SignatureBaseSHA256 string   `json:"signatureBaseSHA256"`
		Modifications       []struct {
			Name       string `json:"name"`
			Find       string `json:"find"`
			Replace    string `json:"replace"`
			DetectedBy string `json:"detectedBy"`
		} `json:"modifications"`
	} `json:"webhook"`
	SemanticIdentity struct {
		TransportCases      []httpDigestSemanticIdentityCase `json:"transportCases"`
		ChangedSemanticCase httpDigestSemanticIdentityCase   `json:"changedSemanticCase"`
	} `json:"semanticIdentity"`
}

type httpDigestSemanticIdentityCase struct {
	Name             string          `json:"name"`
	SemanticInput    string          `json:"semanticInput"`
	ContentCoding    string          `json:"contentCoding"`
	Content          []byte          `json:"contentBase64"`
	ContentDigest    string          `json:"contentDigest"`
	ExpectedIdentity protocol.Digest `json:"expectedIdentity"`
}

func TestHTTPDigestFixtureExactBytes(t *testing.T) {
	t.Parallel()
	fixture := loadHTTPDigestFixture(t)
	expectedNames := []string{"identity-json-response", "gzip-json-response", "range-response", "chunked-transfer-response"}
	for index, vector := range fixture.ExactByteVectors {
		if index >= len(expectedNames) || vector.Name != expectedNames[index] {
			t.Fatalf("exact byte vector %d = %q", index, vector.Name)
		}
		representation := vector.Representation
		content := vector.Content
		framed := vector.FramedBody
		assertDigestField(t, content, vector.ContentDigest)
		assertDigestField(t, representation, vector.ReprDigest)

		switch vector.Name {
		case "identity-json-response":
			if !bytes.Equal(content, representation) || !bytes.Equal(content, framed) {
				t.Fatal("identity bytes differ")
			}
		case "gzip-json-response":
			reader, err := gzip.NewReader(bytes.NewReader(content))
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := io.ReadAll(reader)
			if err != nil || reader.Close() != nil || !bytes.Equal(decoded, representation) {
				t.Fatalf("gzip representation = %q, %v", decoded, err)
			}
			if vector.ContentDigest == vector.ReprDigest {
				t.Fatal("gzip content and representation digests were interchanged")
			}
		case "range-response":
			if vector.Status != 206 || vector.ContentRange != "bytes 10-19/36" || !bytes.Equal(content, representation[10:20]) || vector.ContentDigest == vector.ReprDigest {
				t.Fatal("range vector does not bind partial content and full representation")
			}
		case "chunked-transfer-response":
			if bytes.Equal(framed, content) {
				t.Fatal("chunked vector does not distinguish transfer framing")
			}
			if _, err := transporthttp.VerifyDigestTo(io.Discard, bytes.NewReader(framed), []string{vector.ContentDigest}, transporthttp.VerifyOptions{MaximumBytes: int64(len(framed)), ExpectedLength: int64(len(framed))}); !errors.Is(err, transporthttp.ErrDigestMismatch) {
				t.Fatalf("transfer framing was covered by Content-Digest: %v", err)
			}
		}
	}
	if len(fixture.ExactByteVectors) != len(expectedNames) {
		t.Fatalf("exact byte vectors = %d, want %d", len(fixture.ExactByteVectors), len(expectedNames))
	}
}

func TestHTTPDigestFixtureNegotiationAndFailures(t *testing.T) {
	t.Parallel()
	fixture := loadHTTPDigestFixture(t)
	for _, test := range fixture.NegotiationCases {
		t.Run(test.Name, func(t *testing.T) {
			algorithm, err := transporthttp.NegotiateDigestAlgorithm(test.Fields)
			if test.Accepted {
				if err != nil || string(algorithm) != test.Algorithm {
					t.Fatalf("negotiation = %q, %v", algorithm, err)
				}
				return
			}
			if digestErrorCode(err) != test.Code {
				t.Fatalf("negotiation error = %v, want %s", err, test.Code)
			}
		})
	}
	for _, test := range fixture.FailureCases {
		t.Run(test.Name, func(t *testing.T) {
			var staged bytes.Buffer
			_, err := transporthttp.VerifyDigestTo(&staged, bytes.NewReader(test.Input), test.Fields, transporthttp.VerifyOptions{MaximumBytes: test.MaximumBytes, ExpectedLength: test.ExpectedLength})
			if digestErrorCode(err) != test.Code {
				t.Fatalf("verification error = %v, want %s", err, test.Code)
			}
		})
	}
}

func TestHTTPDigestFixtureOutcomesAndWebhookBinding(t *testing.T) {
	t.Parallel()
	fixture := loadHTTPDigestFixture(t)
	wantOutcomes := []string{"range", "multipart", "resumable", "streamed", "truncated", "proxy-transformed", "redirect", "cache"}
	for index, outcome := range fixture.IntegrityOutcomes {
		if index >= len(wantOutcomes) || outcome.Name != wantOutcomes[index] || outcome.Content == "" || outcome.Representation == "" || outcome.Gate == "" || outcome.Outcome == "" {
			t.Fatalf("integrity outcome %d = %#v", index, outcome)
		}
	}
	if len(fixture.IntegrityOutcomes) != len(wantOutcomes) {
		t.Fatalf("integrity outcomes = %d, want %d", len(fixture.IntegrityOutcomes), len(wantOutcomes))
	}

	body := fixture.Webhook.Body
	assertDigestField(t, body, fixture.Webhook.ContentDigest)
	signatureBase := fixture.Webhook.SignatureBase
	digest := sha256.Sum256(signatureBase)
	if base64.StdEncoding.EncodeToString(digest[:]) != fixture.Webhook.SignatureBaseSHA256 {
		t.Fatal("webhook signature input digest mismatch")
	}
	wantComponents := []string{"@method", "@target-uri", "content-type", "content-digest", "naatre-webhook-id", "naatre-webhook-timestamp"}
	if !slices.Equal(fixture.Webhook.CoveredComponents, wantComponents) {
		t.Fatalf("webhook covered components = %v", fixture.Webhook.CoveredComponents)
	}
	for _, modification := range fixture.Webhook.Modifications {
		switch modification.DetectedBy {
		case "signature":
			changed := []byte(strings.Replace(string(signatureBase), modification.Find, modification.Replace, 1))
			if bytes.Equal(changed, signatureBase) || sha256.Sum256(changed) == digest {
				t.Fatalf("webhook modification %q was not detected", modification.Name)
			}
		case "content-digest":
			changed := []byte(strings.Replace(string(body), modification.Find, modification.Replace, 1))
			if _, err := transporthttp.VerifyDigestTo(io.Discard, bytes.NewReader(changed), []string{fixture.Webhook.ContentDigest}, transporthttp.VerifyOptions{MaximumBytes: int64(len(changed)), ExpectedLength: int64(len(changed))}); !errors.Is(err, transporthttp.ErrDigestMismatch) {
				t.Fatalf("webhook body modification was not detected: %v", err)
			}
		default:
			t.Fatalf("unknown webhook detection phase %q", modification.DetectedBy)
		}
	}
}

func TestHTTPDigestFixtureSeparatesTransportAndSemanticIdentity(t *testing.T) {
	t.Parallel()
	fixture := loadHTTPDigestFixture(t)
	cases := fixture.SemanticIdentity.TransportCases
	if len(cases) != 2 || cases[0].Name != "identity-content" || cases[1].Name != "gzip-content" || cases[0].ContentDigest == cases[1].ContentDigest {
		t.Fatal("semantic identity fixture does not vary transport digests")
	}
	left := verifySemanticIdentityCase(t, cases[0])
	right := verifySemanticIdentityCase(t, cases[1])
	if left != right || left != cases[0].ExpectedIdentity || right != cases[1].ExpectedIdentity {
		t.Fatalf("transport metadata altered semantic identity: %#v, %#v", left, right)
	}
	changedDigest := verifySemanticIdentityCase(t, fixture.SemanticIdentity.ChangedSemanticCase)
	if changedDigest == left {
		t.Fatal("changed semantic input retained operation identity")
	}
}

func verifySemanticIdentityCase(t *testing.T, test httpDigestSemanticIdentityCase) protocol.Digest {
	t.Helper()
	assertDigestField(t, test.Content, test.ContentDigest)
	representation := test.Content
	switch test.ContentCoding {
	case "identity":
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(test.Content))
		if err != nil {
			t.Fatal(err)
		}
		representation, err = io.ReadAll(reader)
		if err != nil || reader.Close() != nil {
			t.Fatalf("decode semantic identity content: %v", err)
		}
	default:
		t.Fatalf("unknown semantic identity content coding %q", test.ContentCoding)
	}
	if string(representation) != test.SemanticInput {
		t.Fatal("semantic input does not match decoded representation")
	}
	canonical, err := protocol.CanonicalizeJSON(representation, protocol.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	actual, err := protocol.SemanticHash(protocol.DocumentHash, canonical)
	if err != nil || actual != test.ExpectedIdentity {
		t.Fatalf("semantic identity = %#v, %v; want %#v", actual, err, test.ExpectedIdentity)
	}
	return actual
}

func loadHTTPDigestFixture(t *testing.T) httpDigestFixture {
	t.Helper()
	var fixture httpDigestFixture
	readFixture(t, "http-digest.json", &fixture)
	if fixture.Profile != transporthttp.DigestProfile || fixture.Specifications.DigestFields != "RFC 9530" || fixture.Specifications.StructuredFields != "RFC 9651" {
		t.Fatalf("HTTP digest fixture header = %#v", fixture)
	}
	if !slices.Equal(fixture.Fields, []string{"Content-Digest", "Repr-Digest", "Want-Content-Digest", "Want-Repr-Digest"}) ||
		!slices.Equal(fixture.Policy.AllowedAlgorithms, []string{"sha-512", "sha-256"}) ||
		fixture.Policy.DefaultAlgorithm != "sha-256" || !slices.Equal(fixture.Policy.EqualWeightPreference, []string{"sha-512", "sha-256"}) ||
		fixture.Policy.MaximumFieldBytes != transporthttp.MaximumDigestFieldBytes || !fixture.Policy.VerifyEveryPresentAllowedDigest ||
		!fixture.Policy.DigestFieldsInHeaders || fixture.Policy.TrailerVerificationSupported || fixture.Policy.RequiresTrailerPreservation ||
		fixture.Policy.TrailerVerificationSupported != transporthttp.TrailerDigestVerificationSupported {
		t.Fatalf("HTTP digest policy = %#v", fixture.Policy)
	}
	return fixture
}

func assertDigestField(t *testing.T, content []byte, field string) {
	t.Helper()
	algorithms := []transporthttp.DigestAlgorithm{transporthttp.SHA256}
	if strings.Contains(field, "sha-512") {
		algorithms = []transporthttp.DigestAlgorithm{transporthttp.SHA512, transporthttp.SHA256}
	}
	generated, err := transporthttp.FormatDigestField(content, algorithms...)
	if err != nil || generated != field {
		t.Fatalf("digest field = %q, %v; want %q", generated, err, field)
	}
	if _, err := transporthttp.VerifyDigestTo(io.Discard, bytes.NewReader(content), []string{field}, transporthttp.VerifyOptions{MaximumBytes: int64(len(content)), ExpectedLength: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
}

var digestErrorCodes = []struct {
	target error
	code   string
}{
	{transporthttp.ErrDigestRequired, "DIGEST_REQUIRED"},
	{transporthttp.ErrDuplicateDigestField, "DUPLICATE_DIGEST_FIELD"},
	{transporthttp.ErrDuplicateDigestAlgorithm, "DUPLICATE_DIGEST_ALGORITHM"},
	{transporthttp.ErrUnsupportedDigestAlgorithm, "UNSUPPORTED_DIGEST_ALGORITHM"},
	{transporthttp.ErrMalformedDigest, "MALFORMED_DIGEST"},
	{transporthttp.ErrNoAcceptableDigest, "NO_ACCEPTABLE_DIGEST"},
	{transporthttp.ErrDigestLimitExceeded, "DIGEST_LIMIT_EXCEEDED"},
	{transporthttp.ErrDigestTruncated, "DIGEST_TRUNCATED"},
	{transporthttp.ErrDigestLengthMismatch, "DIGEST_LENGTH_MISMATCH"},
	{transporthttp.ErrDigestMismatch, "DIGEST_MISMATCH"},
}

func digestErrorCode(cause error) string {
	for _, candidate := range digestErrorCodes {
		if errors.Is(cause, candidate.target) {
			return candidate.code
		}
	}
	return ""
}

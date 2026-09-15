package conformance_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"slices"
	"testing"

	"github.com/valksor/naatre/largevalue"
)

type largeValueFixture struct {
	Profile      string          `json:"profile"`
	Binding      json.RawMessage `json:"binding"`
	Capabilities json.RawMessage `json:"capabilities"`
	Lifecycle    json.RawMessage `json:"lifecycle"`
	Transfer     json.RawMessage `json:"transfer"`
	Boundary     struct {
		SmallCanonicalMaximumBytes  int  `json:"smallCanonicalMaximumBytes"`
		LargeValuesUseDeclaredSlots bool `json:"largeValuesUseDeclaredSlots"`
		CoreBuffersCompleteContent  bool `json:"coreBuffersCompleteContent"`
	} `json:"boundary"`
	RangeCases []struct {
		Name         string `json:"name"`
		Value        string `json:"value"`
		Total        int64  `json:"total"`
		Accepted     bool   `json:"accepted"`
		Status       int    `json:"status"`
		Start        int64  `json:"start"`
		End          int64  `json:"end"`
		ContentRange string `json:"contentRange"`
		Code         string `json:"code"`
	} `json:"rangeCases"`
	ConditionalCases []struct {
		Name          string `json:"name"`
		Method        string `json:"method"`
		ETag          string `json:"etag"`
		IfMatch       string `json:"ifMatch"`
		IfNoneMatch   string `json:"ifNoneMatch"`
		IfRange       string `json:"ifRange"`
		Range         string `json:"range"`
		Status        int    `json:"status"`
		ContentLength int64  `json:"contentLength"`
	} `json:"conditionalCases"`
	ResumeCases []struct {
		Name              string `json:"name"`
		Offset            int64  `json:"offset"`
		ChunkLength       int64  `json:"chunkLength"`
		TotalLength       int64  `json:"totalLength"`
		Validator         string `json:"validator"`
		ExpectedValidator string `json:"expectedValidator"`
		Accepted          bool   `json:"accepted"`
	} `json:"resumeCases"`
	EgressCases []struct {
		Name               string     `json:"name"`
		URL                string     `json:"url"`
		Addresses          []string   `json:"addresses"`
		Chain              []string   `json:"chain"`
		ResolutionSequence [][]string `json:"resolutionSequence"`
		Accepted           bool       `json:"accepted"`
		AcceptedSequence   []bool     `json:"acceptedSequence"`
	} `json:"egressCases"`
	SecurityCases []map[string]any `json:"securityCases"`
}

func TestLargeValueTransportFixture(t *testing.T) {
	t.Parallel()
	fixture := loadLargeValueFixture(t)
	if fixture.Profile != largevalue.Profile || fixture.Boundary.SmallCanonicalMaximumBytes != largevalue.DefaultSmallByteLimit ||
		!fixture.Boundary.LargeValuesUseDeclaredSlots || fixture.Boundary.CoreBuffersCompleteContent {
		t.Fatalf("large value boundary = %#v", fixture.Boundary)
	}
	for _, test := range fixture.RangeCases {
		t.Run(test.Name, func(t *testing.T) {
			selected, err := largevalue.ParseByteRange(test.Value, test.Total)
			if !test.Accepted {
				if !errors.Is(err, largevalue.ErrRangeUnsatisfied) || test.Status != 416 {
					t.Fatalf("range = %#v, %v", selected, err)
				}
				return
			}
			if err != nil || selected.Start != test.Start || selected.End != test.End || selected.ContentRange() != test.ContentRange {
				t.Fatalf("range = %#v, %v", selected, err)
			}
		})
	}
	for _, test := range fixture.ConditionalCases {
		t.Run(test.Name, func(t *testing.T) {
			decision, err := largevalue.EvaluateDownloadRequest(largevalue.DownloadRequest{Method: test.Method, Range: test.Range,
				IfMatch: test.IfMatch, IfNoneMatch: test.IfNoneMatch, IfRange: test.IfRange, Length: 36, ETag: test.ETag})
			if err != nil || decision.Status != test.Status || decision.ContentLength != test.ContentLength || decision.FollowRedirect {
				t.Fatalf("conditional decision = %#v, %v", decision, err)
			}
		})
	}
	for _, test := range fixture.ResumeCases {
		t.Run(test.Name, func(t *testing.T) {
			err := largevalue.ValidateResume(largevalue.ResumeRequest{Offset: test.Offset, ChunkLength: test.ChunkLength,
				TotalLength: test.TotalLength, Validator: test.Validator, ExpectedValidator: test.ExpectedValidator})
			if (err == nil) != test.Accepted {
				t.Fatalf("resume accepted = %v, want %v", err == nil, test.Accepted)
			}
		})
	}
}

func TestLargeValueEgressAndSecurityFixture(t *testing.T) {
	t.Parallel()
	fixture := loadLargeValueFixture(t)
	for _, test := range fixture.EgressCases {
		t.Run(test.Name, func(t *testing.T) {
			answers := test.ResolutionSequence
			if len(answers) == 0 {
				answers = [][]string{test.Addresses}
			}
			resolver := &fixtureResolver{answers: answers}
			policy := largevalue.EgressPolicy{Resolver: resolver, AllowedOrigins: []string{"https://upload.example.test:443"}, AllowedPorts: []uint16{443}}
			if len(test.Chain) > 0 {
				err := policy.ValidateRedirectChain(context.Background(), test.Chain)
				if (err == nil) != test.Accepted {
					t.Fatalf("redirect accepted = %v, want %v", err == nil, test.Accepted)
				}
				return
			}
			if len(test.AcceptedSequence) > 0 {
				for index, expected := range test.AcceptedSequence {
					_, err := policy.ValidateHop(context.Background(), test.URL)
					if (err == nil) != expected {
						t.Fatalf("resolution %d accepted = %v, want %v", index, err == nil, expected)
					}
				}
				return
			}
			_, err := policy.ValidateHop(context.Background(), test.URL)
			if (err == nil) != test.Accepted {
				t.Fatalf("URL accepted = %v, want %v", err == nil, test.Accepted)
			}
		})
	}
	wantSecurity := []string{"path-traversal", "windows-path", "media-type-confusion", "negative-length", "compression-bomb",
		"missing-digest", "digest-mismatch", "wrong-tenant", "expired", "revoked"}
	actual := make([]string, 0, len(fixture.SecurityCases))
	for _, test := range fixture.SecurityCases {
		name, _ := test["name"].(string)
		actual = append(actual, name)
	}
	if !slices.Equal(actual, wantSecurity) {
		t.Fatalf("security cases = %v, want %v", actual, wantSecurity)
	}
}

func loadLargeValueFixture(t *testing.T) largeValueFixture {
	t.Helper()
	var fixture largeValueFixture
	readFixture(t, "large-values.json", &fixture)
	return fixture
}

type fixtureResolver struct{ answers [][]string }

func (r *fixtureResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	if len(r.answers) == 0 {
		return nil, errors.New("fixture DNS answer exhausted")
	}
	answer := r.answers[0]
	r.answers = r.answers[1:]
	addresses := make([]netip.Addr, 0, len(answer))
	for _, value := range answer {
		addresses = append(addresses, netip.MustParseAddr(value))
	}
	return addresses, nil
}

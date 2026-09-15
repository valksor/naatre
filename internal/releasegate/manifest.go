package releasegate

import (
	"encoding/json"
	"fmt"
)

type releaseManifest struct {
	Profile string `json:"profile"`
	Release struct {
		ID             string `json:"id"`
		SourceRevision string `json:"sourceRevision"`
	} `json:"release"`
	Support []struct {
		Component string     `json:"component"`
		Status    string     `json:"status"`
		Evidence  []Evidence `json:"evidence"`
	} `json:"support"`
	ConformanceClaims []struct {
		Component string   `json:"component"`
		Version   string   `json:"version"`
		Profile   string   `json:"profile"`
		Report    Evidence `json:"report"`
	} `json:"conformanceClaims"`
}

// LinkManifest returns a manifest that cites an exact passing aggregate. It
// refuses to advertise planned-only support and never mutates the template.
func LinkManifest(template []byte, aggregate Report, aggregateEvidence Evidence) ([]byte, error) {
	if aggregate.Status != "passed" || len(aggregate.Failures) != 0 || !digestPattern.MatchString(aggregateEvidence.SHA256) || aggregateEvidence.Path == "" {
		return nil, fmt.Errorf("release gate has no passing aggregate evidence")
	}
	var semantic releaseManifest
	if err := json.Unmarshal(template, &semantic); err != nil {
		return nil, fmt.Errorf("decode release manifest: %w", err)
	}
	if semantic.Profile != "naatre.release-manifest-1" || semantic.Release.ID == "" || semantic.Release.SourceRevision != aggregate.SourceRevision {
		return nil, fmt.Errorf("release manifest identity or source revision does not match the aggregate")
	}
	knownEvidence := make(map[Evidence]struct{}, len(aggregate.Inputs))
	for _, evidence := range aggregate.Inputs {
		knownEvidence[evidence] = struct{}{}
	}
	implemented := make(map[string]struct{})
	for _, row := range semantic.Support {
		if row.Status == "implemented" && len(row.Evidence) == 0 {
			return nil, fmt.Errorf("implemented support row %s lacks evidence", row.Component)
		}
		if row.Status != "implemented" && len(row.Evidence) != 0 {
			return nil, fmt.Errorf("planned-only support row %s advertises evidence", row.Component)
		}
		if row.Status == "implemented" {
			implemented[row.Component] = struct{}{}
			for _, evidence := range row.Evidence {
				if _, ok := knownEvidence[evidence]; !ok {
					return nil, fmt.Errorf("implemented support row %s cites evidence outside the release gate", row.Component)
				}
			}
		}
	}
	for _, claim := range semantic.ConformanceClaims {
		if claim.Profile == "release.stable-1" {
			return nil, fmt.Errorf("release manifest already contains a stable release-gate claim")
		}
		if claim.Report.Path == "" || !digestPattern.MatchString(claim.Report.SHA256) {
			return nil, fmt.Errorf("conformance claim %s lacks exact report evidence", claim.Profile)
		}
		if _, ok := implemented[claim.Component]; !ok {
			return nil, fmt.Errorf("conformance claim %s advertises a component without implemented support", claim.Profile)
		}
		if _, ok := knownEvidence[claim.Report]; !ok {
			return nil, fmt.Errorf("conformance claim %s cites evidence outside the release gate", claim.Profile)
		}
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal(template, &document); err != nil {
		return nil, err
	}
	claims := append(semantic.ConformanceClaims, struct {
		Component string   `json:"component"`
		Version   string   `json:"version"`
		Profile   string   `json:"profile"`
		Report    Evidence `json:"report"`
	}{Component: "naatre-release-gate", Version: aggregate.Versions.Gate, Profile: "release.stable-1", Report: aggregateEvidence})
	encodedClaims, err := json.Marshal(claims)
	if err != nil {
		return nil, err
	}
	document["conformanceClaims"] = encodedClaims
	output, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(output, '\n'), nil
}

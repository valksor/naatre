package asyncapi

import (
	"reflect"
	"sort"

	"github.com/valksor/naatre/schema"
)

// Diff compares event-description declarations by stable identity. Its
// classifications use the same vocabulary and strongest-change ordering as
// schema.DiffDocuments.
func Diff(before, after Model) CompatibilityReport {
	result := CompatibilityReport{BeforeRevision: before.Revisions.Schema, AfterRevision: after.Revisions.Schema}
	if result.BeforeRevision == "" {
		result.BeforeRevision = before.Schema.Revision()
	}
	if result.AfterRevision == "" {
		result.AfterRevision = after.Schema.Revision()
	}
	diffDomain(&result, "message", before.Messages, after.Messages, func(value Message) string { return value.ID }, diffMessage)
	diffDomain(&result, "channel", before.Channels, after.Channels, func(value Channel) string { return value.ID }, diffChannel)
	diffDomain(&result, "binding", before.Bindings, after.Bindings, func(value Binding) string { return value.ID }, diffBinding)
	diffDomain(&result, "security", before.Security, after.Security, func(value SecurityScheme) string { return value.ID }, diffSecurity)
	if before.Schema.Revision() != "" && after.Schema.Revision() != "" {
		for _, change := range schema.DiffDocuments(before.Schema, after.Schema).Changes {
			result.Changes = append(result.Changes, Change{Domain: "schema", ID: change.Path, Path: change.Path, Classification: change.Classification})
		}
	}
	sort.Slice(result.Changes, func(i, j int) bool {
		return result.Changes[i].Domain+"\x00"+result.Changes[i].ID+"\x00"+result.Changes[i].Path < result.Changes[j].Domain+"\x00"+result.Changes[j].ID+"\x00"+result.Changes[j].Path
	})
	for _, change := range result.Changes {
		result.Classification = strongest(result.Classification, change.Classification)
	}
	return result
}

func diffDomain[T any](report *CompatibilityReport, domain string, before, after []T, identity func(T) string, compare func(*CompatibilityReport, T, T)) {
	left := make(map[string]T, len(before))
	right := make(map[string]T, len(after))
	for _, value := range before {
		left[identity(value)] = value
	}
	for _, value := range after {
		right[identity(value)] = value
	}
	for id, value := range left {
		other, ok := right[id]
		if !ok {
			report.add(domain, id, "/", schema.ChangeBreaking)
			continue
		}
		compare(report, value, other)
	}
	for id := range right {
		if _, ok := left[id]; !ok {
			report.add(domain, id, "/", schema.ChangeAdditive)
		}
	}
}

func diffMessage(report *CompatibilityReport, before, after Message) {
	if before.NaatreID != after.NaatreID || before.Revision != after.Revision {
		report.add("message", before.ID, "/identity", schema.ChangeBreaking)
	}
	if before.PayloadType != after.PayloadType || before.ContentType != after.ContentType {
		report.add("message", before.ID, "/payload", schema.ChangeBreaking)
	}
	if !reflect.DeepEqual(before.Correlations, after.Correlations) {
		report.add("message", before.ID, "/correlations", schema.ChangeDangerous)
	}
	if !reflect.DeepEqual(before.Examples, after.Examples) {
		report.add("message", before.ID, "/examples", schema.ChangeBehaviorOnly)
	}
}

func diffChannel(report *CompatibilityReport, before, after Channel) {
	if before.NaatreID != after.NaatreID || before.Revision != after.Revision || before.Address != after.Address {
		report.add("channel", before.ID, "/identity-or-address", schema.ChangeBreaking)
	}
	if !reflect.DeepEqual(sorted(before.Messages), sorted(after.Messages)) {
		report.add("channel", before.ID, "/messages", schema.ChangeBreaking)
	}
	if !reflect.DeepEqual(sorted(before.Bindings), sorted(after.Bindings)) {
		report.add("channel", before.ID, "/bindings", schema.ChangeDangerous)
	}
	if !reflect.DeepEqual(before.Semantics, after.Semantics) {
		report.add("channel", before.ID, "/semantics", schema.ChangeBreaking)
	}
}

func diffBinding(report *CompatibilityReport, before, after Binding) {
	if before.NaatreID != after.NaatreID || before.Revision != after.Revision || before.Transport != after.Transport || before.Server != after.Server || before.Implemented != after.Implemented || before.WireCompatibility != after.WireCompatibility {
		report.add("binding", before.ID, "/contract", schema.ChangeBreaking)
	}
	if !reflect.DeepEqual(sorted(before.Evidence), sorted(after.Evidence)) {
		report.add("binding", before.ID, "/evidence", schema.ChangeBehaviorOnly)
	}
}

func diffSecurity(report *CompatibilityReport, before, after SecurityScheme) {
	if before.NaatreID != after.NaatreID || before.Revision != after.Revision || before.Type != after.Type || before.Scheme != after.Scheme || before.Name != after.Name || before.In != after.In {
		report.add("security", before.ID, "/contract", schema.ChangeDangerous)
	}
}

func (r *CompatibilityReport) add(domain, id, path string, classification schema.ChangeClassification) {
	r.Changes = append(r.Changes, Change{Domain: domain, ID: id, Path: path, Classification: classification})
}

func strongest(left, right schema.ChangeClassification) schema.ChangeClassification {
	rank := map[schema.ChangeClassification]int{schema.ChangeAdditive: 1, schema.ChangeBehaviorOnly: 2, schema.ChangeDangerous: 3, schema.ChangeBreaking: 4}
	if rank[right] > rank[left] {
		return right
	}
	return left
}

func sorted(input []string) []string {
	result := append([]string(nil), input...)
	sort.Strings(result)
	return result
}

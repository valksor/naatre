package asyncapi

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sort"
)

// CanonicalJSON returns the byte-stable source for an imported model or
// exports a caller-constructed model through the pinned exporter.
func CanonicalJSON(model Model) ([]byte, error) {
	if len(model.canonical) != 0 {
		return slices.Clone(model.canonical), nil
	}
	content, _, err := Export(model)
	if err != nil {
		return nil, err
	}
	if len(content) == 0 {
		return nil, errors.New("AsyncAPI model has no canonical representation")
	}
	return content, nil
}

// Inspect returns a side-effect-free playground/editor projection. Model has
// no business-handler field, so inspection cannot invoke application code.
func Inspect(model Model) InspectionView {
	view := InspectionView{Profile: Profile, ModelDigest: modelDigest(model), SchemaRevision: model.Revisions.Schema}
	for _, value := range model.Channels {
		view.Channels = append(view.Channels, value.ID)
	}
	for _, value := range model.Messages {
		view.Messages = append(view.Messages, value.ID)
	}
	for _, value := range model.Operations {
		view.Operations = append(view.Operations, value.ID)
	}
	sort.Strings(view.Channels)
	sort.Strings(view.Messages)
	sort.Strings(view.Operations)
	return view
}

// Documentation derives a deterministic human-oriented summary from the same
// imported model used by inspection and mocks.
func Documentation(model Model) DocumentationView {
	inspection := Inspect(model)
	summary := []string{
		fmt.Sprintf("schema %s", inspection.SchemaRevision),
		fmt.Sprintf("channels %d", len(inspection.Channels)),
		fmt.Sprintf("messages %d", len(inspection.Messages)),
		fmt.Sprintf("operations %d", len(inspection.Operations)),
	}
	return DocumentationView{Profile: Profile, ModelDigest: inspection.ModelDigest, Summary: summary}
}

// Mock derives deterministic logical frame names only. It is fixture evidence,
// never evidence of a compatible broker, transport, or business handler.
func Mock(model Model, seed uint64) MockView {
	digest := modelDigest(model)
	frames := []string{"open"}
	messageIDs := make([]string, 0, len(model.Messages))
	for _, message := range model.Messages {
		messageIDs = append(messageIDs, message.ID)
	}
	sort.Strings(messageIDs)
	if len(messageIDs) != 0 {
		rotation := int(seed % uint64(len(messageIDs)))
		messageIDs = append(slices.Clone(messageIDs[rotation:]), messageIDs[:rotation]...)
		for _, message := range messageIDs {
			frames = append(frames, "next:"+message)
		}
	}
	frames = append(frames, "complete")
	return MockView{
		Profile: Profile, ModelDigest: digest, Seed: seed, Frames: frames,
		Evidence: "fixture-only; no AsyncAPI wire compatibility or business behavior is claimed",
	}
}

func modelDigest(model Model) string {
	content := model.canonical
	if len(content) == 0 {
		if exported, _, err := Export(model); err == nil {
			content = exported
		}
	}
	sum := sha256.Sum256(content)
	return fmt.Sprintf("sha256:%x", sum)
}

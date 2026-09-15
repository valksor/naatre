package largevalue

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/valksor/naatre/protocol"
)

// OperationIdentity excludes payload bytes and resolved references while
// binding the canonical document and declared payload-slot contract.
func OperationIdentity(document json.RawMessage, slots []SlotDeclaration) (protocol.Digest, error) {
	declarations := append([]SlotDeclaration(nil), slots...)
	slices.SortFunc(declarations, func(left, right SlotDeclaration) int { return strings.Compare(left.Name, right.Name) })
	for index := range declarations {
		if err := validateSlot(declarations[index]); err != nil {
			return protocol.Digest{}, err
		}
		declarations[index].AllowedMediaTypes = append([]string(nil), declarations[index].AllowedMediaTypes...)
		slices.Sort(declarations[index].AllowedMediaTypes)
		if index > 0 && declarations[index-1].Name == declarations[index].Name {
			return protocol.Digest{}, fmt.Errorf("duplicate payload slot %q", declarations[index].Name)
		}
	}
	payload, err := json.Marshal(struct {
		Document json.RawMessage   `json:"document"`
		Slots    []SlotDeclaration `json:"payloadSlots"`
	}{Document: document, Slots: declarations})
	if err != nil {
		return protocol.Digest{}, fmt.Errorf("marshal payload operation identity: %w", err)
	}
	return semanticDigest(protocol.DocumentHash, payload)
}

// ContentIdentity binds finalized reference and digest metadata into the
// idempotency domain so distinct bytes cannot share a protected result.
func ContentIdentity(operation protocol.Digest, references []ResolvedReference) (protocol.Digest, error) {
	resolved := append([]ResolvedReference(nil), references...)
	slices.SortFunc(resolved, func(left, right ResolvedReference) int { return strings.Compare(left.Slot, right.Slot) })
	for index, reference := range resolved {
		if reference.Slot == "" || reference.Reference == "" || reference.MediaType == "" || reference.Length < 0 || reference.Digest == "" {
			return protocol.Digest{}, errors.New("invalid resolved payload reference")
		}
		if index > 0 && resolved[index-1].Slot == reference.Slot {
			return protocol.Digest{}, fmt.Errorf("duplicate resolved payload slot %q", reference.Slot)
		}
	}
	payload, err := json.Marshal(struct {
		Operation  protocol.Digest     `json:"operation"`
		References []ResolvedReference `json:"payloadReferences"`
	}{Operation: operation, References: resolved})
	if err != nil {
		return protocol.Digest{}, fmt.Errorf("marshal payload content identity: %w", err)
	}
	return semanticDigest(protocol.IdempotencyHash, payload)
}

func semanticDigest(purpose protocol.HashPurpose, payload []byte) (protocol.Digest, error) {
	canonical, err := protocol.CanonicalizeHashPayload(purpose, payload, protocol.DefaultLimits())
	if err != nil {
		return protocol.Digest{}, err
	}
	return protocol.SemanticHash(purpose, canonical)
}

func validateSlot(slot SlotDeclaration) error {
	if strings.TrimSpace(slot.Name) == "" || slot.MaximumLength <= DefaultSmallByteLimit {
		return errors.New("large payload slot requires a name and maximum above the canonical byte limit")
	}
	if slot.Direction != Upload && slot.Direction != Download {
		return errors.New("invalid payload slot direction")
	}
	switch slot.Profile {
	case DirectProfile, PresignedProfile, MultipartProfile, ResumableProfile, ApplicationProfile:
	default:
		return errors.New("invalid payload transfer profile")
	}
	if len(slot.AllowedMediaTypes) == 0 {
		return errors.New("payload slot requires allowed media types")
	}
	return nil
}

package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
)

var ErrInvalidStreamAdvertisement = errors.New("invalid stream source advertisement")

type StreamReplayCapability string

const (
	StreamReplayNone    StreamReplayCapability = "none"
	StreamReplayBounded StreamReplayCapability = "bounded"
	StreamReplayDurable StreamReplayCapability = "durable"
)

type StreamReadConsistency string

const (
	StreamSnapshotStable StreamReadConsistency = "snapshot-stable"
	StreamLiveBestEffort StreamReadConsistency = "live-best-effort"
)

// StreamSourceAdvertisement is the portable source metadata negotiated before
// a subscription source is established.
type StreamSourceAdvertisement struct {
	ProfileVersion            string                 `json:"profileVersion"`
	Replay                    StreamReplayCapability `json:"replay"`
	Consistency               StreamReadConsistency  `json:"consistency"`
	RetentionPolicy           string                 `json:"retentionPolicy"`
	MaxReplayEvents           int                    `json:"maxReplayEvents"`
	MaxReplayBytes            uint64                 `json:"maxReplayBytes"`
	DisclosesEarliestPosition bool                   `json:"disclosesEarliestPosition"`
	HistoryRecovery           StreamRecovery         `json:"historyRecovery"`
}

func (a StreamSourceAdvertisement) Validate(limits StreamLimits) error {
	limits = ResolveStreamLimits(limits)
	if err := NegotiateStreamProfile(a.ProfileVersion); err != nil {
		return err
	}
	if (a.Consistency != StreamSnapshotStable && a.Consistency != StreamLiveBestEffort) ||
		!validStreamIdentifier(a.RetentionPolicy, limits.MaxIdentifierBytes) ||
		(a.HistoryRecovery != StreamRecoveryRestart && a.HistoryRecovery != StreamRecoveryRefetch) {
		return ErrInvalidStreamAdvertisement
	}
	switch a.Replay {
	case StreamReplayNone:
		if a.RetentionPolicy != "none" || a.MaxReplayEvents != 0 || a.MaxReplayBytes != 0 || a.DisclosesEarliestPosition {
			return ErrInvalidStreamAdvertisement
		}
	case StreamReplayBounded, StreamReplayDurable:
		if a.RetentionPolicy == "none" || a.MaxReplayEvents <= 0 || a.MaxReplayBytes == 0 {
			return ErrInvalidStreamAdvertisement
		}
	default:
		return ErrInvalidStreamAdvertisement
	}
	return nil
}

func MarshalStreamSourceAdvertisement(advertisement StreamSourceAdvertisement, limits StreamLimits) ([]byte, error) {
	limits = ResolveStreamLimits(limits)
	if err := advertisement.Validate(limits); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(advertisement)
	if err != nil || len(encoded) > limits.MaxFrameBytes {
		return nil, ErrInvalidStreamAdvertisement
	}
	return encoded, nil
}

func DecodeStreamSourceAdvertisement(input []byte, limits StreamLimits) (StreamSourceAdvertisement, error) {
	limits = ResolveStreamLimits(limits)
	if len(input) == 0 || len(input) > limits.MaxFrameBytes || ValidateJSON(input, limits.JSON) != nil {
		return StreamSourceAdvertisement{}, ErrInvalidStreamAdvertisement
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	var advertisement StreamSourceAdvertisement
	if err := decoder.Decode(&advertisement); err != nil {
		return StreamSourceAdvertisement{}, ErrInvalidStreamAdvertisement
	}
	if err := advertisement.Validate(limits); err != nil {
		return StreamSourceAdvertisement{}, err
	}
	return advertisement, nil
}

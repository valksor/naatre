package subscriptionbroker

import (
	"crypto/sha256"
	"encoding/binary"
	"maps"
	"sync"
	"time"

	naatreruntime "github.com/valksor/naatre/runtime"
)

type RolloutLane string

const (
	RolloutStable    RolloutLane = "stable"
	RolloutCandidate RolloutLane = "candidate"
)

type RolloutOutcome string

const (
	RolloutCaughtUp    RolloutOutcome = "caught-up"
	RolloutHistoryLost RolloutOutcome = "history-lost"
	RolloutInterrupted RolloutOutcome = "interrupted"
	RolloutRevoked     RolloutOutcome = "revoked"
	RolloutSlow        RolloutOutcome = "slow-consumer"
)

type RolloutConfig struct {
	Stable                naatreruntime.SubscriptionBroker
	Candidate             naatreruntime.SubscriptionBroker
	Revision              string
	CandidateBasisPoints  uint16
	MaxConnectionLifetime time.Duration
	ReconnectStagger      time.Duration
}

type Selection struct {
	Broker            naatreruntime.SubscriptionBroker
	Lane              RolloutLane
	ReconnectDeadline time.Time
}

type RolloutEvidence struct {
	Profile              string                    `json:"profile"`
	Revision             string                    `json:"revision"`
	CandidateBasisPoints uint16                    `json:"candidateBasisPoints"`
	RolledBack           bool                      `json:"rolledBack"`
	Selections           map[RolloutLane]uint64    `json:"selections"`
	Outcomes             map[RolloutOutcome]uint64 `json:"outcomes"`
}

// Rollout provides deterministic canary selection, finite connection rotation,
// and an atomic process-local rollback switch. It retains no selection key.
type Rollout struct {
	mu       sync.Mutex
	config   RolloutConfig
	evidence RolloutEvidence
}

func NewRollout(config RolloutConfig) (*Rollout, error) {
	if config.Stable == nil || config.Candidate == nil || config.Revision == "" || config.CandidateBasisPoints > 10000 ||
		config.MaxConnectionLifetime <= 0 || config.ReconnectStagger <= 0 || config.ReconnectStagger >= config.MaxConnectionLifetime {
		return nil, publicError(CodeInvalidConfig, "broker rollout configuration is invalid", nil)
	}
	return &Rollout{config: config, evidence: RolloutEvidence{
		Profile: Profile, Revision: config.Revision, CandidateBasisPoints: config.CandidateBasisPoints,
		Selections: map[RolloutLane]uint64{RolloutStable: 0, RolloutCandidate: 0},
		Outcomes:   map[RolloutOutcome]uint64{},
	}}, nil
}

func (r *Rollout) Select(reference string, established, identityExpiry time.Time) (Selection, error) {
	if r == nil || reference == "" || established.IsZero() {
		return Selection{}, publicError(CodeInvalidRecord, "rollout selection input is invalid", nil)
	}
	digest := sha256.Sum256([]byte(r.config.Revision + "\x00" + reference))
	r.mu.Lock()
	lane := RolloutStable
	broker := r.config.Stable
	if !r.evidence.RolledBack && uint16(binary.BigEndian.Uint64(digest[:8])%10000) < r.config.CandidateBasisPoints {
		lane = RolloutCandidate
		broker = r.config.Candidate
	}
	r.evidence.Selections[lane]++
	r.mu.Unlock()
	offset := time.Duration(binary.BigEndian.Uint64(digest[8:16]) % uint64(r.config.ReconnectStagger+1))
	deadline := established.Add(r.config.MaxConnectionLifetime - offset)
	if !identityExpiry.IsZero() && identityExpiry.Before(deadline) {
		deadline = identityExpiry
		if deadline.Before(established) {
			deadline = established
		}
	}
	return Selection{Broker: broker, Lane: lane, ReconnectDeadline: deadline}, nil
}

func (r *Rollout) Observe(outcome RolloutOutcome) error {
	if r == nil || !validOutcome(outcome) {
		return publicError(CodeInvalidRecord, "rollout outcome is invalid", nil)
	}
	r.mu.Lock()
	r.evidence.Outcomes[outcome]++
	r.mu.Unlock()
	return nil
}

func (r *Rollout) Rollback() {
	if r == nil {
		return
	}
	r.setRolledBack()
}

func (r *Rollout) setRolledBack() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evidence.RolledBack = true
	r.evidence.CandidateBasisPoints = 0
}

func (r *Rollout) Evidence() RolloutEvidence {
	if r == nil {
		return RolloutEvidence{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	value := r.evidence
	value.Selections = maps.Clone(r.evidence.Selections)
	value.Outcomes = maps.Clone(r.evidence.Outcomes)
	return value
}

func validOutcome(value RolloutOutcome) bool {
	switch value {
	case RolloutCaughtUp, RolloutHistoryLost, RolloutInterrupted, RolloutRevoked, RolloutSlow:
		return true
	default:
		return false
	}
}

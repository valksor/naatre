package runtime

import (
	"slices"
	"time"
)

type cursorSecretStatus uint8

const (
	cursorSecretsValid cursorSecretStatus = iota
	cursorSecretsInvalidConfig
	cursorSecretsInvalidKey
	cursorSecretsMissingActiveKey
)

type cursorSecrets struct {
	activeKeyID string
	keys        map[string][]byte
	ttl         time.Duration
	now         func() time.Time
}

func newCursorSecrets(activeKeyID string, source map[string][]byte, ttl time.Duration, now func() time.Time) (cursorSecrets, cursorSecretStatus) {
	if !validStreamReference(activeKeyID, false) || ttl < time.Second {
		return cursorSecrets{}, cursorSecretsInvalidConfig
	}
	keys := make(map[string][]byte, len(source))
	for id, key := range source {
		if !validStreamReference(id, false) || len(key) < 32 {
			return cursorSecrets{}, cursorSecretsInvalidKey
		}
		keys[id] = slices.Clone(key)
	}
	if _, ok := keys[activeKeyID]; !ok {
		return cursorSecrets{}, cursorSecretsMissingActiveKey
	}
	if now == nil {
		now = time.Now
	}
	return cursorSecrets{activeKeyID: activeKeyID, keys: keys, ttl: ttl, now: now}, cursorSecretsValid
}

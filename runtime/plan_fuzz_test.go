package runtime_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
)

func FuzzPrepareNeverInvokesHandlers(f *testing.F) {
	snapshot, calls := validationRegistry(f)
	for _, seed := range []string{
		`{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[]}]}}`,
		`{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"missing"}}]}]}}`,
		`{"version":"1","variables":{"id":"u-1"},"document":{"operations":[{"name":"Q","kind":"query","variables":[{"name":"id","type":"ID","required":true}],"select":[{"$call":{"name":"lookup","args":{"id":{"$var":"id"}},"select":[{"$field":{"name":"name"}}]}}]}]}}`,
		`{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$fragment":{"name":"Loop"}}]}],"fragments":[{"name":"Loop","select":[{"$fragment":{"name":"Loop"}}]}]}}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		request, err := protocol.DecodeRequest(input, protocol.DecodeOptions{})
		if err != nil {
			return
		}
		before := calls.Load()
		plan, _ := runtime.Prepare(snapshot, request)
		if calls.Load() != before {
			t.Fatal("Prepare invoked an application handler")
		}
		if plan == nil {
			return
		}
		first, firstErr := json.Marshal(plan.Description())
		second, secondErr := json.Marshal(plan.Description())
		if firstErr != nil || secondErr != nil || !bytes.Equal(first, second) {
			t.Fatalf("plan description is unstable: %s != %s (%v, %v)", first, second, firstErr, secondErr)
		}
	})
}

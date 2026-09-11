package runtime

import "testing"

func TestComparePathsOrdersIndexesBeforeKeys(t *testing.T) {
	t.Parallel()
	if compared := comparePaths([]any{"root", 1}, []any{"root", "1"}); compared >= 0 {
		t.Fatalf("index/key comparison = %d, want negative", compared)
	}
	if compared := comparePaths([]any{"root", "1"}, []any{"root", 1}); compared <= 0 {
		t.Fatalf("key/index comparison = %d, want positive", compared)
	}
}

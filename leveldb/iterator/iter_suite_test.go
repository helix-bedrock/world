package iterator_test

import (
	"testing"

	"github.com/helix-bedrock/world/leveldb/testutil"
)

func TestIterator(t *testing.T) {
	testutil.RunSuite(t, "Iterator Suite")
}

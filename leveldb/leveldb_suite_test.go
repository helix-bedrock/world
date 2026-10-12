package leveldb

import (
	"testing"

	"github.com/helix-bedrock/world/leveldb/testutil"
)

func TestLevelDB(t *testing.T) {
	testutil.RunSuite(t, "LevelDB Suite")
}

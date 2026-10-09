package features

import (
	"os"
	"testing"

	"relaye2e/harness"
)

func TestMain(m *testing.M) {
	os.Exit(harness.Main(m))
}

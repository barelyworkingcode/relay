package projectfs_test

import (
	"os"
	"strings"
	"testing"
)

// TestMain clears every GIT_* variable before any test runs git.
//
// This is deliberate: git hooks export GIT_DIR (and in a worktree,
// GIT_INDEX_FILE and friends), and the pre-push hook runs this suite. Left
// set, the tests' own git init/config/commit would act on the repository
// being pushed instead of their temp dirs.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GIT_") {
			os.Unsetenv(k)
		}
	}
	os.Exit(m.Run())
}

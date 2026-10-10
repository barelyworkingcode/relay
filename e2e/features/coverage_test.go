package features

import (
	"testing"

	"relaye2e/coverage"
	"relaye2e/harness"
)

func TestFeatureMapCoverage(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{})
	var doc coverage.DoorsDoc
	i.MustCLI("doors", "--json").JSON(t, &doc)
	findings, err := coverage.Check(harness.RepoRoot(), doc)
	if err != nil {
		t.Fatalf("coverage.Check: %v", err)
	}
	for _, f := range findings {
		if f.Rule == "info" {
			t.Logf("%s %s: %s", f.Rule, f.Where, f.Msg)
			continue
		}
		t.Errorf("%s %s: %s", f.Rule, f.Where, f.Msg)
	}
}

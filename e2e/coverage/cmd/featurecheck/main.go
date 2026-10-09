// Command featurecheck runs the coverage check against a repository tree and a
// saved `relay doors --json` document. It exists to try the check by hand.
//
//	go run ./coverage/cmd/featurecheck --repo DIR --doors FILE
package main

import (
	"flag"
	"fmt"
	"os"

	"relaye2e/coverage"
)

func main() {
	repo := flag.String("repo", "", "repository root (required)")
	doorsPath := flag.String("doors", "", "file holding `relay doors --json` output (required)")
	flag.Parse()
	if *repo == "" || *doorsPath == "" {
		fmt.Fprintln(os.Stderr, "featurecheck: --repo and --doors are required")
		os.Exit(2)
	}
	data, err := os.ReadFile(*doorsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "featurecheck:", err)
		os.Exit(2)
	}
	doors, err := coverage.ParseDoors(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "featurecheck:", err)
		os.Exit(2)
	}
	findings, err := coverage.Check(*repo, doors)
	if err != nil {
		fmt.Fprintln(os.Stderr, "featurecheck:", err)
		os.Exit(2)
	}
	for _, f := range findings {
		fmt.Println(f)
	}
	if len(coverage.Failures(findings)) > 0 {
		os.Exit(1)
	}
}

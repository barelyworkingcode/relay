//go:build relayfixture
// +build relayfixture

// This file is a fixture for tests and demo screenshots. It is intentionally
// excluded from the normal build via a never-set build tag so it doesn't
// land in `go test ./...` / `go build ./...` output. Read it as
// illustrative project content, not as code that runs.

package main

import (
	"log/slog"
	"net/http"
	"os"
)

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/contact", contactHandler)
	mux.HandleFunc("/health", healthHandler)

	slog.Info("acme-website starting", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server exited", "error", err)
		os.Exit(1)
	}
}

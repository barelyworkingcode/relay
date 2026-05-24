//go:build relayfixture
// +build relayfixture

// See main.go for why this file is build-tagged out of the normal build.

package main

import (
	"fmt"
	"net/http"
)

func indexHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "<h1>Acme</h1><p>We make things.</p>")
}

func contactHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// Forward to the inbox channel — body never logged.
	w.WriteHeader(http.StatusAccepted)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "ok")
}

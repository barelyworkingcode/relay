package main

import (
	"os"
	"sync"
	"testing"
)

func TestFrontendEnvIsServiceAgnostic(t *testing.T) {
	endpoint := Endpoint{Socket: "/tmp/fe.sock", Token: "fe-token"}
	got := endpoint.FrontendEnv()
	want := map[string]string{
		EnvFrontendSocket: "/tmp/fe.sock",
		EnvFrontendToken:  "fe-token",
	}
	if len(got) != len(want) {
		t.Fatalf("FrontendEnv: len=%d want=%d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("FrontendEnv[%q]=%q want=%q", k, got[k], v)
		}
	}
}

func TestFrontendChannelEnsureIsIdempotent(t *testing.T) {
	c := NewFrontendChannel()
	e1, err := c.Ensure()
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if e1.Token == "" || e1.Socket == "" {
		t.Fatalf("Ensure returned empty values: %+v", e1)
	}

	e2, err := c.Ensure()
	if err != nil {
		t.Fatalf("second Ensure failed: %v", err)
	}
	if e1 != e2 {
		t.Errorf("Ensure not idempotent")
	}
	c.Close()
	if _, err := os.Stat(e1.Socket); !os.IsNotExist(err) {
		t.Errorf("Close did not unlink socket file: %v (path=%s)", err, e1.Socket)
	}
}

func TestFrontendChannelTokenIsLongHex(t *testing.T) {
	c := NewFrontendChannel()
	endpoint, err := c.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// 32 random bytes → 64 hex chars
	if len(endpoint.Token) != 64 {
		t.Errorf("expected 64-char token, got %d: %q", len(endpoint.Token), endpoint.Token)
	}
	for _, ch := range endpoint.Token {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			t.Errorf("token contains non-hex char %q", ch)
			break
		}
	}
}

func TestFrontendChannelEnsureConcurrent(t *testing.T) {
	c := NewFrontendChannel()
	defer c.Close()

	const N = 64
	var wg sync.WaitGroup
	results := make([]Endpoint, N)

	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			endpoint, err := c.Ensure()
			if err != nil {
				t.Errorf("ensure failed: %v", err)
				return
			}
			results[idx] = endpoint
		}(i)
	}
	wg.Wait()

	for i := 1; i < N; i++ {
		if results[i] != results[0] {
			t.Fatalf("concurrent Ensure produced divergent values at index %d", i)
		}
	}
}

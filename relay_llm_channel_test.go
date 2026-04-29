package main

import (
	"os"
	"sync"
	"testing"
)

func TestLLMChannelCredsEnvFor(t *testing.T) {
	creds := LLMChannelCreds{
		Frontend: Endpoint{Socket: "/tmp/fe.sock", Token: "fe-token"},
		Internal: Endpoint{Socket: "/tmp/in.sock", Token: "in-token"},
	}
	cases := []struct {
		id   string
		want map[string]string
	}{
		{"eve", map[string]string{EnvFrontendSocket: "/tmp/fe.sock", EnvFrontendToken: "fe-token"}},
		{"relayscheduler", map[string]string{EnvFrontendSocket: "/tmp/fe.sock", EnvFrontendToken: "fe-token"}},
		{"relay-scheduler", map[string]string{EnvFrontendSocket: "/tmp/fe.sock", EnvFrontendToken: "fe-token"}},
		{"relayllm", map[string]string{EnvInternalSocket: "/tmp/in.sock", EnvInternalToken: "in-token"}},
		{"relay-llm", map[string]string{EnvInternalSocket: "/tmp/in.sock", EnvInternalToken: "in-token"}},
		{"relaytelegram", nil},
		{"random-service", nil},
		{"", nil},
		{"EVE", nil}, // case-sensitive — slugify always lowercases
	}
	for _, tc := range cases {
		got := creds.EnvFor(tc.id)
		if len(got) != len(tc.want) {
			t.Errorf("EnvFor(%q): len=%d want=%d", tc.id, len(got), len(tc.want))
			continue
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("EnvFor(%q)[%q]=%q want=%q", tc.id, k, got[k], v)
			}
		}
	}
}

func TestLLMChannelEnsureIsIdempotent(t *testing.T) {
	c := NewLLMChannel()
	c1, err := c.Ensure()
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if c1.Frontend.Token == "" || c1.Frontend.Socket == "" || c1.Internal.Token == "" || c1.Internal.Socket == "" {
		t.Fatalf("Ensure returned empty values: %+v", c1)
	}
	if c1.Frontend.Token == c1.Internal.Token {
		t.Errorf("frontend and internal tokens should be distinct")
	}
	if c1.Frontend.Socket == c1.Internal.Socket {
		t.Errorf("frontend and internal sockets should be distinct")
	}

	c2, err := c.Ensure()
	if err != nil {
		t.Fatalf("second Ensure failed: %v", err)
	}
	if c1 != c2 {
		t.Errorf("Ensure not idempotent")
	}
	c.Close()
	for _, p := range []string{c1.Frontend.Socket, c1.Internal.Socket} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("Close did not unlink socket file: %v (path=%s)", err, p)
		}
	}
}

func TestLLMChannelTokensAreLongHex(t *testing.T) {
	c := NewLLMChannel()
	creds, err := c.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, token := range []string{creds.Frontend.Token, creds.Internal.Token} {
		// 32 random bytes → 64 hex chars
		if len(token) != 64 {
			t.Errorf("expected 64-char token, got %d: %q", len(token), token)
		}
		for _, ch := range token {
			if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
				t.Errorf("token contains non-hex char %q", ch)
				break
			}
		}
	}
}

func TestLLMChannelEnsureConcurrent(t *testing.T) {
	// Hammer Ensure() from many goroutines and verify they all see the same
	// credential set.
	c := NewLLMChannel()
	defer c.Close()

	const N = 64
	var wg sync.WaitGroup
	results := make([]LLMChannelCreds, N)

	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			creds, err := c.Ensure()
			if err != nil {
				t.Errorf("ensure failed: %v", err)
				return
			}
			results[idx] = creds
		}(i)
	}
	wg.Wait()

	for i := 1; i < N; i++ {
		if results[i] != results[0] {
			t.Fatalf("concurrent Ensure produced divergent values at index %d", i)
		}
	}
}

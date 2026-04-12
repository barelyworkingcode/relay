package main

import (
	"os"
	"sync"
	"testing"
)

func TestParticipatesInLLMChannel(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"eve", true},
		{"relayllm", true},
		{"relay-llm", true},
		{"relayscheduler", false},
		{"relaytelegram", false},
		{"random-service", false},
		{"", false},
		{"EVE", false}, // case-sensitive — slugify always lowercases
	}
	for _, tc := range cases {
		if got := participatesInLLMChannel(tc.id); got != tc.want {
			t.Errorf("participatesInLLMChannel(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestLLMChannelEnsureIsIdempotent(t *testing.T) {
	c := NewLLMChannel()
	t1, s1, err := c.Ensure()
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if t1 == "" || s1 == "" {
		t.Fatalf("Ensure returned empty values: token=%q socket=%q", t1, s1)
	}
	t2, s2, err := c.Ensure()
	if err != nil {
		t.Fatalf("second Ensure failed: %v", err)
	}
	if t1 != t2 || s1 != s2 {
		t.Errorf("Ensure not idempotent: first=(%q,%q) second=(%q,%q)", t1, s1, t2, s2)
	}
	c.Close()
	if _, err := os.Stat(s1); !os.IsNotExist(err) {
		t.Errorf("Close did not unlink socket file: %v", err)
	}
}

func TestLLMChannelTokenIsLongHex(t *testing.T) {
	c := NewLLMChannel()
	token, _, err := c.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
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

func TestLLMChannelEnsureConcurrent(t *testing.T) {
	// Hammer Ensure() from many goroutines and verify they all see the same
	// credential pair (no torn writes, no double-init).
	c := NewLLMChannel()
	defer c.Close()

	const N = 64
	var wg sync.WaitGroup
	tokens := make([]string, N)
	sockets := make([]string, N)

	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			tk, sk, err := c.Ensure()
			if err != nil {
				t.Errorf("ensure failed: %v", err)
				return
			}
			tokens[idx] = tk
			sockets[idx] = sk
		}(i)
	}
	wg.Wait()

	for i := 1; i < N; i++ {
		if tokens[i] != tokens[0] || sockets[i] != sockets[0] {
			t.Fatalf("concurrent Ensure produced divergent values at index %d", i)
		}
	}
}

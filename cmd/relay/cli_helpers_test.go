package main

import (
	"maps"
	"strings"
	"testing"
)

// resolveID and its --id register-time override are gone (ADR-017
// implementation spec S6): McpOps.Add and ServiceOps.Create both derive a
// record's id from --name (slugify) unconditionally, the same as every
// other door always has, so a CLI-only override that brokering could never
// honour was removed along with the direct-mutation path it served.

func TestParseEnvPairs(t *testing.T) {
	tests := []struct {
		name    string
		pairs   []string
		want    map[string]string
		wantErr string
	}{
		{name: "nil", pairs: nil},
		{name: "empty", pairs: []string{}},
		{name: "valid", pairs: []string{"FOO=bar", "BAZ=qux"}, want: map[string]string{"FOO": "bar", "BAZ": "qux"}},
		{name: "value with =", pairs: []string{"KEY=val=ue"}, want: map[string]string{"KEY": "val=ue"}},
		{name: "no =", pairs: []string{"NOPE"}, wantErr: "invalid --env format"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, err := parseEnvPairs(tc.pairs)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseEnvPairs(%q) error = %v, want one mentioning %q", tc.pairs, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseEnvPairs(%q): unexpected error: %v", tc.pairs, err)
			}
			if tc.want == nil {
				if env != nil {
					t.Errorf("parseEnvPairs(%q) = %v, want nil map", tc.pairs, env)
				}
				return
			}
			if !maps.Equal(env, tc.want) {
				t.Errorf("parseEnvPairs(%q) = %v, want %v", tc.pairs, env, tc.want)
			}
		})
	}
}

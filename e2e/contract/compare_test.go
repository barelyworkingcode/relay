package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

func entry(step, value string) Entry {
	return Entry{Step: step, Kind: "http", Value: json.RawMessage(value)}
}

func TestCompareFindsADifference(t *testing.T) {
	t.Parallel()
	same := Transcript{entry("01 GET /api/projects", `{"status":200}`), entry("02 GET /api/hosts", `{"status":200}`)}
	cases := []struct {
		name       string
		fake, real Transcript
		wantStep   string // empty: no difference
		wantInMsg  string
	}{
		{"equal", same, append(Transcript{}, same...), "", ""},
		{"value differs", same,
			Transcript{same[0], entry("02 GET /api/hosts", `{"status":409}`)},
			"02 GET /api/hosts", "409"},
		{"relay answers fewer steps", same, same[:1], "02 GET /api/hosts", "(no such step)"},
		{"relay answers an extra step", same[:1], same, "02 GET /api/hosts", "(no such step)"},
		{"step label differs", same,
			Transcript{same[0], entry("02 GET /api/mcps", `{"status":200}`)},
			"02 GET /api/hosts / 02 GET /api/mcps", ""},
		{"order is compared exactly", same, Transcript{same[1], same[0]}, "01 GET /api/projects / 02 GET /api/hosts", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := firstDifference(c.fake, c.real)
			if c.wantStep == "" {
				if d != nil {
					t.Fatalf("equal transcripts differ: %+v", d)
				}
				return
			}
			if d == nil {
				t.Fatalf("a difference at %q went unnoticed", c.wantStep)
			}
			if d.step != c.wantStep {
				t.Fatalf("difference at %q, want %q", d.step, c.wantStep)
			}
			if !strings.Contains(d.fake+d.real, c.wantInMsg) {
				t.Fatalf("difference shows fake %q and real %q, want %q in it", d.fake, d.real, c.wantInMsg)
			}
		})
	}
}

// TestNormaliseMasksNoiseOnly checks both halves of the normaliser's job: two
// targets' generated ids, times and paths read the same, and a real change in
// a status or a body does not.
func TestNormaliseMasksNoiseOnly(t *testing.T) {
	t.Parallel()
	mk := func(dir, id string) *normaliser {
		n := &normaliser{spec: map[string]bool{"p_acme": true}, num: map[string]int{}, paths: []pathSub{{dir, "<DIR>"}}}
		n.learn(id)
		return n
	}
	render := func(n *normaliser, id, dir, ts string, status string) string {
		body := `{"id":"` + id + `","project":"p_acme","path":"` + dir + `/projects/p_acme","created_at":"` + ts + `","at":1234,"pid":77,` +
			`"status":"` + status + `","ssh_argv":["ssh","ControlPath=` + dir + `/c/%C"],"probe":{"node_path":"/x/node","ok":true}}`
		return string(n.value([]byte(body)))
	}
	a := render(mk("/tmp/a/001", "id-one"), "id-one", "/tmp/a/001", "2026-01-01T00:00:00Z", "idle")
	b := render(mk("/tmp/b/002", "id-two"), "id-two", "/tmp/b/002", "2027-05-05T10:20:30.123+02:00", "connected")
	if a != b {
		t.Fatalf("noise was not masked\n  a: %s\n  b: %s", a, b)
	}
	if !strings.Contains(a, `"p_acme"`) || !strings.Contains(a, "<ID:1>") {
		t.Fatalf("spec id must stay and the generated id must read <ID:1>: %s", a)
	}
	c := render(mk("/tmp/b/002", "id-two"), "id-two", "/tmp/b/002", "2026-01-01T00:00:00Z", "unreachable")
	if c == a {
		t.Fatalf("a different status was masked: %s", c)
	}
}

package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

// Entry is one normalised step of a transcript. Value is canonical JSON with
// sorted keys.
type Entry struct {
	Step, Kind string
	Value      json.RawMessage
}

// Transcript is everything one target answered, in order.
type Transcript []Entry

// difference is the first place two transcripts part.
type difference struct{ step, fake, real string }

func (e Entry) String() string { return fmt.Sprintf("%s %s", e.Kind, e.Value) }

// firstDifference walks the transcripts in step. A missing entry, an extra
// entry or a differing step, kind or value is a difference.
func firstDifference(fake, real Transcript) *difference {
	for k := 0; k < len(fake) || k < len(real); k++ {
		switch {
		case k >= len(fake):
			return &difference{real[k].Step, "(no such step)", real[k].String()}
		case k >= len(real):
			return &difference{fake[k].Step, fake[k].String(), "(no such step)"}
		}
		f, r := fake[k], real[k]
		if f.Step != r.Step || f.Kind != r.Kind || !bytes.Equal(f.Value, r.Value) {
			step := f.Step
			if f.Step != r.Step {
				step = f.Step + " / " + r.Step
			}
			return &difference{step, f.String(), r.String()}
		}
	}
	return nil
}

// Compare fails t at the first step where the two transcripts differ. The
// failure shows the step label and both values; a failed test keeps both
// instance directories.
func Compare(t *testing.T, fake, real Transcript) {
	t.Helper()
	if d := firstDifference(fake, real); d != nil {
		t.Fatalf("fakerelay and relay differ at step %s\n  fakerelay: %s\n  relay:     %s", d.step, d.fake, d.real)
	}
}

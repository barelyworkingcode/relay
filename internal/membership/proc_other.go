//go:build !darwin

package membership

// otherSource is the non-darwin Source: it never has an answer, so Resolve
// always fails closed. This package's algorithm is platform-agnostic, but
// relay only ships on darwin and no other OS's process-ancestry API has been
// verified against C3 — refusing every pid is the honest statement of that,
// not a placeholder to fill in later.
type otherSource struct{}

// NewSource returns the non-darwin ancestry reader: no process is ever a
// member.
func NewSource() Source { return otherSource{} }

func (otherSource) Info(pid int) (ProcInfo, bool) { return ProcInfo{}, false }

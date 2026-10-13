package types

// Launcher replaces the program a claude, codex or pi session runs: the
// session host runs Command, then Args, then the kind's own arguments. Relay
// sends one only for an agent entry that names a command; the host never reads
// one from disk.
type Launcher struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// Program is the argv prefix to run: the launcher's command and args, or
// fallback alone when l is nil.
func (l *Launcher) Program(fallback string) []string {
	if l == nil {
		return []string{fallback}
	}
	return append([]string{l.Command}, l.Args...)
}

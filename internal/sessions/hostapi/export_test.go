package hostapi

import "github.com/barelyworkingcode/relay/internal/membership"

// RegisterSessionForTest inserts a live session root directly, bypassing
// POST /launch entirely. Test-only seam (production code only ever
// populates the table from handleLaunch): it exists so a /permission test
// can exercise the real C3 membership.Resolve walk against a known root
// without needing a full shim+target process tree for every case, the same
// way the shim's own tests build fake process trees rather than driving
// relay's entire launch path. rootPID here plays the role handleLaunch
// gives the shim's own pid (sessionEntry's doc comment) — a caller
// registering some other process's pid is exercising the same code path a
// real launch would, just skipping the real spawn.
func (s *Server) RegisterSessionForTest(id string, rootPID int) {
	info, ok := membership.NewSource().Info(rootPID)
	if !ok {
		info = membership.ProcInfo{}
	}
	s.table.put(&sessionEntry{id: id, state: stateLive, shimPID: rootPID, rootStart: info})
}

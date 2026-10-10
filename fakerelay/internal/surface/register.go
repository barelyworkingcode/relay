// Package surface registers every domain package's doors.
package surface

import (
	"github.com/barelyworkingcode/relay/fakerelay/internal/api"
	"github.com/barelyworkingcode/relay/fakerelay/internal/files"
	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/sessions"
)

// Register installs the file, API and session doors. The file plane goes first
// because the other two read host status through it.
func Register(r server.Registrar, d server.Deps) error {
	hosts, err := files.Register(r, d)
	if err != nil {
		return err
	}
	d.Hosts = hosts
	if err := api.Register(r, d); err != nil {
		return err
	}
	return sessions.Register(r, d)
}

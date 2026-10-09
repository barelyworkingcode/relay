// Package surface registers every domain package's doors.
package surface

import (
	"github.com/barelyworkingcode/relay/fakerelay/internal/api"
	"github.com/barelyworkingcode/relay/fakerelay/internal/files"
	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/sessions"
)

// Register installs the file, API and session doors.
func Register(r server.Registrar, d server.Deps) error {
	for _, reg := range []func(server.Registrar, server.Deps) error{files.Register, api.Register, sessions.Register} {
		if err := reg(r, d); err != nil {
			return err
		}
	}
	return nil
}

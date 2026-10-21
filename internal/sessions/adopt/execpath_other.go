//go:build !darwin

package adopt

import "errors"

// execPath has no answer off macOS, so no child is ever the turn helper.
func execPath(pid int) (string, error) {
	return "", errors.New("adopt: exec path is read on macOS only")
}

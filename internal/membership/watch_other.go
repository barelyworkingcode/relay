//go:build !darwin

package membership

import "fmt"

// WatchExit has no non-darwin implementation: no process is ever a member
// on this platform (proc_other.go), so nothing in relay should ever call
// this here. It always errors rather than panicking or blocking forever, so
// a caller that reaches it anyway fails closed like every other Source
// failure on this platform.
func WatchExit(pid int, want ProcInfo, onExit func()) (cancel func(), err error) {
	return nil, fmt.Errorf("membership: WatchExit is darwin-only")
}

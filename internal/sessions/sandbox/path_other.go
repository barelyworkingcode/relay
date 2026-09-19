//go:build !darwin

package sandbox

// onDiskPath is the identity off macOS: Seatbelt is a macOS mechanism, and the
// case problem it corrects is a case-insensitive-volume one.
func onDiskPath(p string) string { return p }

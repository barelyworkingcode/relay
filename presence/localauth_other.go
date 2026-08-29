//go:build !darwin

package presence

import (
	"context"
	"sync/atomic"
)

var localAuthProviderConstructions atomic.Int64

// LocalAuthProviderConstructions reports how many times the real provider's
// constructor has been called in this process. See the darwin file for what
// this backs (AC-20).
func LocalAuthProviderConstructions() int64 {
	return localAuthProviderConstructions.Load()
}

// LocalAuthProvider is unavailable outside darwin: LocalAuthentication is a
// macOS/iOS framework, and relay's tray host is macOS-only regardless. Kept
// so the package still builds and tests on other platforms.
type LocalAuthProvider struct{}

func NewLocalAuthProvider() *LocalAuthProvider {
	localAuthProviderConstructions.Add(1)
	return &LocalAuthProvider{}
}

func (p *LocalAuthProvider) Evaluate(ctx context.Context, reason string) error {
	return ErrUnavailable
}

package service

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"sync"

	"github.com/barelyworkingcode/relay/internal/peertoken"
)

// IdentityKind names what a bound launch identity is, and so what it may do.
// The protocol (docs/launch-identity.md) is the same for every kind; only the
// record relay binds, and the capability lookup that reads it, differ.
type IdentityKind string

const (
	// IdentityKindService is a process relay's service registry launched.
	// Its capability comes from the service record's frontend_consumer.
	IdentityKindService IdentityKind = "service"
)

// Identity is what relay knows about one launch: who it launched, and once
// Hello succeeds, which process presented the launch secret.
type Identity struct {
	Kind IdentityKind
	// Name is the launch's name on the wire: Hello's "name". For a service it
	// is the service id.
	Name string
	// FrontendConsumer is meaningful only for IdentityKindService.
	FrontendConsumer bool
	// Process is the zero value until Hello binds the launch.
	Process peertoken.Process
}

// IsFrontendConsumer reports whether this identity is a service that reaches
// the frontend socket, and therefore no bridge service operation.
func (i Identity) IsFrontendConsumer() bool {
	return i.Kind == IdentityKindService && i.FrontendConsumer
}

// IsBridgeService reports whether this identity is a service that holds the
// bridge service operations, and therefore no frontend access.
func (i Identity) IsBridgeService() bool {
	return i.Kind == IdentityKindService && !i.FrontendConsumer
}

// LaunchSecretHexLen is the length of the launch secret on the wire: 32
// random bytes, lowercase hex.
const LaunchSecretHexLen = 64

// ErrHelloRefused is the one error every refused Hello returns. The reason is
// wrapped for relay's own log; the bridge answers every refusal identically so
// a caller cannot learn which launches exist or whether its guess was close.
var ErrHelloRefused = errors.New("hello refused")

// Launches is the table of live launches and the identities bound to them.
// The service registry begins and ends launches, the bridge binds them at
// Hello, and both the bridge router and the frontend server look callers up
// by peer audit token.
type Launches struct {
	mu     sync.Mutex
	byName map[string]*Launch
	bound  map[peertoken.Process]*Launch
}

// Launch is one launch's record. Its handle is what ends it.
type Launch struct {
	table *Launches
	id    Identity
	// secretHash rather than the secret: relay needs to recognise the secret,
	// never to produce it again after writing it into the child's pipe.
	secretHash [sha256.Size]byte
	spent      bool
	ended      bool
}

func NewLaunches() *Launches {
	return &Launches{
		byName: make(map[string]*Launch),
		bound:  make(map[peertoken.Process]*Launch),
	}
}

// Begin records a new launch named id.Name and returns its single-use secret.
// A launch already recorded under that name is ended first: a name has at
// most one live launch, so a restart can never leave the previous process's
// identity standing beside the new one's.
func (t *Launches) Begin(id Identity) (string, *Launch, error) {
	if id.Name == "" {
		return "", nil, errors.New("launch identity: empty name")
	}
	if id.Kind == "" {
		return "", nil, errors.New("launch identity: empty kind")
	}
	secret, err := GenerateRandomHex(LaunchSecretHexLen / 2)
	if err != nil {
		return "", nil, fmt.Errorf("launch identity: %w", err)
	}
	id.Process = peertoken.Process{}
	l := &Launch{table: t, id: id, secretHash: sha256.Sum256([]byte(secret))}

	t.mu.Lock()
	defer t.mu.Unlock()
	if prev := t.byName[id.Name]; prev != nil {
		t.endLocked(prev)
	}
	t.byName[id.Name] = l
	return secret, l, nil
}

// End clears this launch and any identity bound to it. Safe to call more
// than once, and a no-op for a launch a later Begin already replaced.
func (l *Launch) End() {
	if l == nil {
		return
	}
	l.table.mu.Lock()
	defer l.table.mu.Unlock()
	l.table.endLocked(l)
}

func (t *Launches) endLocked(l *Launch) {
	if l.ended {
		return
	}
	l.ended = true
	if t.byName[l.id.Name] == l {
		delete(t.byName, l.id.Name)
	}
	if l.spent && t.bound[l.id.Process] == l {
		delete(t.bound, l.id.Process)
	}
}

// Bind is Hello: it binds the launch named name to peer if secret is that
// launch's secret. On success the secret is spent and no later Bind for the
// same launch succeeds, whoever presents it.
//
// A wrong secret does not spend the launch. This is deliberate: spending on a
// miss would let any same-user process that can name a service turn that
// service's start into a failure by guessing once, while a guess against 256
// bits buys nothing.
func (t *Launches) Bind(name, secret string, peer peertoken.Token) (Identity, error) {
	if !peer.Valid() {
		return Identity{}, fmt.Errorf("%w: peer audit token unavailable", ErrHelloRefused)
	}
	if !isLaunchSecretShape(secret) {
		return Identity{}, fmt.Errorf("%w: malformed secret", ErrHelloRefused)
	}
	presented := sha256.Sum256([]byte(secret))
	proc := peer.Process()

	t.mu.Lock()
	defer t.mu.Unlock()
	l := t.byName[name]
	if l == nil {
		return Identity{}, fmt.Errorf("%w: no live launch named %q", ErrHelloRefused, name)
	}
	if subtle.ConstantTimeCompare(presented[:], l.secretHash[:]) != 1 {
		return Identity{}, fmt.Errorf("%w: secret does not match launch %q", ErrHelloRefused, name)
	}
	if l.spent {
		return Identity{}, fmt.Errorf("%w: launch %q is already bound", ErrHelloRefused, name)
	}
	if other := t.bound[proc]; other != nil {
		return Identity{}, fmt.Errorf("%w: process %d already holds identity %q", ErrHelloRefused, proc.PID, other.id.Name)
	}
	l.spent = true
	l.id.Process = proc
	t.bound[proc] = l
	return l.id, nil
}

// Lookup returns the identity bound to the process peer names, if any.
func (t *Launches) Lookup(peer peertoken.Token) (Identity, bool) {
	if t == nil || !peer.Valid() {
		return Identity{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	l := t.bound[peer.Process()]
	if l == nil {
		return Identity{}, false
	}
	return l.id, true
}

// Bound returns the identity bound to the live launch named name, if Hello
// has bound one.
func (t *Launches) Bound(name string) (Identity, bool) {
	if t == nil {
		return Identity{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	l := t.byName[name]
	if l == nil || !l.spent {
		return Identity{}, false
	}
	return l.id, true
}

// Len reports how many launches are live, bound or not.
func (t *Launches) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.byName)
}

func isLaunchSecretShape(s string) bool {
	if len(s) != LaunchSecretHexLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

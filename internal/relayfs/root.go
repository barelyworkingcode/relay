package relayfs

import (
	"os"

	"github.com/hugelgupf/p9/p9"
)

// Policy is the whole of what this package is told about a mount's access
// mode — read or write. It knows nothing about grants, projects, or
// enrolments; that's P4/P5's job, mapping a config.MountGrant into this.
type Policy struct {
	Write bool
}

// Hooks is everything a later piece (P4) plugs in — auditing and budget
// enforcement. The zero value (NopHooks) is a no-op, which is what makes
// this package testable with no audit or listener in the loop: every test
// in this package uses NopHooks.
type Hooks interface {
	// AdmitOp is called once per 9P request this package handles, before
	// doing any work. A non-nil error refuses the operation immediately.
	AdmitOp() error
	// ChargeRead is called with the number of bytes about to be returned
	// from a read, before they are returned. A non-nil error means the
	// read is refused (map to EDQUOT in the caller).
	ChargeRead(n int) error
	// ChargeWrite is the write-side counterpart, called with the number of
	// bytes about to be written, before the write happens.
	ChargeWrite(n int) error
	// BeginMutation is called before a mutating operation (create, mkdir,
	// unlink, rmdir, rename, symlink, truncate, chmod, setxattr,
	// removexattr, write) is applied. args is a small map describing what's
	// about to happen (path, target, size, mode — whatever's relevant; see
	// each call site below for exactly what to pass). It returns an `end`
	// function to call with the operation's own result once it's known
	// (nil error = succeeded), and an error which, if non-nil, refuses the
	// operation before anything is touched — the caller must not call the
	// filesystem at all in that case, let alone `end`.
	BeginMutation(op string, args map[string]any) (end func(err error), err error)
	// Refused is called (not to authorize anything — just to report) when
	// an operation is refused by this package's own containment policy,
	// never by Hooks itself. op and args mirror BeginMutation's; rule is a
	// short machine-readable string naming which containment-table rule
	// fired (e.g. "symlink-on-open", "hardlink-refused", "apple-xattr").
	Refused(op string, args map[string]any, rule string)
}

// NopHooks is the zero-cost, always-permits implementation used by every
// test in this package and by nothing else.
type NopHooks struct{}

func (NopHooks) AdmitOp() error { return nil }

func (NopHooks) ChargeRead(n int) error { return nil }

func (NopHooks) ChargeWrite(n int) error { return nil }

func (NopHooks) BeginMutation(op string, args map[string]any) (func(err error), error) {
	return func(error) {}, nil
}

func (NopHooks) Refused(op string, args map[string]any, rule string) {}

// Root is the scoped 9P server: one directory, kernel-enforced containment,
// implementing p9.Attacher so a mount session (a later piece) can hand it
// straight to p9.NewServer.
type Root struct {
	root   *os.Root
	policy Policy
	hooks  Hooks
}

// Open opens path (an absolute host directory — the caller, a later piece,
// has already validated this via project.ValidateMounts; this package does
// not re-validate the operator's own configuration, only what a 9P client
// does once attached) with os.OpenRoot, and returns a Root ready to Attach.
func Open(path string, policy Policy, hooks Hooks) (*Root, error) {
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	if hooks == nil {
		hooks = NopHooks{}
	}
	return &Root{root: r, policy: policy, hooks: hooks}, nil
}

// Attach implements p9.Attacher. Returns a file representing the mount
// root itself.
func (r *Root) Attach() (p9.File, error) {
	return &file{r: r, rel: ""}, nil
}

// Close releases the underlying os.Root handle.
func (r *Root) Close() error {
	return r.root.Close()
}

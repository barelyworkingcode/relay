//go:build darwin && relaytest

package sealed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/barelyworkingcode/relay/internal/logging"
)

const (
	// FileKeyringName is the store file inside the config dir. It holds the
	// bytes the login-keychain item holds, so decodeKeychainPayload reads both.
	FileKeyringName = "test-keychain.json"
	// FileKeyringFaultName selects which login-keychain failure the file
	// provider reproduces. It is re-read on every operation.
	FileKeyringFaultName = "test-keychain-fault.json"
)

// errSecInteractionNotAllowedStatus is the OSStatus the login keychain
// returns for a locked keychain or a foreign-ACL item.
const errSecInteractionNotAllowedStatus = -25308

type keychainFault string

const (
	faultNone    keychainFault = "none"
	faultLocked  keychainFault = "locked"
	faultMissing keychainFault = "missing"
	faultCorrupt keychainFault = "corrupt"
	faultSlow    keychainFault = "slow"
)

// corruptPayload is deliberately not the keychain payload shape, so the
// corrupt fault runs decodeKeychainPayload's own failure.
var corruptPayload = []byte("not a keychain payload")

type fileKeyring struct {
	dir string
	mu  sync.Mutex
}

// OpenFileKeyring returns a Keyring that keeps its one item in dir and never
// touches the login keychain. It refuses when dir, the store file or the
// fault file cannot be trusted, so a harness learns at start that the
// provider is unusable rather than at the first seal.
func OpenFileKeyring(configDir string) (Keyring, error) {
	st, err := os.Stat(configDir)
	if err != nil {
		return nil, fmt.Errorf("sealed: test keychain dir: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("sealed: test keychain dir %s is not a directory", configDir)
	}
	probe, err := os.CreateTemp(configDir, ".test-keychain-probe-*")
	if err != nil {
		return nil, fmt.Errorf("sealed: test keychain dir %s is not writable: %w", configDir, err)
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())

	k := &fileKeyring{dir: configDir}
	for _, name := range []string{FileKeyringName, FileKeyringFaultName} {
		if err := checkPrivateFile(filepath.Join(configDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if _, err := k.readFault(); err != nil {
		return nil, err
	}
	return k, nil
}

// checkPrivateFile requires a regular file (not a symlink) owned by this uid
// with no group or other bits. A missing file returns an error wrapping
// os.ErrNotExist.
func checkPrivateFile(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("sealed: %s is not a regular file", path)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("sealed: %s is not owned by this user", path)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("sealed: %s has group or other permissions (%#o); want 0600", path, st.Mode().Perm())
	}
	return nil
}

func (k *fileKeyring) readFault() (keychainFault, error) {
	path := filepath.Join(k.dir, FileKeyringFaultName)
	if err := checkPrivateFile(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return faultNone, nil
		}
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("sealed: reading %s: %w", path, err)
	}
	var v struct {
		Fault keychainFault `json:"fault"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("sealed: %s is invalid: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", fmt.Errorf("sealed: %s is invalid: trailing data", path)
	}
	switch v.Fault {
	case faultNone, faultLocked, faultMissing, faultCorrupt, faultSlow:
		return v.Fault, nil
	}
	return "", fmt.Errorf("sealed: %s is invalid: unknown fault %q", path, v.Fault)
}

// beginOp reads the fault and, when it changes the operation, records the
// event before the operation acts, so a fault that blocks has already logged.
func (k *fileKeyring) beginOp(op string) (keychainFault, error) {
	f, err := k.readFault()
	if err != nil {
		return "", err
	}
	if f != faultNone {
		logging.BeginEvent(context.Background(), "debug.keychain.fault").
			Set("keychain_op", op).Set("fault", string(f)).
			End(logging.OutcomeOK, "", nil)
	}
	return f, nil
}

// waitOutBound blocks for relay's own keychain bound, as an unanswered
// dialog would, and parks nothing past it.
func waitOutBound() {
	t := time.NewTimer(keychainReadTimeout)
	defer t.Stop()
	<-t.C
}

func (k *fileKeyring) storePath() string { return filepath.Join(k.dir, FileKeyringName) }

func (k *fileKeyring) Load() (string, []byte, error) {
	f, err := k.beginOp("load")
	if err != nil {
		return "", nil, err
	}
	return k.load(f)
}

func (k *fileKeyring) load(f keychainFault) (string, []byte, error) {
	switch f {
	case faultLocked:
		return "", nil, fmt.Errorf("%w: OSStatus %d (errSecInteractionNotAllowed) for %s/%s",
			ErrKeyUnreadable, errSecInteractionNotAllowedStatus, keychainService, keychainAccount)
	case faultMissing:
		return "", nil, ErrKeyMissing
	case faultCorrupt:
		return decodeKeychainPayload(corruptPayload)
	case faultSlow:
		// relay's own bound answers, exactly as for an unanswered keychain dialog.
		waitOutBound()
		return "", nil, errCopyTimeout(keychainService, keychainAccount)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := checkPrivateFile(k.storePath()); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, ErrKeyMissing
		}
		return "", nil, err
	}
	data, err := os.ReadFile(k.storePath())
	if err != nil {
		return "", nil, fmt.Errorf("sealed: reading %s: %w", k.storePath(), err)
	}
	return decodeKeychainPayload(data)
}

func (k *fileKeyring) Create() (string, []byte, error) {
	f, err := k.beginOp("create")
	if err != nil {
		return "", nil, err
	}
	if f != faultMissing {
		if _, _, err := k.load(f); err == nil {
			return "", nil, fmt.Errorf("sealed: a key already exists under %s/%s; refusing to overwrite", keychainService, keychainAccount)
		} else if !errors.Is(err, ErrKeyMissing) {
			return "", nil, err
		}
	}
	keyID, key, err := generateKey()
	if err != nil {
		return "", nil, err
	}
	payload, err := encodeKeychainPayload(keyID, key)
	if err != nil {
		return "", nil, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.writeStore(payload); err != nil {
		return "", nil, err
	}
	return keyID, key, nil
}

// writeStore replaces the store atomically: a temp file at 0600 in the same
// directory, then a rename.
func (k *fileKeyring) writeStore(payload []byte) error {
	tmp, err := os.CreateTemp(k.dir, ".test-keychain-*")
	if err != nil {
		return fmt.Errorf("sealed: writing test keychain: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sealed: writing test keychain: %w", err)
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sealed: writing test keychain: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("sealed: writing test keychain: %w", err)
	}
	if err := os.Rename(tmp.Name(), k.storePath()); err != nil {
		return fmt.Errorf("sealed: writing test keychain: %w", err)
	}
	return nil
}

func (k *fileKeyring) Destroy() error {
	f, err := k.beginOp("destroy")
	if err != nil {
		return err
	}
	switch f {
	case faultLocked:
		return fmt.Errorf("sealed: deleting keychain item: OSStatus %d", errSecInteractionNotAllowedStatus)
	case faultMissing:
		return nil
	case faultSlow:
		waitOutBound()
		return errDeleteTimeout(keychainService, keychainAccount)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := os.Remove(k.storePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sealed: deleting test keychain: %w", err)
	}
	return nil
}

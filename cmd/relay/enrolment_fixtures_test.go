package main

// Fixtures package main's own enrolment tests share: the CSR generators, the
// sandboxed store, and the project every grant names. internal/enrolment
// carries its own copies of the first two — the domain package cannot reach
// package main's test helpers, and these are generators, not assertions, so
// the two sides proving the same shape independently costs nothing.

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/enrolment"
	"github.com/barelyworkingcode/relay/internal/project"
)

// genCSRPEMFromKey builds a PKCS#10 request PEM over key, letting the
// caller override the signature algorithm (0 picks the default for the key
// type).
func genCSRPEMFromKey(t *testing.T, key crypto.Signer, sigAlg x509.SignatureAlgorithm, cn string) []byte {
	t.Helper()
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}, SignatureAlgorithm: sigAlg}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	assertNoErr(t, err, "CreateCertificateRequest")
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

// genClientCSRPEM is the fixture every package-main test that needs a real,
// valid CSR rather than a negative case of its own reaches for.
func genClientCSRPEM(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	assertNoErr(t, err, "generate P-256 key")
	return genCSRPEMFromKey(t, key, 0, cn)
}

func parseCSRForTest(t *testing.T, csrPEM []byte) *x509.CertificateRequest {
	t.Helper()
	csr, err := enrolment.ParseClientCSR(csrPEM)
	assertNoErr(t, err, "ParseClientCSR")
	return csr
}

// newEnrolmentSandbox returns a sandboxed store whose config dir also holds
// the CA and the emitted bundles, so nothing here can touch the real
// ~/Library/Application Support/relay.
func newEnrolmentSandbox(t *testing.T) (string, config.SettingsStore) {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	return dir, store
}

// mkStoreProject creates a project of the given kind in the store and returns
// it. path must be empty for a remote project (project.ValidateShape).
func mkStoreProject(t *testing.T, store config.SettingsStore, kind config.ProjectKind, name, path string) config.Project {
	t.Helper()
	var proj config.Project
	var createErr error
	assertNoErr(t, store.With(func(s *config.Settings) {
		proj, createErr = project.CreateWithTokenKind(s, kind, name, path, []string{}, []string{}, nil, nil)
	}), "create %s project", kind)
	assertNoErr(t, createErr, "create %s project", kind)
	return proj
}

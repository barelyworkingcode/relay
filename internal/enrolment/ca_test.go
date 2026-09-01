package enrolment

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// parseCertPEM decodes a single PEM certificate for assertions.
func parseCertPEM(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatalf("not valid PEM: %q", certPEM)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	assertNoErr(t, err, "parse certificate")
	return cert
}

// The CA is generated lazily on first use and persisted. A second call MUST
// reuse it: minting a fresh CA would silently invalidate every certificate
// issued so far, which — since revocation rather than expiry is the control —
// is the one failure that would cut every enrolled client at once with no
// operator action to point at.
func TestLoadOrCreateCA_ReusesPersistedCA(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)

	first, err := LoadOrCreateCA(testSealer())
	assertNoErr(t, err, "first LoadOrCreateCA")

	for _, name := range []string{CAKeySealedFile, CACertFile} {
		info, err := os.Stat(filepath.Join(dir, name))
		assertNoErr(t, err, "stat %s", name)
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Fatalf("%s mode = %#o, want 0600", name, perm)
		}
	}

	second, err := LoadOrCreateCA(testSealer())
	assertNoErr(t, err, "second LoadOrCreateCA")
	if !bytes.Equal(first.CertPEM(), second.CertPEM()) {
		t.Fatal("second LoadOrCreateCA returned a different CA certificate — the persisted CA was not reused")
	}

	// The real property, stated in the terms that matter: a certificate the
	// first call signed still verifies against the second call's pool.
	_, certPEM, _, err := first.IssueClientCert("hermes-mail")
	assertNoErr(t, err, "issue client cert")
	cert := parseCertPEM(t, certPEM)
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     second.Pool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("client cert from the first CA does not verify against the reloaded CA: %v", err)
	}
}

// The certificate's identity is tied to the enrolment's client id, and the
// certificate is usable for client auth and nothing else.
func TestIssueClientCert_BindsClientIDAndClientAuth(t *testing.T) {
	mkEmptySandboxRelayHome(t)
	ca, err := LoadOrCreateCA(testSealer())
	assertNoErr(t, err, "LoadOrCreateCA")

	keyPEM, certPEM, fingerprint, err := ca.IssueClientCert("hermes-mail")
	assertNoErr(t, err, "IssueClientCert")

	cert := parseCertPEM(t, certPEM)
	if cert.Subject.CommonName != "hermes-mail" {
		t.Fatalf("certificate CN = %q, want %q", cert.Subject.CommonName, "hermes-mail")
	}
	if cert.IsCA {
		t.Fatal("client certificate must not be a CA")
	}
	wantEKU := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != wantEKU[0] {
		t.Fatalf("certificate ExtKeyUsage = %v, want client auth only", cert.ExtKeyUsage)
	}
	if fingerprint != FingerprintCert(cert) {
		t.Fatalf("returned fingerprint %q does not match the certificate's own %q", fingerprint, FingerprintCert(cert))
	}
	if block, _ := pem.Decode(keyPEM); block == nil {
		t.Fatal("client key is not valid PEM")
	}

	// Client certs are long-lived on purpose: revocation, not expiry, is the
	// control. A cert that expires inside a couple of years would quietly
	// reintroduce the renewal path decision 8 refuses.
	if years := cert.NotAfter.Sub(cert.NotBefore).Hours() / 24 / 365; years < 5 {
		t.Fatalf("client certificate validity = %.1f years, want a multi-year lifetime (revocation is the control, not expiry)", years)
	}
}

// Fingerprints are the identity the listener resolves and the value the audit
// log keeps after an enrolment is deleted, so they are never truncated.
func TestFingerprint_IsFullLengthSHA256(t *testing.T) {
	mkEmptySandboxRelayHome(t)
	ca, err := LoadOrCreateCA(testSealer())
	assertNoErr(t, err, "LoadOrCreateCA")

	_, _, fpA, err := ca.IssueClientCert("hermes-mail")
	assertNoErr(t, err, "issue A")
	_, _, fpB, err := ca.IssueClientCert("hermes-cal")
	assertNoErr(t, err, "issue B")

	for _, fp := range []string{fpA, fpB} {
		if !strings.HasPrefix(fp, "sha256:") {
			t.Fatalf("fingerprint %q missing sha256: prefix", fp)
		}
		if hexPart := strings.TrimPrefix(fp, "sha256:"); len(hexPart) != 64 {
			t.Fatalf("fingerprint hex length = %d, want the full 64 (never truncate — see FingerprintDER)", len(hexPart))
		}
	}
	if fpA == fpB {
		t.Fatal("two certificates produced the same fingerprint")
	}
	if FingerprintCert(nil) != "" {
		t.Fatal("FingerprintCert(nil) must be empty, not a hash of nothing")
	}
}

// The listener needs a server certificate the client can verify with the
// ca.crt in its bundle; issuing one is the CA's job, not the listener's.
func TestIssueServerCert_VerifiesAgainstCAForLoopback(t *testing.T) {
	mkEmptySandboxRelayHome(t)
	ca, err := LoadOrCreateCA(testSealer())
	assertNoErr(t, err, "LoadOrCreateCA")

	tlsCert, err := ca.IssueServerCert()
	assertNoErr(t, err, "IssueServerCert")
	if tlsCert.Leaf == nil {
		t.Fatal("server certificate has no parsed Leaf")
	}
	if _, err := tlsCert.Leaf.Verify(x509.VerifyOptions{
		Roots:     ca.Pool(),
		DNSName:   "localhost",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("default server certificate does not verify for localhost: %v", err)
	}
}

// AC-23: SignClientCSR overrides subject, EKU, serial and validity exactly
// like IssueClientCert, and only the CSR's own public key crosses into the
// certificate — no SAN, extension or subject component the CSR requested
// survives.
func TestRelayCA_SignClientCSR_OverridesEverythingButThePublicKey(t *testing.T) {
	mkEmptySandboxRelayHome(t)
	ca, err := LoadOrCreateCA(testSealer())
	assertNoErr(t, err, "LoadOrCreateCA")

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	assertNoErr(t, err, "generate client key")
	tmpl := &x509.CertificateRequest{
		Subject:        pkix.Name{CommonName: "csr-supplied-cn", Organization: []string{"csr-supplied-org"}},
		DNSNames:       []string{"evil.example"},
		EmailAddresses: []string{"evil@example.com"},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	assertNoErr(t, err, "CreateCertificateRequest")
	csr, err := x509.ParseCertificateRequest(der)
	assertNoErr(t, err, "ParseCertificateRequest")

	before := time.Now()
	certPEM, fingerprint, err := ca.SignClientCSR(csr, "hermes-mail")
	assertNoErr(t, err, "SignClientCSR")

	cert := parseCertPEM(t, certPEM)
	if cert.Subject.CommonName != "hermes-mail" {
		t.Fatalf("CommonName = %q, want the --client-id, not the CSR's own CN", cert.Subject.CommonName)
	}
	if len(cert.Subject.Organization) != 1 || cert.Subject.Organization[0] != "relay" {
		t.Fatalf("Organization = %v, want [relay]", cert.Subject.Organization)
	}
	if len(cert.Subject.OrganizationalUnit) != 1 || cert.Subject.OrganizationalUnit[0] != "relay-enrolment" {
		t.Fatalf("OrganizationalUnit = %v, want [relay-enrolment]", cert.Subject.OrganizationalUnit)
	}
	if len(cert.DNSNames) != 0 || len(cert.EmailAddresses) != 0 {
		t.Fatalf("a SAN carried over from the CSR: dns=%v email=%v", cert.DNSNames, cert.EmailAddresses)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("ExtKeyUsage = %v, want [ClientAuth]", cert.ExtKeyUsage)
	}
	if !cert.BasicConstraintsValid || cert.IsCA {
		t.Fatalf("BasicConstraintsValid/IsCA = %v/%v, want true/false", cert.BasicConstraintsValid, cert.IsCA)
	}
	if cert.SerialNumber == nil || cert.SerialNumber.Sign() == 0 {
		t.Fatal("serial number is zero")
	}
	wantNotAfter := before.Add(clientCertValidity)
	if diff := cert.NotAfter.Sub(wantNotAfter); diff < -time.Hour || diff > time.Hour {
		t.Fatalf("NotAfter = %s, want ~= now + clientCertValidity (%s)", cert.NotAfter, wantNotAfter)
	}
	if string(cert.RawSubjectPublicKeyInfo) != string(csr.RawSubjectPublicKeyInfo) {
		t.Fatal("the certificate's public key is not byte-identical to the CSR's")
	}
	if fingerprint != FingerprintCert(cert) {
		t.Fatalf("returned fingerprint %q != FingerprintCert(cert) %q", fingerprint, FingerprintCert(cert))
	}
}

// SignClientCSR refuses a non-ECDSA public key rather than panicking or
// silently mis-issuing — belt and braces behind ParseClientCSR's own
// refusal, since signLeaf itself only accepts *ecdsa.PublicKey.
func TestRelayCA_SignClientCSR_RefusesNonECDSAKey(t *testing.T) {
	mkEmptySandboxRelayHome(t)
	ca, err := LoadOrCreateCA(testSealer())
	assertNoErr(t, err, "LoadOrCreateCA")

	// A CertificateRequest built by hand, bypassing ParseClientCSR, with an
	// Ed25519 public key set directly — SignClientCSR must not trust that
	// its caller already validated the key type.
	csr := &x509.CertificateRequest{PublicKey: []byte("not an ecdsa key")}
	if _, _, err := ca.SignClientCSR(csr, "hermes-mail"); err == nil {
		t.Fatal("SignClientCSR must refuse a non-ECDSA public key")
	}
}

// AC-30: `relay enrol ca-fingerprint`'s value (CAFingerprintFromDisk, which
// reads ca.crt straight off disk with no sealer at all) and the value a
// client pins (RelayCA.CertFingerprint, from the loaded CA the tray holds)
// are byte-identical for the same CA.
func TestCAFingerprint_DiskReadMatchesLoadedCA(t *testing.T) {
	mkEmptySandboxRelayHome(t)

	ca, err := LoadOrCreateCA(testSealer())
	assertNoErr(t, err, "LoadOrCreateCA")

	fromDisk, err := CAFingerprintFromDisk()
	assertNoErr(t, err, "CAFingerprintFromDisk")

	if fromDisk != ca.CertFingerprint() {
		t.Fatalf("CAFingerprintFromDisk() = %q, want %q (RelayCA.CertFingerprint of the same CA)", fromDisk, ca.CertFingerprint())
	}
	if !strings.HasPrefix(fromDisk, "sha256:") {
		t.Fatalf("fingerprint %q lacks the sha256: prefix", fromDisk)
	}
}

// CAFingerprintFromDisk must never need a sealer: it is the one enrol
// subcommand a CLI process can answer without dialing the tray.
func TestCAFingerprint_RefusesNamingTheFixWhenNoCAExistsYet(t *testing.T) {
	mkEmptySandboxRelayHome(t)
	_, err := CAFingerprintFromDisk()
	if err == nil || !strings.Contains(err.Error(), "relay enrol") {
		t.Fatalf("err = %v, want a refusal naming a `relay enrol` command to run first", err)
	}
}

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"relaygo/bridge"
)

// Relay is its own certificate authority for remote clients (ADR-010
// decision 8). A two-machine deployment does not warrant the ceremony of an
// external CA, and the CA never leaves the host: only the certificates it
// signs travel.
//
// The CA material lives as PEM files in the config dir rather than inline in
// settings.json. settings.json is rewritten atomically in full on every
// mutation (see FileSettingsStore.save) — a key and a certificate embedded
// there would be re-serialized on every project rename, and would ride along
// in every deepCopySettings round-trip and every settings DTO that forgets to
// strip them. A file the CA code opens by name is both smaller and harder to
// leak by accident.
const (
	caKeyFile  = "ca.key"
	caCertFile = "ca.crt"

	// The CA outlives the certificates it signs by a wide margin: renewing it
	// means re-enrolling every client, which is exactly the manual ceremony
	// decision 8 accepts once and does not want to repeat on a timer.
	caValidity = 20 * 365 * 24 * time.Hour

	// Client certificates are deliberately long-lived. Decision 8:
	// "revocation rather than expiry is the control". A short lifetime would
	// need an authenticated renewal path, and any credential that can be
	// replayed to obtain a fresh certificate reintroduces precisely the
	// bearer secret decision 2 removed — at the one point in the system where
	// the result is a new identity rather than a single call. Ten years is
	// long enough that expiry never silently cuts a working agent, which
	// keeps revocation the only mechanism anyone has to reason about.
	clientCertValidity = 10 * 365 * 24 * time.Hour

	// Server certificates are issued on demand by the listener rather than
	// persisted, so their lifetime only has to cover a single relay process's
	// uptime with room to spare. The client verifies them against the CA, so
	// rotating one costs nothing on the client side.
	serverCertValidity = 5 * 365 * 24 * time.Hour
)

// RelayCA is relay's self-signed certificate authority: the private key, the
// self-signed certificate, and the PEM encoding of that certificate (which is
// what ships to a client so it can verify the server).
//
// Obtain one with LoadOrCreateCA. A RelayCA is immutable after construction
// and safe for concurrent use.
type RelayCA struct {
	key     *ecdsa.PrivateKey
	cert    *x509.Certificate
	certPEM []byte
}

// caMu serializes LoadOrCreateCA so two callers racing on first use cannot
// each generate a CA and have the loser's certificate silently overwrite the
// winner's — which would invalidate every certificate signed in between.
var caMu sync.Mutex

// caPaths returns the on-disk locations of the CA key and certificate. Both
// live in bridge.ConfigDir(), so --config-dir and the test sandbox reroute
// them together with settings.json.
func caPaths() (keyPath, certPath string) {
	dir := bridge.ConfigDir()
	return filepath.Join(dir, caKeyFile), filepath.Join(dir, caCertFile)
}

// LoadOrCreateCA returns relay's CA, generating and persisting it on first
// use. Generation is lazy rather than part of startup: an install that never
// enrols a remote client never grows a signing key it has no use for.
//
// Idempotent — a second call reads the persisted PEM back rather than signing
// a new CA, so certificates issued by an earlier call keep verifying.
func LoadOrCreateCA() (*RelayCA, error) {
	caMu.Lock()
	defer caMu.Unlock()

	keyPath, certPath := caPaths()
	ca, err := loadCA(keyPath, certPath)
	if err == nil {
		return ca, nil
	}
	if !os.IsNotExist(err) {
		// A present-but-unreadable CA is not something to paper over by
		// minting a new one: that would silently invalidate every enrolment
		// on the host. Surface it and let the operator decide.
		return nil, err
	}
	return generateCA(keyPath, certPath)
}

// loadCA reads an existing CA from disk. Returns an os.IsNotExist error when
// either file is missing, which is LoadOrCreateCA's signal to generate.
func loadCA(keyPath, certPath string) (*RelayCA, error) {
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("ca key %s is not valid PEM", keyPath)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse ca key %s: %w", keyPath, err)
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("ca certificate %s is not valid PEM", certPath)
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse ca certificate %s: %w", certPath, err)
	}
	return &RelayCA{key: key, cert: cert, certPEM: certPEM}, nil
}

// generateCA mints a fresh self-signed CA and writes it to disk 0600.
func generateCA(keyPath, certPath string) (*RelayCA, error) {
	// ECDSA P-256 rather than Ed25519: both ends are ours today, but a CA is
	// the one artifact here that has to keep verifying for as long as any
	// certificate it signed, and P-256 is accepted by every TLS stack a
	// future client might be written in.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ca key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "relay local CA", Organization: []string{"relay"}},
		NotBefore:             now.Add(-5 * time.Minute), // tolerate a client clock a few minutes behind
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true, // this CA signs leaves only; it never delegates
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("self-sign ca certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse generated ca certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal ca key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	// The key goes down first and durably: a certificate on disk whose key is
	// missing would make LoadOrCreateCA fail every subsequent call rather
	// than regenerate, since a half-written CA is not os.IsNotExist.
	if err := atomicWriteFile(keyPath, keyPEM, 0600); err != nil {
		return nil, fmt.Errorf("write ca key: %w", err)
	}
	if err := atomicWriteFile(certPath, certPEM, 0600); err != nil {
		return nil, fmt.Errorf("write ca certificate: %w", err)
	}
	return &RelayCA{key: key, cert: cert, certPEM: certPEM}, nil
}

// CertPEM returns the PEM-encoded CA certificate. This is the copy that ships
// in an enrolment bundle so the client can verify relay's server certificate.
func (ca *RelayCA) CertPEM() []byte {
	out := make([]byte, len(ca.certPEM))
	copy(out, ca.certPEM)
	return out
}

// Pool returns a x509.CertPool containing only relay's CA — the value a
// RemoteServer hands to tls.Config.ClientCAs so that no certificate signed by
// anything else can complete a handshake.
func (ca *RelayCA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return pool
}

// IssueClientCert signs a client certificate for an enrolment's client id and
// returns its PEM key, PEM certificate, and full fingerprint.
//
// The client id is the certificate's Common Name, so the identity relay
// records is bound into the credential itself rather than sitting only in a
// settings.json row beside it. Resolution at connection time still goes
// through the fingerprint (FindEnrolmentByFingerprint) — a CN is chosen by
// whoever signs, and only relay signs, but the fingerprint is what cannot be
// re-used by a second certificate claiming the same name.
func (ca *RelayCA) IssueClientCert(clientID string) (keyPEM, certPEM []byte, fingerprint string, err error) {
	if clientID == "" {
		return nil, nil, "", fmt.Errorf("client id is required")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, "", fmt.Errorf("generate client key: %w", err)
	}
	der, err := ca.signLeaf(&key.PublicKey, pkix.Name{
		CommonName:         clientID,
		Organization:       []string{"relay"},
		OrganizationalUnit: []string{"relay-enrolment"},
	}, clientCertValidity, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)
	if err != nil {
		return nil, nil, "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, "", fmt.Errorf("marshal client key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return keyPEM, certPEM, FingerprintDER(der), nil
}

// IssueServerCert signs a short-chain server certificate for the given hosts,
// ready to drop into tls.Config.Certificates. Deliberately not persisted: the
// client verifies it against the CA cert in its bundle, so a fresh one per
// process start costs the client nothing and gives the host one fewer private
// key sitting on disk.
//
// hosts may name IPs or DNS names; an empty list defaults to loopback, which
// matches RemoteServer's default bind (decision 9).
func (ca *RelayCA) IssueServerCert(hosts ...string) (tls.Certificate, error) {
	if len(hosts) == 0 {
		hosts = []string{"127.0.0.1", "::1", "localhost"}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate server key: %w", err)
	}
	der, err := ca.signLeaf(&key.PublicKey, pkix.Name{
		CommonName:   "relay",
		Organization: []string{"relay"},
	}, serverCertValidity, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, hosts)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse server certificate: %w", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}

// signLeaf is the one place a leaf certificate is created, so client and
// server certs cannot drift in serial generation, clock skew tolerance, or
// basic-constraints handling. hosts, when non-empty, become IP/DNS SANs.
func (ca *RelayCA) signLeaf(pub *ecdsa.PublicKey, subject pkix.Name, validity time.Duration, eku []x509.ExtKeyUsage, hosts []string) ([]byte, error) {
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               subject,
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           eku,
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			continue
		}
		tmpl.DNSNames = append(tmpl.DNSNames, h)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		return nil, fmt.Errorf("sign certificate: %w", err)
	}
	return der, nil
}

// randomSerial returns a 128-bit positive certificate serial number.
func randomSerial() (*big.Int, error) {
	max := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	// A zero serial is legal-ish but universally disliked by TLS stacks.
	return serial.Add(serial, big.NewInt(1)), nil
}

// FingerprintDER returns the canonical fingerprint of a DER-encoded
// certificate: "sha256:" followed by the FULL 64 hex characters.
//
// Never truncate this. Decision 6 records the fingerprint in full precisely so
// a revoked device's history stays legible after the enrolment naming it has
// been deleted — a shortened fingerprint answers "probably that key" where the
// whole point is answering "that key". It is also the value the listener
// resolves against an enrolment, and a prefix match on identity is a match on
// something an attacker gets to grind.
func FingerprintDER(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// FingerprintCert is FingerprintDER for an already-parsed certificate — what
// RemoteServer calls on the peer certificate of an accepted connection.
func FingerprintCert(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	return FingerprintDER(cert.Raw)
}

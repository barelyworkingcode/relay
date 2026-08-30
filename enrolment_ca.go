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
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"relaygo/bridge"
	"relaygo/sealed"
)

const (
	caKeyFile = "ca.key"
	// caKeySealedFile replaces caKeyFile once migration runs (§4.7 step 4)
	// or a fresh CA is generated (§5.7 clause 3): relay never writes a
	// plaintext ca.key again after either.
	caKeySealedFile = "ca.key.sealed"
	caCertFile      = "ca.crt"

	caValidity = 20 * 365 * 24 * time.Hour

	// Client certificates are deliberately long-lived: revocation, not
	// expiry, is the control. A short lifetime would need an authenticated
	// renewal path, and any credential that can be replayed to obtain a
	// fresh certificate reintroduces a bearer secret at the one point where
	// the result is a new identity rather than a single call. Ten years is
	// long enough that expiry never silently cuts a working agent, which
	// keeps revocation the only mechanism anyone has to reason about.
	clientCertValidity = 10 * 365 * 24 * time.Hour

	serverCertValidity = 5 * 365 * 24 * time.Hour
)

type RelayCA struct {
	key     *ecdsa.PrivateKey
	cert    *x509.Certificate
	certPEM []byte
}

var caMu sync.Mutex

func caPaths() (keyPath, sealedKeyPath, certPath string) {
	dir := bridge.ConfigDir()
	return filepath.Join(dir, caKeyFile), filepath.Join(dir, caKeySealedFile), filepath.Join(dir, caCertFile)
}

// LoadOrCreateCA resolves relay's CA per §5.7: a sealed key wins if one
// exists, a plaintext one is folded into ca.key.sealed and removed (§4.7),
// and only when neither exists is a fresh CA generated — sealed from the
// start, never as a plaintext ca.key.
//
// A nil sealer (the CLI's shape, or the tray degraded per §5.6) can still
// read an already-parsed *RelayCA nothing here needs to open, but cannot
// open ca.key.sealed or seal a freshly migrated or generated one; every
// such path names the degraded state instead of falling through to
// generating a new CA, which would silently invalidate every enrolment on
// the host.
func LoadOrCreateCA(sealer sealed.Sealer) (*RelayCA, error) {
	caMu.Lock()
	defer caMu.Unlock()

	keyPath, sealedKeyPath, certPath := caPaths()

	if _, err := os.Stat(sealedKeyPath); err == nil {
		return loadSealedCA(sealedKeyPath, certPath, sealer)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat %s: %w", sealedKeyPath, err)
	}

	if _, err := os.Stat(keyPath); err == nil {
		if sealer == nil {
			return nil, fmt.Errorf("%w: ca.key exists in plaintext but there is no sealer to migrate it", errSealUnavailable)
		}
		if err := migrateCAKey(filepath.Dir(keyPath), sealer); err != nil {
			return nil, err
		}
		return loadSealedCA(sealedKeyPath, certPath, sealer)
	} else if !os.IsNotExist(err) {
		// A present-but-unreadable CA is not something to paper over by
		// minting a new one: that would silently invalidate every
		// enrolment on the host. Surface it and let the operator decide.
		return nil, err
	}

	if sealer == nil {
		return nil, fmt.Errorf("%w: no CA exists yet and there is no sealer to create one under", errSealUnavailable)
	}
	return generateCA(sealedKeyPath, certPath, sealer)
}

func loadSealedCA(sealedKeyPath, certPath string, sealer sealed.Sealer) (*RelayCA, error) {
	if sealer == nil {
		return nil, fmt.Errorf("%w: ca.key.sealed exists but there is no sealer to open it", errSealUnavailable)
	}
	data, err := os.ReadFile(sealedKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", sealedKeyPath, err)
	}
	var env sealed.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("%s is not a valid sealed envelope: %w", sealedKeyPath, err)
	}
	keyPEM, err := sealer.Unseal(env, []byte(caAADPrefix+"ca.key"))
	if err != nil {
		return nil, fmt.Errorf("%s could not be opened: %w", sealedKeyPath, err)
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	return parseCA(keyPEM, certPEM, sealedKeyPath, certPath)
}

func parseCA(keyPEM, certPEM []byte, keyName, certPath string) (*RelayCA, error) {
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("ca key %s is not valid PEM", keyName)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse ca key %s: %w", keyName, err)
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

func generateCA(sealedKeyPath, certPath string, sealer sealed.Sealer) (*RelayCA, error) {
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

	if err := os.MkdirAll(filepath.Dir(sealedKeyPath), 0700); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	// Sealed from the start: a freshly generated CA never has a plaintext
	// ca.key written for it (§5.7 clause 3).
	if err := sealCAKeyFile(filepath.Dir(sealedKeyPath), sealer, keyPEM); err != nil {
		return nil, err
	}
	if err := atomicWriteFile(certPath, certPEM, 0600); err != nil {
		return nil, fmt.Errorf("write ca certificate: %w", err)
	}
	return &RelayCA{key: key, cert: cert, certPEM: certPEM}, nil
}

func (ca *RelayCA) CertPEM() []byte {
	out := make([]byte, len(ca.certPEM))
	copy(out, ca.certPEM)
	return out
}

func (ca *RelayCA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return pool
}

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

// SignClientCSR is IssueClientCert minus key generation: the CSR's own
// public key crosses into the certificate and nothing else from the
// request does. Subject, serial, validity, KeyUsage and ExtKeyUsage are
// all relay's, byte-identical to IssueClientCert, so the two paths issue
// certificates a caller cannot tell apart by shape — only by whether relay
// or the client generated the key underneath.
func (ca *RelayCA) SignClientCSR(csr *x509.CertificateRequest, clientID string) (certPEM []byte, fingerprint string, err error) {
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, "", fmt.Errorf("csr public key is %T, not *ecdsa.PublicKey — ParseClientCSR should have refused this already", csr.PublicKey)
	}
	der, err := ca.signLeaf(pub, pkix.Name{
		CommonName:         clientID,
		Organization:       []string{"relay"},
		OrganizationalUnit: []string{"relay-enrolment"},
	}, clientCertValidity, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)
	if err != nil {
		return nil, "", err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return certPEM, FingerprintDER(der), nil
}

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

func randomSerial() (*big.Int, error) {
	max := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	// A zero serial is legal-ish but universally disliked by TLS stacks.
	return serial.Add(serial, big.NewInt(1)), nil
}

// FingerprintDER is "sha256:" followed by the full 64 hex characters. Never
// truncate this: the fingerprint is recorded in full precisely so a
// revoked device's history stays legible after the enrolment naming it has
// been deleted — a shortened fingerprint answers "probably that key" where
// the whole point is answering "that key". It is also the value the
// listener resolves against an enrolment, and a prefix match on identity
// is a match on something an attacker gets to grind.
func FingerprintDER(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func FingerprintCert(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	return FingerprintDER(cert.Raw)
}

// SPKISHA256Hex is the hex SHA-256 of a certificate request's
// SubjectPublicKeyInfo — the value bound into both the presence digest and
// the stored Enrolment.SPKISHA256 field, so the same private key cannot be
// enrolled twice under two client ids (§1.4, §1.5 of the CSR enrolment
// spec).
func SPKISHA256Hex(rawSPKI []byte) string {
	sum := sha256.Sum256(rawSPKI)
	return hex.EncodeToString(sum[:])
}

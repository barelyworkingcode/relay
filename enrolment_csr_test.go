package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

// genCSRPEMFromKey builds a PKCS#10 request PEM over key, letting the
// caller override the signature algorithm (0 picks the default for the key
// type) — the one knob the ECDSA-with-SHA1 negative case needs.
func genCSRPEMFromKey(t *testing.T, key crypto.Signer, sigAlg x509.SignatureAlgorithm, cn string) []byte {
	t.Helper()
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}, SignatureAlgorithm: sigAlg}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	assertNoErr(t, err, "CreateCertificateRequest")
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

// genClientCSRPEM is the fixture every other package test (enrolment_test.go,
// enrol_cmd_test.go, presence_gate_wiring_test.go) reaches for when it needs
// a real, valid CSR rather than a negative case of its own.
func genClientCSRPEM(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	assertNoErr(t, err, "generate P-256 key")
	return genCSRPEMFromKey(t, key, 0, cn)
}

// tamperCSRSignature flips the last byte of a valid CSR's signature, which
// leaves the ASN.1 structure valid but the signature no longer verifiable —
// the "the requester does not hold this key" case, distinct from "the bytes
// don't even parse."
func tamperCSRSignature(t *testing.T, csrPEM []byte) []byte {
	t.Helper()
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		t.Fatal("tamperCSRSignature: not valid PEM")
	}
	var req struct {
		Info      asn1.RawValue
		AlgID     asn1.RawValue
		Signature asn1.BitString
	}
	_, err := asn1.Unmarshal(block.Bytes, &req)
	assertNoErr(t, err, "asn1.Unmarshal CSR")
	tampered := append([]byte(nil), req.Signature.Bytes...)
	tampered[len(tampered)-1] ^= 0xFF
	req.Signature.Bytes = tampered
	out, err := asn1.Marshal(req)
	assertNoErr(t, err, "asn1.Marshal tampered CSR")
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: out})
}

func genSelfSignedCertPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	assertNoErr(t, err, "generate key")
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-cert"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	assertNoErr(t, err, "CreateCertificate")
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// A valid P-256 CSR parses, and its public key round-trips into the
// returned request unchanged — the positive control every negative case
// below is measured against.
func TestParseClientCSR_ValidRequestParses(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	assertNoErr(t, err, "generate key")
	csrPEM := genCSRPEMFromKey(t, key, 0, "hermes")

	csr, err := ParseClientCSR(csrPEM)
	assertNoErr(t, err, "ParseClientCSR")
	if csr.Subject.CommonName != "hermes" {
		t.Fatalf("CommonName = %q, want hermes", csr.Subject.CommonName)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		t.Fatalf("parsed CSR's public key does not match the signing key")
	}
}

// AC-14 through AC-19: every negative case, independently, asserting the
// message names the file/problem and the fix.
func TestParseClientCSR_Negatives(t *testing.T) {
	validPEM := genClientCSRPEM(t, "hermes")
	second := genClientCSRPEM(t, "hermes-2")

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	assertNoErr(t, err, "generate RSA key")
	rsaPEM := genCSRPEMFromKey(t, rsaKey, 0, "rsa-client")

	_, edPriv, err := ed25519.GenerateKey(rand.Reader)
	assertNoErr(t, err, "generate ed25519 key")
	edPEM := genCSRPEMFromKey(t, edPriv, 0, "ed25519-client")

	p384Key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	assertNoErr(t, err, "generate P-384 key")
	p384PEM := genCSRPEMFromKey(t, p384Key, 0, "p384-client")

	sha1Key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	assertNoErr(t, err, "generate P-256 key for SHA1 test")
	sha1PEM := genCSRPEMFromKey(t, sha1Key, x509.ECDSAWithSHA1, "sha1-client")

	oversized := make([]byte, maxCSRBytes+1)

	// A PEM block honestly labelled something other than "CERTIFICATE
	// REQUEST" (and other than "CERTIFICATE", which gets its own message),
	// wrapping bytes that ARE a valid PKCS#10 request. If the block-type
	// check ever went missing, x509.ParseCertificateRequest would parse
	// these bytes just fine and this case would silently start passing.
	// Appended at the end of the table, not spliced in, so it cannot shift
	// the indices tests[5:8] below relies on.
	validBlock, _ := pem.Decode(validPEM)
	mislabeledPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: validBlock.Bytes})

	tests := []struct {
		name    string
		in      []byte
		wantErr string
	}{
		{"garbage bytes, not PEM at all", []byte("this is not a CSR, just some text"), "not a parseable"},
		{"garbage bytes over the size cap, refused before parsing", oversized, "a few hundred bytes"},
		{"a certificate, not a signing request", genSelfSignedCertPEM(t), "that is a certificate, not a signing request"},
		{"trailing bytes after a valid block", append(append([]byte{}, validPEM...), second...), "trailing data"},
		{"signature does not verify against its own key", tamperCSRSignature(t, validPEM), "not proof the requester holds the private key"},
		{"RSA-2048 key", rsaPEM, "RSA-2048"},
		{"Ed25519 key", edPEM, "Ed25519"},
		{"ECDSA P-384 key", p384PEM, "P-384"},
		{"a differently-labelled PEM block wrapping a valid request", mislabeledPEM, "unexpected PEM block type"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseClientCSR(tc.in)
			if err == nil {
				t.Fatalf("want a refusal, got none")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}

	// AC-16's ECDSA P-256 acceptance and the ECDSA-with-SHA1 negative both
	// name what relay accepts.
	for _, tc := range tests[5:8] {
		_, err := ParseClientCSR(tc.in)
		if !strings.Contains(err.Error(), "P-256") {
			t.Errorf("%s: error = %q, want it to name P-256 as what is accepted", tc.name, err.Error())
		}
	}

	// AC-17: ECDSA-with-SHA1 is refused via CheckSignature's own
	// InsecureAlgorithmError, not a second algorithm check relay wrote.
	_, err = ParseClientCSR(sha1PEM)
	var algErr x509.InsecureAlgorithmError
	if !errors.As(err, &algErr) {
		t.Fatalf("ECDSA-with-SHA1 CSR: err = %v, want it to wrap x509.InsecureAlgorithmError", err)
	}
}

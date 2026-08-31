package main

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// maxCSRBytes bounds a CSR at "a few hundred bytes" times generous
// headroom: large enough for any legitimate PKCS#10 request, small enough
// that an operator who carried in the wrong file sees a refusal instead of
// relay parsing whatever else was on the USB stick.
const maxCSRBytes = 16 << 10

// csrTooLargeMessage is shared with the CLI's own local pre-check
// (parseEnrolSignFlags' readCSRFile), so an operator sees the identical
// wording whether the file is refused before the round trip or, belt and
// braces, inside it.
func csrTooLargeMessage(n int) string {
	return fmt.Sprintf("a certificate signing request is a few hundred bytes; this is %d — is this the right file?", n)
}

// ParseClientCSR validates a CSR an operator carried in before anything in
// enrolment_ca.go ever sees it. Pure: no store, no CA, no sealer, no
// filesystem — every refusal here is decidable from the bytes alone, and
// every rule below runs in this order (§1.3): a cheap size check first, PEM
// shape, no trailing data, ASN.1 parse, proof of possession, and only then
// the key type.
func ParseClientCSR(pemBytes []byte) (*x509.CertificateRequest, error) {
	if len(pemBytes) > maxCSRBytes {
		return nil, fmt.Errorf("%s", csrTooLargeMessage(len(pemBytes)))
	}

	block, rest := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("not a parseable PKCS#10 request: no PEM block found")
	}
	if block.Type == "CERTIFICATE" {
		return nil, fmt.Errorf("that is a certificate, not a signing request")
	}
	if block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("not a parseable PKCS#10 request: unexpected PEM block type %q", block.Type)
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, fmt.Errorf("trailing data after the signing request: is this two requests concatenated into one file?")
	}

	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("not a parseable PKCS#10 request: %w", err)
	}

	// CheckSignature is the proof-of-possession check: it proves the
	// requester holds the private key matching the public key it enclosed,
	// which is what makes it safe to accept a CSR carried in on an
	// operator's USB stick rather than presented over an authenticated
	// channel.
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("the request is not signed by the key it carries, so it is not proof the requester holds the private key: %w", err)
	}
	// This is subtle: crypto/x509's CheckSignature allows a SHA-1 signature
	// for a CertificateRequest even though it refuses one on a Certificate —
	// SHA-1 is permitted for CSRs and CRLs in the standard library, not
	// refused by CheckSignature itself. A client certificate signed off
	// this CSR lives for clientCertValidity (a decade), so relay checks
	// explicitly rather than trusting a proof-of-possession signature
	// standard library policy would let through.
	if csr.SignatureAlgorithm == x509.ECDSAWithSHA1 {
		return nil, fmt.Errorf("the request is signed with SHA-1, which relay refuses regardless of what crypto/x509 permits for a CSR: %w", x509.InsecureAlgorithmError(csr.SignatureAlgorithm))
	}

	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("the request's key is %s; relay only accepts ECDSA P-256 (see `relayremote enrol`)", describeCSRKeyType(csr.PublicKey))
	}
	if pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("the request's key is ECDSA %s; relay only accepts ECDSA P-256 (see `relayremote enrol`)", pub.Curve.Params().Name)
	}

	// Not refused, deliberately: everything else in the request — subject,
	// SANs, requested extensions, the challenge-password attribute — is
	// never read. relay overrides the subject wholesale in SignClientCSR;
	// refusing here on some unknown attribute would make relay's
	// acceptance depend on which openssl or library produced the file,
	// for no security benefit, since none of it is trusted anyway.
	return csr, nil
}

func describeCSRKeyType(pub crypto.PublicKey) string {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		return fmt.Sprintf("RSA-%d", k.N.BitLen())
	case ed25519.PublicKey:
		return "Ed25519"
	default:
		return fmt.Sprintf("%T", pub)
	}
}

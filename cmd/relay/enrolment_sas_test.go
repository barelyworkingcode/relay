package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sasVectorsSHA256 pins testdata/sas_vectors.json, and the identical constant
// stands in relayRemote's sas_test.go. The two repositories share no module,
// so this file is the only thing tying the two implementations together; a
// divergence surfaces as two different six-character codes on two screens,
// which this feature has trained the operator to read as an attack.
const sasVectorsSHA256 = "584e9b4f7f6be2c0fd026205ba8d142c3d10665ae92867dfaefee06870a5347a"

func sasVectorsPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "testdata", "sas_vectors.json")
}

type sasVector struct {
	Name    string `json:"name"`
	CASPKI  string `json:"ca_spki"`
	CSRSPKI string `json:"csr_spki"`
	RC      string `json:"rc"`
	RR      string `json:"rr"`
	SAS     string `json:"sas"`
	Commit  string `json:"commit"`
}

func loadSASVectors(t *testing.T) []sasVector {
	t.Helper()
	path := sasVectorsPath(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Vectors []sasVector `json:"vectors"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc.Vectors
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

// AC-1
func TestSASVectorFileIsPinned(t *testing.T) {
	path := sasVectorsPath(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != sasVectorsSHA256 {
		t.Fatalf("%s sha256 = %s, want %s\nthe shared vector file has diverged from relayRemote's copy",
			path, got, sasVectorsSHA256)
	}
}

// AC-1
func TestSASVectorsMatchImplementation(t *testing.T) {
	vectors := loadSASVectors(t)
	if len(vectors) < 20 {
		t.Fatalf("got %d vectors, want at least 20", len(vectors))
	}
	seen := map[byte]bool{}
	for _, v := range vectors {
		caSPKI := mustHex(t, v.CASPKI)
		csrSPKI := mustHex(t, v.CSRSPKI)
		rc := mustHex(t, v.RC)
		rr := mustHex(t, v.RR)

		if got := computeSAS(caSPKI, csrSPKI, rc, rr); got != v.SAS {
			t.Errorf("%s: computeSAS = %q, want %q", v.Name, got, v.SAS)
		}
		if got := sasCommitment(sha256.Sum256(csrSPKI), rc); got != v.Commit {
			t.Errorf("%s: sasCommitment = %q, want %q", v.Name, got, v.Commit)
		}
		if len(v.SAS) != 6 {
			t.Errorf("%s: vector sas %q is not 6 characters", v.Name, v.SAS)
		}
		if !validSASHex(v.Commit, sha256.Size) {
			t.Errorf("%s: vector commit %q is not 64 lowercase hex", v.Name, v.Commit)
		}
		for i := 0; i < len(v.SAS); i++ {
			seen[v.SAS[i]] = true
		}
	}
	for i := 0; i < len(sasAlphabet); i++ {
		if !seen[sasAlphabet[i]] {
			t.Errorf("no vector exercises alphabet symbol %q", sasAlphabet[i])
		}
	}
}

// AC-2
func TestSASAlphabetHasNoConfusablePair(t *testing.T) {
	if len(sasAlphabet) != 32 {
		t.Fatalf("alphabet length %d, want 32", len(sasAlphabet))
	}
	seen := map[byte]bool{}
	for i := 0; i < len(sasAlphabet); i++ {
		c := sasAlphabet[i]
		switch {
		case seen[c]:
			t.Errorf("duplicate symbol %q", c)
		case c == '0' || c == '1' || c == 'I' || c == 'O':
			t.Errorf("confusable symbol %q is present", c)
		case !(c >= '2' && c <= '9') && !(c >= 'A' && c <= 'Z'):
			t.Errorf("symbol %q is neither an uppercase letter nor a digit 2-9", c)
		}
		seen[c] = true
	}
}

func flipDigestBit(d [32]byte, bit int) [32]byte {
	d[bit/8] ^= 1 << (7 - uint(bit%8))
	return d
}

// AC-3
func TestSASEncodingIsSixCharactersOfThirtyBits(t *testing.T) {
	rng := rand.New(rand.NewSource(0x5A5A5A5A))
	for i := 0; i < 10000; i++ {
		var d [32]byte
		rng.Read(d[:])
		out := enc30(d)
		if len(out) != 6 {
			t.Fatalf("enc30 returned %d characters: %q", len(out), out)
		}
		for j := 0; j < len(out); j++ {
			if !strings.ContainsRune(sasAlphabet, rune(out[j])) {
				t.Fatalf("character %q of %q is outside the alphabet", out[j], out)
			}
		}
	}
}

// AC-3
func TestSASEncodingDependsOnExactlyTheFirstThirtyBits(t *testing.T) {
	rng := rand.New(rand.NewSource(0x30B175))
	for trial := 0; trial < 256; trial++ {
		var d [32]byte
		rng.Read(d[:])
		base := enc30(d)
		for bit := 0; bit < 256; bit++ {
			got := enc30(flipDigestBit(d, bit))
			if bit < 30 && got == base {
				t.Fatalf("flipping bit %d left the code at %q (digest %x)", bit, base, d)
			}
			if bit >= 30 && got != base {
				t.Fatalf("flipping bit %d moved the code %q -> %q (digest %x)", bit, base, got, d)
			}
		}
	}
}

// AC-4
func TestSASDomainSeparation(t *testing.T) {
	if sasDomain == sasCommitDomain {
		t.Fatal("the two domain tags are equal")
	}
	if len(sasDomain) != 13 || sasDomain[len(sasDomain)-1] != 0 {
		t.Fatalf("sasDomain = %q, want 13 bytes ending in NUL", sasDomain)
	}
	if len(sasCommitDomain) != 20 || sasCommitDomain[len(sasCommitDomain)-1] != 0 {
		t.Fatalf("sasCommitDomain = %q, want 20 bytes ending in NUL", sasCommitDomain)
	}

	rng := rand.New(rand.NewSource(0xD0A1))
	caSPKI := make([]byte, 91)
	csrSPKI := make([]byte, 91)
	rc := make([]byte, sasNonceBytes)
	rr := make([]byte, sasNonceBytes)
	for _, b := range [][]byte{caSPKI, csrSPKI, rc, rr} {
		rng.Read(b)
	}
	caSum := sha256.Sum256(caSPKI)
	csrSum := sha256.Sum256(csrSPKI)

	body := append(append(append(append([]byte{}, caSum[:]...), csrSum[:]...), rc...), rr...)
	withDomain := sha256.Sum256(append([]byte(sasDomain), body...))
	naked := sha256.Sum256(body)

	if got := computeSAS(caSPKI, csrSPKI, rc, rr); got != enc30(withDomain) {
		t.Fatalf("computeSAS = %q, want %q — the domain tag is not in the preimage as written", got, enc30(withDomain))
	}
	if enc30(naked) == computeSAS(caSPKI, csrSPKI, rc, rr) {
		t.Fatal("the code is unchanged without the domain prefix")
	}

	crossed := sha256.Sum256(append(append([]byte(sasDomain), csrSum[:]...), rc...))
	if sasCommitment(csrSum, rc) == hex.EncodeToString(crossed[:]) {
		t.Fatal("sasCommitment uses the SAS domain tag rather than the commitment one")
	}
}

// AC-5
func TestSASCommitmentBindsTheKey(t *testing.T) {
	rng := rand.New(rand.NewSource(0xC0117))
	rc := make([]byte, sasNonceBytes)
	a := make([]byte, 91)
	b := make([]byte, 91)
	for i := 0; i < 1000; i++ {
		rng.Read(rc)
		rng.Read(a)
		rng.Read(b)
		sumA := sha256.Sum256(a)
		sumB := sha256.Sum256(b)
		commitA := sasCommitment(sumA, rc)
		if commitA != sasCommitment(sumA, rc) {
			t.Fatal("sasCommitment is not deterministic")
		}
		if commitA == sasCommitment(sumB, rc) {
			t.Fatalf("iteration %d: a commitment over key A verified against key B", i)
		}
	}
}

// AC-7
func TestSASSubstitutionChangesTheCode(t *testing.T) {
	rng := rand.New(rand.NewSource(0xA77ACC))
	caReal := make([]byte, 91)
	csrReal := make([]byte, 91)
	rc := make([]byte, sasNonceBytes)
	rr := make([]byte, sasNonceBytes)
	for _, b := range [][]byte{caReal, csrReal, rc, rr} {
		rng.Read(b)
	}
	honest := computeSAS(caReal, csrReal, rc, rr)

	caAttacker := make([]byte, 91)
	csrAttacker := make([]byte, 91)
	caCollisions, csrCollisions := 0, 0
	for i := 0; i < 1000; i++ {
		rng.Read(caAttacker)
		rng.Read(csrAttacker)
		if computeSAS(caAttacker, csrReal, rc, rr) == honest {
			caCollisions++
			t.Errorf("iteration %d: substituting the CA left the code at %q", i, honest)
		}
		if computeSAS(caReal, csrAttacker, rc, rr) == honest {
			csrCollisions++
			t.Errorf("iteration %d: substituting the CSR left the code at %q", i, honest)
		}
	}
	if caCollisions != 0 || csrCollisions != 0 {
		t.Fatalf("collisions: ca=%d csr=%d, want 0 and 0", caCollisions, csrCollisions)
	}
}

func TestSASNonceIsThirtyTwoLowercaseHex(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 128; i++ {
		n, err := newSASNonce()
		if err != nil {
			t.Fatalf("newSASNonce: %v", err)
		}
		if !validSASHex(n, sasNonceBytes) {
			t.Fatalf("newSASNonce returned %q", n)
		}
		if seen[n] {
			t.Fatalf("newSASNonce repeated %q", n)
		}
		seen[n] = true
	}
}

func TestSASValidHex(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want bool
	}{
		{strings.Repeat("a", 32), 16, true},
		{strings.Repeat("0", 64), 32, true},
		{"0123456789abcdef0123456789abcdef", 16, true},
		{strings.Repeat("A", 32), 16, false},
		{"0123456789ABCDEF0123456789abcdef", 16, false},
		{strings.Repeat("a", 31), 16, false},
		{strings.Repeat("a", 33), 16, false},
		{"", 16, false},
		{strings.Repeat("g", 32), 16, false},
		{"0123456789abcdef0123456789abcde ", 16, false},
	}
	for _, c := range cases {
		if got := validSASHex(c.s, c.n); got != c.want {
			t.Errorf("validSASHex(%q, %d) = %v, want %v", c.s, c.n, got, c.want)
		}
	}
}

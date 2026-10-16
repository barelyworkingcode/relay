package harness

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"
)

const enrolDeadline = 60 * time.Second

// EnrolOpen runs `relay enrol open --profile ID --json` and returns the address
// the window bound, which is the `listen` field it prints. The CLI exits after
// the listener is bound, so the address is dialable on return. The instance
// must approve the enrolment.window.open prompt.
func (i *Instance) EnrolOpen(profileID string) string {
	i.t.Helper()
	var w struct {
		Open   bool   `json:"open"`
		Listen string `json:"listen"`
	}
	i.MustCLI("enrol", "open", "--profile", profileID, "--json").JSON(i.t, &w)
	if !w.Open || w.Listen == "" {
		i.t.Fatalf("enrol open --json reports open=%v listen=%q, want an open window with an address", w.Open, w.Listen)
	}
	return w.Listen
}

// EnrolConn is one plain-TCP connection to the enrolment listener. A request
// is lodged and polled on the same connection.
type EnrolConn struct {
	t    *testing.T
	conn net.Conn
	rd   *bufio.Reader
}

// EnrolDial connects to addr, the `listen` of an open window. It fails t when
// the dial does not succeed.
func (i *Instance) EnrolDial(addr string) *EnrolConn {
	i.t.Helper()
	conn, err := net.DialTimeout("tcp", addr, enrolDeadline)
	if err != nil {
		i.t.Fatalf("dialing the enrolment listener %s: %v", addr, err)
	}
	c := &EnrolConn{t: i.t, conn: conn, rd: bufio.NewReaderSize(conn, 64*1024)}
	i.t.Cleanup(c.Close)
	return c
}

// EnrolRefused reports whether a dial to addr is refused by the kernel because
// nothing listens there. Any other failure fails t.
func (i *Instance) EnrolRefused(addr string) bool {
	i.t.Helper()
	conn, err := net.DialTimeout("tcp", addr, enrolDeadline)
	if err == nil {
		_ = conn.Close()
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	i.t.Fatalf("dialing %s failed with something other than a refusal: %v", addr, err)
	return false
}

// Send writes one frame line and reads one reply line. ok is false when relay
// closed the connection with no reply. It fails t when neither happens in 60 s.
func (c *EnrolConn) Send(frame any) (reply map[string]any, ok bool) {
	c.t.Helper()
	line, err := json.Marshal(frame)
	if err != nil {
		c.t.Fatalf("encoding the enrolment frame: %v", err)
	}
	if err := c.conn.SetDeadline(time.Now().Add(enrolDeadline)); err != nil {
		c.t.Fatalf("setting the enrolment deadline: %v", err)
	}
	if _, err := c.conn.Write(append(line, '\n')); err != nil {
		return nil, false
	}
	raw, err := c.rd.ReadBytes('\n')
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			c.t.Fatalf("no enrolment reply within %s", enrolDeadline)
		}
		if errors.Is(err, io.EOF) || len(raw) == 0 {
			return nil, false
		}
		c.t.Fatalf("reading the enrolment reply: %v", err)
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		c.t.Fatalf("the enrolment reply is not a JSON object: %v\n%s", err, tailString(raw, 500))
	}
	return reply, true
}

// Closed reports whether relay closes the connection within d: a read returns
// EOF or a reset. It returns false when the connection is still open at d, so
// d must be shorter than relay's own 30 s idle timeout for the answer to mean
// anything.
func (c *EnrolConn) Closed(d time.Duration) bool {
	c.t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		c.t.Fatalf("setting the enrolment deadline: %v", err)
	}
	_, err := c.rd.ReadByte()
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	return err != nil
}

// Close ends the connection. It is idempotent.
func (c *EnrolConn) Close() { _ = c.conn.Close() }

// SASClient is the client half of the comparison-code exchange: a random
// 16-byte value committed at lodge and opened on the first poll.
type SASClient struct{ nonce []byte }

// NewSASClient draws the client's random value.
func NewSASClient(t *testing.T) *SASClient {
	t.Helper()
	n := make([]byte, 16)
	if _, err := rand.Read(n); err != nil {
		t.Fatalf("drawing a comparison nonce: %v", err)
	}
	return &SASClient{nonce: n}
}

// Commit is the 64 lowercase hex commitment sent as `sas_commit` for id's CSR:
// sha256("relay.sas.commit.v1\x00" || sha256(CSR SPKI DER) || nonce).
func (s *SASClient) Commit(id *RemoteIdentity) string {
	spki, err := x509.MarshalPKIXPublicKey(&id.key.PublicKey)
	if err != nil {
		id.t.Fatalf("encoding the CSR public key: %v", err)
	}
	spkiSum := sha256.Sum256(spki)
	in := append([]byte("relay.sas.commit.v1\x00"), spkiSum[:]...)
	sum := sha256.Sum256(append(in, s.nonce...))
	return hex.EncodeToString(sum[:])
}

// Open is the 32 lowercase hex value sent as `sas_open` on the first poll.
func (s *SASClient) Open() string { return hex.EncodeToString(s.nonce) }

// Lodge builds the EnrolmentRequest frame for id with a commitment from s.
func Lodge(id *RemoteIdentity, label string, s *SASClient) map[string]any {
	return map[string]any{"type": "EnrolmentRequest", "csr_pem": string(id.CSRPEM()), "label": label, "sas_commit": s.Commit(id)}
}

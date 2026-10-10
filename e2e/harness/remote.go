package harness

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"testing"
	"time"
)

// RemoteIdentity is a remote client's key pair, made in the harness so no test
// handles a private key relay generated.
type RemoteIdentity struct {
	t   *testing.T
	key *ecdsa.PrivateKey
	csr []byte
}

// NewRemoteIdentity makes an ECDSA P-256 key and a CSR whose subject is cn.
func NewRemoteIdentity(t *testing.T, cn string) *RemoteIdentity {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a remote client key: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cn},
	}, key)
	if err != nil {
		t.Fatalf("creating a CSR for %q: %v", cn, err)
	}
	return &RemoteIdentity{t: t, key: key, csr: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})}
}

// CSRPEM is the signing request, for `relay enrol sign --csr -` or an
// EnrolmentRequest frame.
func (r *RemoteIdentity) CSRPEM() []byte { return r.csr }

// RemoteConn is one mutual-TLS connection to the remote listener.
type RemoteConn struct {
	t    *testing.T
	conn net.Conn
	rd   *bufio.Reader
}

const remoteDeadline = 60 * time.Second

// RemoteDial connects to ready.json listeners.remote with r's key and certPEM
// (the client.crt relay signed), trusting caPEM. The server certificate carries
// the names in remote.listen, so ServerName is 127.0.0.1.
func (i *Instance) RemoteDial(r *RemoteIdentity, certPEM, caPEM []byte) *RemoteConn {
	i.t.Helper()
	addr := i.Ready.Listeners["remote"]
	if addr == "" {
		i.t.Fatalf("ready.json has no listeners.remote; the remote listener is off")
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(r.key)
	if err != nil {
		i.t.Fatalf("encoding the remote client key: %v", err)
	}
	pair, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		i.t.Fatalf("pairing the client certificate with its key: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		i.t.Fatalf("the CA PEM holds no certificate")
	}
	d := &net.Dialer{Timeout: remoteDeadline}
	conn, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      pool,
		ServerName:   "127.0.0.1",
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		i.t.Fatalf("dialing the remote listener %s: %v", addr, err)
	}
	c := &RemoteConn{t: i.t, conn: conn, rd: bufio.NewReaderSize(conn, 64*1024)}
	i.t.Cleanup(c.Close)
	return c
}

// Send writes one frame line and reads one reply line. ok is false when relay
// closed the connection, with no reply. It fails t when neither happens in 60 s.
func (c *RemoteConn) Send(frame any) (reply map[string]any, ok bool) {
	c.t.Helper()
	line, err := json.Marshal(frame)
	if err != nil {
		c.t.Fatalf("encoding the remote frame: %v", err)
	}
	if err := c.conn.SetDeadline(time.Now().Add(remoteDeadline)); err != nil {
		c.t.Fatalf("setting the remote deadline: %v", err)
	}
	if _, err := c.conn.Write(append(line, '\n')); err != nil {
		return nil, false
	}
	raw, err := c.rd.ReadBytes('\n')
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			c.t.Fatalf("no remote reply within %s", remoteDeadline)
		}
		return nil, false
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		c.t.Fatalf("the remote reply is not a JSON object: %v\n%s", err, tailString(raw, 500))
	}
	return reply, true
}

// Close ends the connection. It is idempotent.
func (c *RemoteConn) Close() { _ = c.conn.Close() }

package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"
)

// ReadLaunchSecret is the service half of docs/launch-identity.md's launch
// fd contract. launched is false, with no error, when EnvLaunchFD is unset:
// the process was not launched by relay. When it is set, anything other than
// exactly 64 lowercase hex characters read to EOF is an error, and the caller
// must exit rather than continue without an identity.
//
// The descriptor is closed before returning on every path, so no child the
// caller spawns afterwards can inherit it.
func ReadLaunchSecret() (secret string, launched bool, err error) {
	raw, ok := os.LookupEnv(EnvLaunchFD)
	if !ok {
		return "", false, nil
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd != LaunchFD {
		return "", true, fmt.Errorf("%s=%q: want %d", EnvLaunchFD, raw, LaunchFD)
	}
	f := os.NewFile(uintptr(fd), "relay-launch")
	if f == nil {
		return "", true, fmt.Errorf("%s: descriptor %d is not open", EnvLaunchFD, fd)
	}
	// One byte past the secret's length, so an over-long pipe is detected
	// rather than truncated into a valid-looking secret.
	buf, readErr := io.ReadAll(io.LimitReader(f, 65))
	closeErr := f.Close()
	if readErr != nil {
		return "", true, fmt.Errorf("read launch secret: %w", readErr)
	}
	if closeErr != nil {
		return "", true, fmt.Errorf("close launch fd: %w", closeErr)
	}
	if !isLaunchSecret(string(buf)) {
		return "", true, errors.New("launch secret is not 64 lowercase hex characters")
	}
	return string(buf), true, nil
}

func isLaunchSecret(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// SendHello presents the launch secret on a fresh connection to sockPath and
// returns relay's recognition of the caller. The identity it binds belongs to
// this process, not to the connection, so the connection is closed after.
func SendHello(sockPath, name, secret string) (HelloResult, error) {
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return HelloResult{}, fmt.Errorf("dial bridge: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	payload, err := json.Marshal(BridgeRequest{Type: ReqHello, Name: name, Token: secret})
	if err != nil {
		return HelloResult{}, err
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return HelloResult{}, fmt.Errorf("write hello: %w", err)
	}
	sc := NewScanner(conn)
	if !sc.Scan() {
		return HelloResult{}, fmt.Errorf("read hello response: %v", sc.Err())
	}
	var resp BridgeResponse
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return HelloResult{}, fmt.Errorf("parse hello response: %w", err)
	}
	if err := checkError(&resp); err != nil {
		return HelloResult{}, err
	}
	var out HelloResult
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return HelloResult{}, fmt.Errorf("parse hello result: %w", err)
	}
	return out, nil
}

package harness

import (
	"encoding/base64"
	"regexp"
)

// SSHCall is one line of the ssh stub's call log: the destination and the
// remote command relay passed.
type SSHCall struct {
	Dest   string `json:"dest"`
	Remote string `json:"remote"`
}

// SSHCalls reads the ssh stub's call log. The stub appends a line before it
// acts, so a read after relay's reply needs no wait. It returns nil when the
// instance has no stub or the stub was never called.
func (i *Instance) SSHCalls() []SSHCall {
	i.t.Helper()
	return readJSONL[SSHCall](i.t, i.fakePath("fakessh.jsonl"))
}

var sshLauncherScript = regexp.MustCompile(`printf %s ([A-Za-z0-9+/=]+) \| base64 -d`)

// Script returns the script relay wrapped in the base64 launcher
// (docs/ssh-hosts.md), or Remote unchanged when it is not wrapped.
func (c SSHCall) Script() string {
	m := sshLauncherScript.FindStringSubmatch(c.Remote)
	if m == nil {
		return c.Remote
	}
	b, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		return c.Remote
	}
	return string(b)
}

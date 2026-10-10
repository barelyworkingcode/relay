package harness

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"
)

const enrolDeadline = 60 * time.Second

// EnrolSend opens one TCP connection to ready.json listeners.enrolment, writes
// each frame as a line and reads its reply line, in order. When relay closes
// the connection early (it does after 64 frames) the replies read so far are
// returned. It fails t when a reply does not come in 60 s.
func (i *Instance) EnrolSend(frames ...any) []map[string]any {
	i.t.Helper()
	addr := i.Ready.Listeners["enrolment"]
	if addr == "" {
		i.t.Fatalf("ready.json has no listeners.enrolment; the enrolment listener is off")
	}
	conn, err := net.DialTimeout("tcp", addr, enrolDeadline)
	if err != nil {
		i.t.Fatalf("dialing the enrolment listener %s: %v", addr, err)
	}
	defer func() { _ = conn.Close() }()
	rd := bufio.NewReaderSize(conn, 64*1024)
	var replies []map[string]any
	for n, f := range frames {
		line, err := json.Marshal(f)
		if err != nil {
			i.t.Fatalf("encoding enrolment frame %d: %v", n, err)
		}
		if err := conn.SetDeadline(time.Now().Add(enrolDeadline)); err != nil {
			i.t.Fatalf("setting the enrolment deadline: %v", err)
		}
		if _, err := conn.Write(append(line, '\n')); err != nil {
			return replies
		}
		raw, err := rd.ReadBytes('\n')
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				i.t.Fatalf("no reply to enrolment frame %d within %s", n, enrolDeadline)
			}
			if errors.Is(err, io.EOF) || len(raw) == 0 {
				return replies
			}
			i.t.Fatalf("reading the reply to enrolment frame %d: %v", n, err)
		}
		var reply map[string]any
		if err := json.Unmarshal(raw, &reply); err != nil {
			i.t.Fatalf("the reply to enrolment frame %d is not a JSON object: %v\n%s", n, err, tailString(raw, 500))
		}
		replies = append(replies, reply)
	}
	return replies
}

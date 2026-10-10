package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

var logFiles = []string{"relay.log.1", "relay.log", "relaysessions.log.1", "relaysessions.log"}

var eventKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

type logLine struct {
	ts  time.Time
	ok  bool
	raw string
	m   map[string]any
}

type tailer struct {
	dir     string
	offsets map[string]int64
}

// read returns the whole lines appended since the last read, sorted by ts.
func (t *tailer) read() []logLine {
	var out []logLine
	for _, name := range logFiles {
		f, err := os.Open(filepath.Join(t.dir, "logs", name))
		if err != nil {
			continue
		}
		f.Seek(t.offsets[name], io.SeekStart)
		b, _ := io.ReadAll(f)
		f.Close()
		end := bytes.LastIndexByte(b, '\n') + 1
		t.offsets[name] += int64(end)
		for _, ln := range strings.Split(strings.TrimSuffix(string(b[:end]), "\n"), "\n") {
			if ln == "" {
				continue
			}
			l := logLine{raw: ln}
			if json.Unmarshal([]byte(ln), &l.m) != nil {
				l.m = nil
			} else if ts, _ := l.m["ts"].(string); ts != "" {
				if tm, err := time.Parse(time.RFC3339Nano, ts); err == nil {
					l.ts, l.ok = tm, true
				}
			}
			out = append(out, l)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ts.Before(out[j].ts) })
	return out
}

func parseSince(s string) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return time.Now().Add(-d), nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999Z07:00", "2006-01-02T15:04:05.999999999"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("--since needs an RFC 3339 time or a positive duration")
}

func (e *env) logs(args []string) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	event := fs.String("event", "", "")
	since := fs.String("since", "", "")
	follow := fs.Bool("follow", false, "")
	timeout := fs.Duration("timeout", 0, "")
	if code, ok := e.parse(fs, args); !ok {
		return code
	}
	var sinceT time.Time
	if *since != "" {
		var err error
		if sinceT, err = parseSince(*since); err != nil {
			return e.fail(2, "%v", err)
		}
	}
	if *event != "" && !eventKeyRe.MatchString(*event) {
		return e.fail(2, "--event needs an event key such as service.restart, got %q", *event)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["timeout"] && (!*follow || *timeout <= 0) {
		return e.fail(2, "--timeout needs a positive duration and --follow")
	}
	filtered := *event != "" || e.g.Trace != "" || *since != ""
	match := func(l logLine) bool {
		if !filtered {
			return true
		}
		if l.m == nil || (e.g.Trace != "" && l.m["trace_id"] != e.g.Trace) || (*event != "" && l.m["event"] != *event) {
			return false
		}
		return *since == "" || (l.ok && !l.ts.Before(sinceT))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Subscribe before the first read, so a line written between the two is
	// either read now or announced.
	var wake <-chan struct{}
	if *follow {
		wake = e.watch(ctx)
	}
	t := &tailer{dir: e.g.ConfigDir, offsets: map[string]int64{}}
	exists := false
	for _, n := range logFiles[:2] {
		if _, err := os.Stat(filepath.Join(e.g.ConfigDir, "logs", n)); err == nil {
			exists = true
		}
	}
	if !exists {
		return e.fail(2, "no relay log in %s; is %s a relay config dir?", filepath.Join(e.g.ConfigDir, "logs"), e.g.ConfigDir)
	}
	printed := 0
	emit := func(lines []logLine) bool {
		for _, l := range lines {
			if match(l) {
				e.printLine(l, *asJSON)
				printed++
				if *follow && *event != "" {
					return true
				}
			}
		}
		return false
	}
	if emit(t.read()) || !*follow {
		return exitFor(printed)
	}
	var deadline <-chan time.Time
	if *timeout > 0 {
		tm := time.NewTimer(*timeout)
		defer tm.Stop()
		deadline = tm.C
	}
	for {
		select {
		case <-wake:
			if emit(t.read()) {
				return 0
			}
		case <-deadline:
			return exitFor(printed)
		case <-ctx.Done():
			return exitFor(printed)
		}
	}
}

func exitFor(printed int) int {
	if printed > 0 {
		return 0
	}
	return 1
}

func (e *env) printLine(l logLine, asJSON bool) {
	if asJSON || l.m == nil {
		fmt.Fprintln(e.out, l.raw)
		return
	}
	core := map[string]bool{"ts": true, "level": true, "msg": true, "service": true, "op": true, "status": true, "duration_ms": true, "error": true, "trace_id": true, "event": true, "reason": true}
	var extra []string
	for k, v := range l.m {
		if !core[k] {
			extra = append(extra, fmt.Sprintf("%s=%v", k, v))
		}
	}
	sort.Strings(extra)
	fmt.Fprintf(e.out, "%v %v %v %v", l.m["ts"], l.m["level"], l.m["msg"], l.m["status"])
	if r, ok := l.m["reason"]; ok {
		fmt.Fprintf(e.out, " reason=%v", r)
	}
	if t, _ := l.m["trace_id"].(string); t != "" {
		fmt.Fprintf(e.out, " trace=%s", t)
	}
	if len(extra) > 0 {
		fmt.Fprint(e.out, " "+strings.Join(extra, " "))
	}
	fmt.Fprintln(e.out)
}

// watch connects to the server's append notices and turns each into a wake.
// With no server the channel never fires and the caller waits out its timeout.
func (e *env) watch(ctx context.Context) <-chan struct{} {
	wake := make(chan struct{}, 1)
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://fakerelay/v1/follow", nil)
	resp, err := e.client().Do(req)
	if err != nil {
		return wake
	}
	go func() {
		defer resp.Body.Close()
		dec := json.NewDecoder(resp.Body)
		for {
			var n map[string]any
			if dec.Decode(&n) != nil {
				return
			}
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}()
	return wake
}

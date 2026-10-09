package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/logging"
)

const logsUsage = "usage: relay [--config-dir DIR] [--trace ID] logs [--json] [--event KEY] [--since TIME] [--follow [--timeout DUR]]"

// Exit codes of `relay logs`.
const (
	logsExitMatched = 0
	logsExitNone    = 1
	logsExitError   = 2
)

type logsOptions struct {
	filter  logging.Filter
	json    bool
	follow  bool
	timeout time.Duration
}

func runLogsCommand(args []string, traceID string) {
	os.Exit(logsMain(args, traceID, bridge.ConfigDir(), os.Stdout, os.Stderr))
}

func logsMain(args []string, traceID, configDir string, stdout, stderr io.Writer) int {
	opts, err := parseLogsArgs(args, traceID, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n%s\n", err, logsUsage)
		return logsExitError
	}

	// Installed before any read so a signal that arrives early is held, not
	// fatal.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)

	logDir := filepath.Join(configDir, "logs")
	tailer, err := logging.OpenTailer(logDir)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return logsExitError
	}
	defer tailer.Close()
	if !tailer.HasRelayLog() {
		fmt.Fprintf(stderr, "error: no relay log in %s; is %s a relay config dir?\n", logDir, configDir)
		return logsExitError
	}

	out := bufio.NewWriter(stdout)
	printed := 0
	emit := func(l logging.Line) {
		if opts.json {
			_, _ = out.Write(l.Raw)
		} else {
			_, _ = out.WriteString(logging.FormatLine(l))
		}
		_ = out.WriteByte('\n')
		printed++
	}
	exit := func() int {
		if err := out.Flush(); err != nil {
			fmt.Fprintf(stderr, "error: write: %v\n", err)
			return logsExitError
		}
		if printed > 0 {
			return logsExitMatched
		}
		return logsExitNone
	}
	// --follow --event stops at the first match; any other follow runs to its
	// deadline or a signal.
	untilMatch := opts.follow && opts.filter.Event != ""

	history, err := tailer.Drain()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return logsExitError
	}
	logging.SortLines(history)
	for _, l := range history {
		if !opts.filter.Match(l) {
			continue
		}
		emit(l)
		if untilMatch {
			return exit()
		}
	}
	if !opts.follow {
		return exit()
	}
	if err := out.Flush(); err != nil {
		fmt.Fprintf(stderr, "error: write: %v\n", err)
		return logsExitError
	}

	select {
	case <-sigs:
		return exit()
	default:
	}

	if err := tailer.Watch(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return logsExitError
	}
	go func() {
		<-sigs
		tailer.Wake()
	}()
	var deadline time.Time
	if opts.timeout > 0 {
		deadline = time.Now().Add(opts.timeout)
	}

	// Deliberate: Watch is armed before this Drain, so a line written between
	// the history read and now still raises an event and is read on the next
	// pass; nothing is missed and nothing repeats.
	for {
		lines, err := tailer.Drain()
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return logsExitError
		}
		for _, l := range lines {
			if !opts.filter.Match(l) {
				continue
			}
			emit(l)
			if untilMatch {
				return exit()
			}
		}
		if err := out.Flush(); err != nil {
			fmt.Fprintf(stderr, "error: write: %v\n", err)
			return logsExitError
		}
		if tailer.Wait(deadline) != logging.WaitChanged {
			return exit()
		}
	}
}

func parseLogsArgs(args []string, traceID string, now time.Time) (logsOptions, error) {
	var o logsOptions
	var event, since string
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&o.json, "json", false, "")
	fs.StringVar(&event, "event", "", "")
	fs.StringVar(&since, "since", "", "")
	fs.BoolVar(&o.follow, "follow", false, "")
	fs.DurationVar(&o.timeout, "timeout", 0, "")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	timeoutSet := false
	fs.Visit(func(f *flag.Flag) { timeoutSet = timeoutSet || f.Name == "timeout" })
	if timeoutSet {
		if !o.follow {
			return o, fmt.Errorf("--timeout is only valid with --follow")
		}
		if o.timeout <= 0 {
			return o, fmt.Errorf("--timeout must be a positive duration")
		}
	}
	if event != "" && !logging.EventKeyPattern.MatchString(event) {
		return o, fmt.Errorf("--event %q is not an event key such as service.restart", event)
	}
	o.filter.TraceID, o.filter.Event = traceID, event
	if since != "" {
		t, err := logging.ParseSince(since, now)
		if err != nil {
			return o, err
		}
		o.filter.Since = t
	}
	return o, nil
}

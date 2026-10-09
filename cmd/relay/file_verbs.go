package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/logging"
)

// filesWatchRequest and filesWatchResult are the files.watch op's wire types.
// Its frames are the /ws/files frames, one per Progress frame.
type filesWatchRequest struct {
	ProjectID string `json:"project_id"`
}

type filesWatchResult struct {
	ProjectID string `json:"project_id"`
}

var errFilesUnavailable = errors.New("file operations are not available in this relay process")

// adminFilesWatch subscribes the CLI peer to the same watch hub /ws/files uses
// and relays its frames until the peer leaves. A peer that leaves cancels ctx
// (frameconn's peer watch), which releases the watch.
func adminFilesWatch(ctx context.Context, r *appRouter, args json.RawMessage) (_ json.RawMessage, err error) {
	if r.fileOps == nil {
		return nil, errFilesUnavailable
	}
	progress := bridge.ProgressFromContext(ctx)
	if progress == nil {
		return nil, errors.New("files.watch streams frames and needs a streaming caller")
	}
	req, err := decodeAdminArgs[filesWatchRequest]("files.watch", args)
	if err != nil {
		return nil, err
	}
	if req.ProjectID == "" {
		return nil, errors.New("files.watch: project_id is required")
	}
	ev := logging.BeginEvent(ctx, "files.watch").Set("project_id", req.ProjectID)
	defer func() {
		// Deliberate: the peer leaving is how a watch ends, so a cancelled
		// context is a normal end, not an error.
		if errors.Is(ctx.Err(), context.Canceled) && err == nil {
			ev.End(logging.OutcomeOK, "", nil)
			return
		}
		endEvent(ev, err)
	}()

	c := newFilesConn(r.fileOps, nil, callerAuditActor(operatorLaunchCaller(ctx)))
	defer func() {
		c.close()
		c.releaseAll()
	}()
	defer r.fileOps.subscribeHostStatus(c)()
	c.enqueue(req.ProjectID, func() { c.watch(req.ProjectID) })

	for {
		select {
		case raw := <-c.out:
			progress(bridge.ProgressUpdate{Data: raw})
		case <-ctx.Done():
			return marshalAdminResult(filesWatchResult(req))
		case <-c.done:
			return nil, errors.New("files.watch: the frame queue overflowed; run it again")
		}
	}
}

// filesWatch prints one line per /ws/files frame. --until ends it on the first
// frame of that type, --timeout after that long, and Ctrl-C at any time.
func filesWatch(args []string) {
	fs := flag.NewFlagSet("files watch", flag.ExitOnError)
	project := fs.String("project", "", "project id (required)")
	until := fs.String("until", "", "stop after the first frame of this type (host_status, watch_ok, watch_error, fs_event)")
	timeout := fs.Duration("timeout", 0, "stop after this long (0 waits until interrupted)")
	asJSON := fs.Bool("json", false, "print each frame as one line of JSON")
	fs.Parse(args)
	if *project == "" {
		exitError("--project is required")
	}

	client := requireService("relay files watch")
	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	if *timeout > 0 {
		var cancelTimeout context.CancelFunc
		ctx, cancelTimeout = context.WithTimeout(ctx, *timeout)
		defer cancelTimeout()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	matched := false
	var watchFailure string
	body := marshalCLIArgs(filesWatchRequest{ProjectID: *project})
	_, err := client.AdminOpStream(ctx, "files.watch", body, func(data json.RawMessage) {
		if matched || watchFailure != "" {
			return
		}
		var f fileFrame
		if json.Unmarshal(data, &f) != nil {
			return
		}
		printWatchFrame(data, f, *asJSON)
		switch {
		case *until != "" && f.Type == *until:
			matched = true
			cancel()
		case f.Type == "watch_error" && f.ProjectID == *project:
			watchFailure = f.Error
			cancel()
		}
	})
	switch {
	case watchFailure != "":
		exitError("watch %s: %s", *project, watchFailure)
	case matched:
		return
	case err == nil:
		return
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) && *until != "":
		exitError("timed out after %s waiting for %s", timeout.Round(time.Millisecond), *until)
	case ctx.Err() != nil:
		return
	}
	fmt.Fprintf(os.Stderr, "error: %s\n", adminOpErrorText(err))
	os.Exit(1)
}

func printWatchFrame(data json.RawMessage, f fileFrame, asJSON bool) {
	if asJSON {
		printJSONLine(data)
		return
	}
	switch f.Type {
	case "host_status":
		fmt.Printf("host %s %s\n", f.Name, f.Status)
	case "watch_ok":
		fmt.Printf("watching %s\n", f.ProjectID)
	case "watch_error":
		fmt.Printf("watch error %s: %s\n", f.Code, f.Error)
	case "fs_event":
		fmt.Printf("%s %s\n", f.Kind, f.Path)
	default:
		printJSONLine(data)
	}
}

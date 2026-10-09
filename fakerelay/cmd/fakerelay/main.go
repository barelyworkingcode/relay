// Command fakerelay is a fake relay: the same sockets, routes, verbs and log
// files as relay, for testing a client on any OS.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/barelyworkingcode/relay/fakerelay/internal/cli"
	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/surface"
)

func main() { os.Exit(run()) }

func run() int {
	g, rest, err := cli.ParseGlobal(os.Args[1:], os.Getenv)
	if err != nil {
		var ee *cli.ExitError
		code := 1
		if errors.As(err, &ee) {
			code = ee.Code
		}
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		return code
	}
	if len(rest) > 0 && rest[0] == "serve" {
		return serve(g, rest[1:])
	}
	return cli.Run(g, rest, os.Stdout, os.Stderr)
}

func serve(g cli.Global, args []string) int {
	if len(args) > 0 || g.ConfigDir == "" {
		fmt.Fprintln(os.Stderr, "usage: fakerelay --config-dir DIR serve")
		return 2
	}
	srv, err := server.New(g.ConfigDir, os.Stdout)
	if err == nil {
		cli.RegisterServer(srv.Registrar(), srv.Deps(), srv.Launcher())
		err = surface.Register(srv.Registrar(), srv.Deps())
		if err != nil {
			srv.Close()
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		return 1
	}
	return 0
}

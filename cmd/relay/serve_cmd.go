package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

const serveUsage = "Usage: relay [--config-dir DIR] serve"

// runServeCommand runs relay's full server for the selected config dir with no
// tray and no window. It prints one line to stdout, the ready file's path,
// once every listener is up, and never returns: a clean SIGTERM or SIGINT
// exits 0 from the server core's signal handler.
func runServeCommand(args []string) {
	if len(args) != 0 {
		fmt.Fprintf(os.Stderr, "relay serve: unexpected argument %q\n%s\n", args[0], serveUsage)
		os.Exit(2)
	}
	platform := newHeadlessPlatform()
	platform.Init()
	if _, err := startServerCore(serverOptions{Platform: platform}); err != nil {
		slog.Error("relay serve cannot start", "config_dir", bridge.ConfigDir(), "error", err)
		fmt.Fprintf(os.Stderr, "error: relay serve cannot start in %s: %v\n", bridge.ConfigDir(), err)
		os.Exit(1)
	}
	fmt.Println(readyFilePath(bridge.ConfigDir()))
	platform.Run()
}

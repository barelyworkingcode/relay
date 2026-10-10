package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// liveTools is what relay's probe finds on the machine behind a host. A fake
// host stands on the machine fakerelay runs on, so the lookups run here, on
// the world's host_path when it names one.
type liveTools struct {
	NodePath, NodeVersion, ClaudePath, ClaudeVersion, TmuxPath string
}

const versionTimeout = 10 * time.Second

// lookupTool is `command -v tool` over a PATH list.
func lookupTool(pathList, tool string) string {
	if pathList == "" {
		pathList = os.Getenv("PATH")
	}
	for _, dir := range filepath.SplitList(pathList) {
		p := filepath.Join(dir, tool)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

func toolVersion(path string) string {
	if path == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
}

func probeLiveTools(pathList string) liveTools {
	t := liveTools{
		NodePath:   lookupTool(pathList, "node"),
		ClaudePath: lookupTool(pathList, "claude"),
		TmuxPath:   lookupTool(pathList, "tmux"),
	}
	var wg sync.WaitGroup
	for path, dst := range map[string]*string{t.NodePath: &t.NodeVersion, t.ClaudePath: &t.ClaudeVersion} {
		wg.Add(1)
		go func() { defer wg.Done(); *dst = toolVersion(path) }()
	}
	wg.Wait()
	return t
}

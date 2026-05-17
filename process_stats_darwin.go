//go:build darwin

package main

import (
	"context"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// SampleRSSByRoot returns, for each root PID, the total resident memory (bytes)
// of that process and all of its descendants. Services in relay are launched
// via `/bin/sh -l -c '...'`, so the canonical PID is a shell wrapper and the
// real workload lives one level down — summing the subtree is what matters.
//
// Implementation: one `ps -axo pid=,ppid=,rss=` call per invocation (~5 ms on
// macOS), parsed in-process, then BFS from each root summing RSS. Returns an
// empty map on failure rather than partial data.
func SampleRSSByRoot(rootPIDs []int) map[int]uint64 {
	if len(rootPIDs) == 0 {
		return map[int]uint64{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,rss=").Output()
	if err != nil {
		slog.Warn("rss sample: ps failed", "error", err)
		return map[int]uint64{}
	}

	// rss is in KB on macOS; multiply by 1024 at the end.
	rssKB := make(map[int]uint64, 256)
	children := make(map[int][]int, 256)

	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		rss, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			continue
		}
		rssKB[pid] = rss
		children[ppid] = append(children[ppid], pid)
	}

	result := make(map[int]uint64, len(rootPIDs))
	for _, root := range rootPIDs {
		if _, ok := rssKB[root]; !ok {
			// Root is dead or not visible; report zero so callers can distinguish.
			result[root] = 0
			continue
		}
		var sumKB uint64
		visited := make(map[int]struct{}, 16)
		queue := []int{root}
		for len(queue) > 0 {
			pid := queue[0]
			queue = queue[1:]
			if _, seen := visited[pid]; seen {
				continue
			}
			visited[pid] = struct{}{}
			sumKB += rssKB[pid]
			queue = append(queue, children[pid]...)
		}
		result[root] = sumKB * 1024
	}
	return result
}

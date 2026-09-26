package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type evidence struct {
	PR                                     int
	Commit, ToolCommit, WorldSummary, Home string
	Results                                []result
}

func gh(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

func prHead(ctx context.Context, pr int) (string, error) {
	head, err := gh(ctx, "", "pr", "view", strconv.Itoa(pr), "--json", "headRefOid", "-q", ".headRefOid")
	if err == nil && head == "" {
		err = errors.New("PR has no head commit")
	}
	return head, err
}

func statusState(rs []result) string {
	counts, _ := tally(rs)
	switch {
	case counts[stateFail] > 0:
		return "failure"
	case counts[stateBlocked] > 0:
		return "error"
	}
	return "success"
}

func renderComment(ev evidence) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### devbox/verify: %s\n\n", statusState(ev.Results))
	fmt.Fprintf(&b, "| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Relay commit | `%s` |\n", ev.Commit)
	fmt.Fprintf(&b, "| World verify | %s |\n", ev.WorldSummary)
	fmt.Fprintf(&b, "| Tool commit | `%s` |\n\n", ev.ToolCommit)
	fmt.Fprintf(&b, "| Journey | Result | Detail |\n|---|---|---|\n")
	for _, r := range ev.Results {
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", r.ID, r.State, strings.ReplaceAll(r.Detail, "|", `\|`))
	}
	return scrub(b.String(), ev.Home)
}

// post comments first so the commit status can link to the comment.
func post(ctx context.Context, ev evidence) (commentURL string, err error) {
	out, err := gh(ctx, renderComment(ev), "pr", "comment", strconv.Itoa(ev.PR), "--body-file", "-")
	if err != nil {
		return "", err
	}
	lines := strings.Split(out, "\n")
	commentURL = strings.TrimSpace(lines[len(lines)-1])
	if !strings.HasPrefix(commentURL, "https://") {
		return "", errors.New("gh pr comment printed no comment URL")
	}
	state := statusState(ev.Results)
	counts, _ := tally(ev.Results)
	desc := fmt.Sprintf("pass=%d fail=%d blocked=%d notrun=%d", counts[statePass], counts[stateFail], counts[stateBlocked], counts[stateNotRun])
	_, err = gh(ctx, "", "api", "-X", "POST", "repos/{owner}/{repo}/statuses/"+ev.Commit,
		"-f", "state="+state, "-f", "context=devbox/verify", "-f", "target_url="+commentURL, "-f", "description="+desc)
	if err != nil {
		return "", err
	}
	return commentURL, nil
}

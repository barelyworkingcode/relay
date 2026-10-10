package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type evidence struct {
	PR                                     int
	Commit, ToolCommit, WorldSummary, Home string
	RunTime                                time.Duration
	Results                                []result
	Repaired                               []repairLine
}

func gh(ctx context.Context, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh %s %s failed: %w", args[0], args[1], err)
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
	fmt.Fprintf(&b, "| Repaired | %s |\n", repairedCell(ev.Repaired))
	fmt.Fprintf(&b, "| Tool commit | `%s` |\n", ev.ToolCommit)
	fmt.Fprintf(&b, "| Run time | %d s |\n\n", (ev.RunTime.Milliseconds()+500)/1000)
	fmt.Fprintf(&b, "| Journey | Result | Detail |\n|---|---|---|\n")
	for _, r := range ev.Results {
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", r.ID, r.State, strings.ReplaceAll(r.Detail, "|", `\|`))
	}
	return scrub(b.String(), ev.Home)
}

// repairedCell states "none" rather than leaving the row out, so the absence
// of a repair is stated, not implied.
func repairedCell(rs []repairLine) string {
	if len(rs) == 0 {
		return "none"
	}
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = strings.Join(strings.Fields(r.What+": "+r.Detail), " ")
	}
	return strings.ReplaceAll(strings.Join(parts, "; "), "|", `\|`)
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

const fullContext = "devbox/verify-full"

// mainPreflight requires the checkout to be clean and at the default-branch
// head on GitHub, so a full run vouches for main and nothing else. It returns
// the repository's owner/name.
func mainPreflight(ctx context.Context, checkout, head string) (string, error) {
	dirty, err := gitOut(checkout, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if dirty != "" {
		return "", errors.New("checkout is not clean")
	}
	info, err := gh(ctx, "", "repo", "view", "--json", "nameWithOwner,defaultBranchRef", "-q", `.nameWithOwner + " " + .defaultBranchRef.name`)
	if err != nil {
		return "", err
	}
	slug, branch, ok := strings.Cut(info, " ")
	if !ok || slug == "" || branch == "" {
		return "", errors.New("cannot read the repository's default branch")
	}
	remote, err := gh(ctx, "", "api", "repos/"+slug+"/commits/"+branch, "-q", ".sha")
	if err != nil {
		return "", err
	}
	if remote != head {
		return "", fmt.Errorf("checkout HEAD %s is not %s head %s", head, branch, remote)
	}
	return slug, nil
}

// fullState is success only when every journey in the table ran and passed.
// Unlike statusState, a NOTRUN journey or a short run is not green: an epic's
// full run has no journey to excuse.
func fullState(rs []result, total int) string {
	counts, _ := tally(rs)
	if total == 0 || len(rs) != total || counts[statePass] != total {
		return "failure"
	}
	return "success"
}

// postFull posts the status first and comments only once it landed, and only
// on success: the comment line is what merge-check looks for, so it must not
// exist for a run that did not pass.
func postFull(ctx context.Context, epic int, slug, head string, rs []result, total int) (string, error) {
	state := fullState(rs, total)
	counts, _ := tally(rs)
	desc := fmt.Sprintf("full %d/%d pass=%d fail=%d blocked=%d notrun=%d", len(rs), total,
		counts[statePass], counts[stateFail], counts[stateBlocked], counts[stateNotRun])
	_, err := gh(ctx, "", "api", "-X", "POST", "repos/"+slug+"/statuses/"+head,
		"-f", "state="+state, "-f", "context="+fullContext, "-f", "description="+desc)
	if err != nil {
		return "", err
	}
	if state == "success" {
		if _, err := gh(ctx, "Full verify: "+slug+"@"+head, "issue", "comment", strconv.Itoa(epic), "-R", slug, "--body-file", "-"); err != nil {
			return "", err
		}
	}
	return state, nil
}

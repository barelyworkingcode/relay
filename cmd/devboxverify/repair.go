package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// repairTimeout covers repair.sh's world-lock wait, a verify, a reset and a
// second verify. It is a cap, not a prediction.
const repairTimeout = 900 * time.Second

type repairStep struct {
	OK     bool
	Detail string
}

type repairLine struct{ What, Detail string }

type repairOutcome struct {
	Bootstrap, World repairStep
	Repaired         []repairLine
	Pass, Fail       int
}

func (s repairStep) result() (string, error) {
	if !s.OK {
		return "", errors.New(s.Detail)
	}
	return s.Detail, nil
}

func blockedStep(detail string) repairStep {
	return repairStep{Detail: "BLOCKED environment: " + detail}
}

// parseRepair maps repair.sh's stdout onto the bootstrap and world checks. It
// does not classify: every detail is repair.sh's own. A FAIL line wins over an
// OK line and over the exit code, and world is OK only when repair.sh said so,
// exited 0 in time and printed a green SUMMARY.
func parseRepair(stdout string, exit int, timedOut bool) repairOutcome {
	how := fmt.Sprintf("exited %d", exit)
	if timedOut {
		how = fmt.Sprintf("timed out after %ds", int(repairTimeout.Seconds()))
	}
	var o repairOutcome
	type seen struct {
		ok, fail       bool
		okDet, failDet string
	}
	checks := map[string]*seen{"bootstrap": {}, "world": {}}
	for _, line := range strings.Split(stdout, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		switch {
		case len(f) == 4 && f[0] == "CHECK" && checks[f[1]] != nil:
			c := checks[f[1]]
			switch {
			case f[2] == "FAIL" && !c.fail:
				c.fail, c.failDet = true, f[3]
			case f[2] == "OK":
				c.ok, c.okDet = true, f[3]
			}
		case len(f) == 3 && f[0] == "REPAIRED":
			o.Repaired = append(o.Repaired, repairLine{What: f[1], Detail: f[2]})
		}
	}

	b := checks["bootstrap"]
	switch {
	case b.fail:
		o.Bootstrap = blockedStep(b.failDet)
	case b.ok:
		o.Bootstrap = repairStep{OK: true, Detail: b.okDet}
	default:
		o.Bootstrap = blockedStep("bootstrap incomplete; repair.sh " + how + " without a result; run bootstrap.sh")
	}

	pass, fail, sumErr := parseWorldSummary(stdout)
	if sumErr == nil {
		o.Pass, o.Fail = pass, fail
	}
	w := checks["world"]
	switch {
	case w.fail:
		o.World = blockedStep(w.failDet)
	case w.ok && exit == 0 && !timedOut && sumErr == nil && fail == 0:
		o.World = repairStep{OK: true, Detail: w.okDet}
	default:
		o.World = blockedStep("repair.sh " + how + " without a result")
	}
	return o
}

// runRepair runs repair.sh once. Its stdout is parsed and also copied to our
// stderr, like every other world script's; its stderr is inherited.
func runRepair(worldCheckout string) repairOutcome {
	ctx, cancel := context.WithTimeout(context.Background(), repairTimeout)
	defer cancel()
	var out strings.Builder
	cmd := exec.CommandContext(ctx, filepath.Join(worldCheckout, "repair.sh"))
	// This is subtle: see script. A child left behind would hold the pipe open.
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdout, cmd.Stderr = io.MultiWriter(&out, os.Stderr), os.Stderr
	exit := 0
	if err := cmd.Run(); err != nil {
		exit = -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else {
			fmt.Fprintln(os.Stderr, "repair.sh:", scrub(err.Error(), home))
		}
	}
	return parseRepair(out.String(), exit, ctx.Err() == context.DeadlineExceeded)
}

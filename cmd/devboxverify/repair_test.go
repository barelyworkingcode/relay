package main

import (
	"reflect"
	"testing"
)

func TestParseRepair(t *testing.T) {
	const (
		bootOK     = "CHECK\tbootstrap\tOK\tcomplete\n"
		worldGreen = "CHECK\tworld\tOK\tgreen\n"
		summary12  = "SUMMARY\tpass=12\tfail=0\n"
		before     = "1/12 failed, first file acme todo.txt: missing"
		incomplete = "bootstrap incomplete; needs a person: helper-build (helper app is not built), helper-authorize; run bootstrap.sh"
		blocked    = "BLOCKED environment: "
		timedOut   = "timed out after 900s"
	)
	ok := func(d string) repairStep { return repairStep{OK: true, Detail: d} }
	fail := func(d string) repairStep { return repairStep{Detail: blocked + d} }
	noResult := func(how string) repairStep {
		return repairStep{Detail: blocked + "repair.sh " + how + " without a result"}
	}
	unbooted := func(how string) repairStep {
		return repairStep{Detail: blocked + "bootstrap incomplete; repair.sh " + how + " without a result; run bootstrap.sh"}
	}

	cases := []struct {
		name     string
		stdout   string
		exit     int
		timedOut bool
		want     repairOutcome
	}{
		{"green", bootOK + worldGreen + summary12, 0, false,
			repairOutcome{Bootstrap: ok("complete"), World: ok("green"), Pass: 12}},
		{"bootstrap needs a person", "CHECK\tbootstrap\tFAIL\t" + incomplete + "\n", 6, false,
			repairOutcome{Bootstrap: fail(incomplete), World: noResult("exited 6")}},
		{"world data invalid", "CHECK\tbootstrap\tFAIL\tworld data: acme has no files\n", 2, false,
			repairOutcome{Bootstrap: fail("world data: acme has no files"), World: noResult("exited 2")}},
		{"lock timeout", bootOK + "CHECK\tworld\tFAIL\ttimed out waiting for the world lock; another run holds it\n", 2, false,
			repairOutcome{Bootstrap: ok("complete"), World: fail("timed out waiting for the world lock; another run holds it")}},
		{"verify error needs a person", bootOK + "CHECK\tworld\tFAIL\tneeds a person: no world manifest\n", 6, false,
			repairOutcome{Bootstrap: ok("complete"), World: fail("needs a person: no world manifest")}},
		{"repaired", bootOK + "REPAIRED\tworld\treset; before: " + before + "\nCHECK\tworld\tOK\tgreen after repair\n" + summary12, 0, false,
			repairOutcome{Bootstrap: ok("complete"), World: ok("green after repair"), Pass: 12,
				Repaired: []repairLine{{What: "world", Detail: "reset; before: " + before}}}},
		{"repair failed", bootOK + "CHECK\tworld\tFAIL\tverify.sh is not green; repair failed; before: " + before + "; reset ok; after: " + before + "\n", 1, false,
			repairOutcome{Bootstrap: ok("complete"), World: fail("verify.sh is not green; repair failed; before: " + before + "; reset ok; after: " + before)}},
		{"no output at all", "", 2, false,
			repairOutcome{Bootstrap: unbooted("exited 2"), World: noResult("exited 2")}},
		{"world OK but nonzero exit", bootOK + worldGreen + summary12, 1, false,
			repairOutcome{Bootstrap: ok("complete"), World: noResult("exited 1"), Pass: 12}},
		{"world OK without a SUMMARY", bootOK + worldGreen, 0, false,
			repairOutcome{Bootstrap: ok("complete"), World: noResult("exited 0")}},
		{"world OK with a red SUMMARY", bootOK + worldGreen + "SUMMARY\tpass=11\tfail=1\n", 0, false,
			repairOutcome{Bootstrap: ok("complete"), World: noResult("exited 0"), Pass: 11, Fail: 1}},
		{"timeout with no output", "", -1, true,
			repairOutcome{Bootstrap: unbooted(timedOut), World: noResult(timedOut)}},
		{"timeout after a green world", bootOK + worldGreen + summary12, 0, true,
			repairOutcome{Bootstrap: ok("complete"), World: noResult(timedOut), Pass: 12}},
		{"FAIL lines beat exit 0", "CHECK\tbootstrap\tFAIL\t" + incomplete + "\nCHECK\tworld\tFAIL\tneeds a person: partial world\n", 0, false,
			repairOutcome{Bootstrap: fail(incomplete), World: fail("needs a person: partial world")}},
		{"unknown lines ignored", "repair: checking\n" + bootOK + "PASS\tfile\tacme\ttodo.txt\nCHECK\tother\tFAIL\tnot ours\n" + worldGreen + "note\n" + summary12, 0, false,
			repairOutcome{Bootstrap: ok("complete"), World: ok("green"), Pass: 12}},
		{"counts from the last SUMMARY", bootOK + worldGreen + "SUMMARY\tpass=3\tfail=2\n" + summary12, 0, false,
			repairOutcome{Bootstrap: ok("complete"), World: ok("green"), Pass: 12}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseRepair(c.stdout, c.exit, c.timedOut)
			if len(got.Repaired) == 0 {
				got.Repaired = nil
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("parseRepair(%q, %d, %v)\n got %+v\nwant %+v", c.stdout, c.exit, c.timedOut, got, c.want)
			}
		})
	}
}

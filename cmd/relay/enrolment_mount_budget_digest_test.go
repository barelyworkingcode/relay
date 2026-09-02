package main

// The mount-budget half of relayfs P1b's presence-digest wiring: an
// operator confirming an enrolment create/sign/update must see the mount
// budget bound into the same digest the tool-plane budget already is (§6.4)
// — if a mount-budget field could change without moving the digest, a
// presence grant answered for one mount budget would be silently redeemable
// for a different one. One test per digest function, each varying all
// three mount fields independently.

import (
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/enrolment"
)

func baseMountBudget() config.EnrolmentBudget {
	return config.EnrolmentBudget{
		WindowSeconds: 3600, MaxCalls: 120, MaxResultBytes: 64 << 20,
		MountMaxOps: 500_000, MountMaxReadBytes: 512 << 20, MountMaxWriteBytes: 512 << 20,
	}
}

// TestEnrolmentFields_DigestBindsEachMountBudgetField is enrolment.create's
// digest (enrolmentFields.presenceDigest): changing any one of the three
// mount-plane budget fields, holding everything else fixed, must move the
// digest.
func TestEnrolmentFields_DigestBindsEachMountBudgetField(t *testing.T) {
	base := enrolmentFields{ClientID: "hermes", ProjectIDs: []string{"proj-a"}, Budget: baseMountBudget()}
	baseDigest := base.presenceDigest()

	variants := []struct {
		name   string
		budget config.EnrolmentBudget
	}{
		{"mount_max_ops", func() config.EnrolmentBudget { b := baseMountBudget(); b.MountMaxOps = 1; return b }()},
		{"mount_max_read_bytes", func() config.EnrolmentBudget { b := baseMountBudget(); b.MountMaxReadBytes = 1; return b }()},
		{"mount_max_write_bytes", func() config.EnrolmentBudget { b := baseMountBudget(); b.MountMaxWriteBytes = 1; return b }()},
	}
	for _, v := range variants {
		variant := enrolmentFields{ClientID: base.ClientID, ProjectIDs: base.ProjectIDs, Budget: v.budget}
		if variant.presenceDigest() == baseDigest {
			t.Errorf("variant %q produced the same digest as the base request", v.name)
		}
	}
	if base.presenceDigest() != baseDigest {
		t.Fatal("presenceDigest is not deterministic over the same request")
	}
}

// TestEnrolmentSignFields_DigestBindsEachMountBudgetField is the sign-path
// counterpart: enrolment.sign's digest must move on each mount-budget field
// too, independent of the CSR-key binding TestEnrolmentSignFields_DigestBindsEveryFieldIncludingTheCSRKey
// already pins.
func TestEnrolmentSignFields_DigestBindsEachMountBudgetField(t *testing.T) {
	csr := parseCSRForTest(t, genClientCSRPEM(t, "hermes"))
	base := enrolmentSignFields{ClientID: "hermes", ProjectIDs: []string{"proj-a"}, Budget: baseMountBudget()}
	baseDigest := base.presenceDigest(csr)

	variants := []struct {
		name   string
		budget config.EnrolmentBudget
	}{
		{"mount_max_ops", func() config.EnrolmentBudget { b := baseMountBudget(); b.MountMaxOps = 1; return b }()},
		{"mount_max_read_bytes", func() config.EnrolmentBudget { b := baseMountBudget(); b.MountMaxReadBytes = 1; return b }()},
		{"mount_max_write_bytes", func() config.EnrolmentBudget { b := baseMountBudget(); b.MountMaxWriteBytes = 1; return b }()},
	}
	for _, v := range variants {
		variant := enrolmentSignFields{ClientID: base.ClientID, ProjectIDs: base.ProjectIDs, Budget: v.budget}
		if variant.presenceDigest(csr) == baseDigest {
			t.Errorf("variant %q produced the same digest as the base request", v.name)
		}
	}
	if base.presenceDigest(csr) != baseDigest {
		t.Fatal("presenceDigest is not deterministic over the same request")
	}
}

// TestEnrolmentUpdateDigest_BindsEachMountBudgetField is the update-path
// counterpart, absent-aware like every other field in this digest: setting
// only one mount-budget pointer must move the digest relative to leaving it
// nil, exactly as the pre-existing three tool-plane fields already do.
func TestEnrolmentUpdateDigest_BindsEachMountBudgetField(t *testing.T) {
	base := enrolment.UpdateRequest{ClientID: "hermes"}
	baseDigest := enrolmentUpdateDigest(base)

	ops, readBytes, writeBytes := 42, int64(1<<20), int64(2<<20)
	variants := []struct {
		name string
		req  enrolment.UpdateRequest
	}{
		{"mount_max_ops", enrolment.UpdateRequest{ClientID: base.ClientID, Budget: enrolment.BudgetUpdate{MountMaxOps: &ops}}},
		{"mount_max_read_bytes", enrolment.UpdateRequest{ClientID: base.ClientID, Budget: enrolment.BudgetUpdate{MountMaxReadBytes: &readBytes}}},
		{"mount_max_write_bytes", enrolment.UpdateRequest{ClientID: base.ClientID, Budget: enrolment.BudgetUpdate{MountMaxWriteBytes: &writeBytes}}},
	}
	for _, v := range variants {
		if enrolmentUpdateDigest(v.req) == baseDigest {
			t.Errorf("variant %q (mount budget field set) produced the same digest as leaving every budget field nil", v.name)
		}
	}
	if enrolmentUpdateDigest(base) != baseDigest {
		t.Fatal("enrolmentUpdateDigest is not deterministic over the same request")
	}

	// A budget-only update naming a DIFFERENT mount field must not share a
	// digest either — the presence grant must bind to which field changed,
	// not merely "some budget field changed".
	if enrolmentUpdateDigest(variants[0].req) == enrolmentUpdateDigest(variants[1].req) {
		t.Error("two different mount-budget-only updates produced the same digest")
	}
}

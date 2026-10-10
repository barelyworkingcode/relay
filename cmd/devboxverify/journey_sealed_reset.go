package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	sealedResetPosID = "gate-sealed-reset-pos"
	sealedInstance   = "/tmp/dbv-sealed"
	sealedTimeout    = 120 * time.Second

	// emptyStoreResetReason is relay's reason for a reset whose store holds
	// nothing countable. The tray's store holds world projects, so its reason
	// never matches and a prompt for the tray cannot be answered by mistake.
	emptyStoreResetReason = "reset the sealed store, permanently deleting 0 project token(s), 0 control-plane credential(s), " +
		"0 enrolment(s) and the certificate authority that signed them, and 0 passkey(s)"
)

func runSealedResetPos(ctx context.Context, e env) (res result) {
	const id = sealedResetPosID
	fail := func(detail string) result { return result{id, stateFail, detail} }
	if err := releaseBinary(e.RelayBin); err != nil {
		return blocked(id, err.Error())
	}
	trayStore, _, err := readSettings(e.ConfigDir)
	if err != nil {
		return fail("the installed store: " + err.Error())
	}
	trayKey := trayStore.SealedKeyID
	if status, err := sealStatus(ctx, e, e.ConfigDir); err != nil {
		return fail(err.Error())
	} else if status != "" {
		return blocked(id, "the installed store is already degraded")
	}

	dir, err := prepareInstanceDir(ctx, e, sealedInstance)
	if err != nil {
		return fail(err.Error())
	}
	var inst *serveInstance
	defer func() { res = withTeardown(ctx, res, inst) }()
	inst, err = startServe(ctx, e, dir)
	if err != nil {
		return fail(err.Error())
	}
	k1, adminKey, err := inst.SettingsKeyID()
	switch {
	case err != nil:
		return fail(err.Error())
	case k1 == "":
		return fail("the instance has no sealed_key_id after start")
	case adminKey != k1:
		return fail("the instance's admin_secret is not an envelope under its sealed_key_id")
	}
	if present, err := keychainItemPresent(ctx, keychainAccountFor(dir)); err != nil {
		return fail(err.Error())
	} else if !present {
		return fail("the instance's login-keychain item is absent")
	}

	// Load-bearing: a reset destroys the keychain item its instance resolved to.
	// An instance on the installed store's key would take the installed store with it.
	if k1 == trayKey {
		return fail("the instance shares the installed store's sealing key; reset not attempted, it would destroy the installed store")
	}

	trace := "dbv-reset-" + e.Nonce
	reset, d := gatedCLI(ctx, e, emptyStoreResetReason, "--config-dir", dir, "--trace", trace, "sealed", "reset", "--json")
	if r, refused := positiveRefusal(id, reset.Stderr, d); refused {
		return r
	}
	var body struct {
		Reset bool `json:"reset"`
	}
	switch {
	case reset.Exit != 0:
		return cliExitFail(id, "sealed reset", reset)
	case json.Unmarshal([]byte(strings.TrimSpace(reset.Stdout)), &body) != nil || !body.Reset:
		return fail("sealed reset did not print {reset: true}")
	}
	events, err := instanceEvents(ctx, e, inst, trace, "sealed.reset")
	switch {
	case err != nil:
		return fail(err.Error())
	case len(events) != 1 || events[0]["status"] != "ok":
		return fail(fmt.Sprintf("want one sealed.reset event with status ok, got %d", len(events)))
	}

	k2, adminKey, err := inst.SettingsKeyID()
	switch {
	case err != nil:
		return fail(err.Error())
	case k2 == "" || k2 == k1:
		return fail("the reset did not mint a new sealing key")
	case adminKey != k2:
		return fail("after the reset admin_secret is not an envelope under the new key")
	}
	if status, err := sealStatus(ctx, e, dir); err != nil {
		return fail(err.Error())
	} else if status != "" {
		return fail("the instance is degraded after the reset")
	}

	if err := inst.Restart(ctx, e); err != nil {
		return fail(err.Error())
	}
	k3, _, err := inst.SettingsKeyID()
	switch {
	case err != nil:
		return fail(err.Error())
	case k3 != k2:
		return fail("the sealing key changed across the restart")
	}
	if status, err := sealStatus(ctx, e, dir); err != nil {
		return fail(err.Error())
	} else if status != "" {
		return fail("the instance did not start sealed again after the restart")
	}
	if present, err := keychainItemPresent(ctx, keychainAccountFor(dir)); err != nil {
		return fail(err.Error())
	} else if !present {
		return fail("the instance's login-keychain item is absent after the restart")
	}

	after, _, err := readSettings(e.ConfigDir)
	switch {
	case err != nil:
		return fail("the installed store: " + err.Error())
	case after.SealedKeyID != trayKey:
		return fail("the installed store's sealing key changed")
	}
	if status, err := sealStatus(ctx, e, e.ConfigDir); err != nil || status != "" {
		return fail("the installed store is no longer sealed and healthy")
	}
	return result{id, statePass, "reset on the instance's own store (not audited); new key minted; sealed again after restart; the installed store untouched"}
}

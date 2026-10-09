package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
)

var errSealedResetUnavailable = errors.New("sealed store reset is not available in this relay process")

type sealedResetResult struct {
	Reset bool `json:"reset"`
}

// adminSealedReset is sealed.store.reset: the CLI door to the same App.resetSealed
// the tray's "Reset Sealed Store..." item calls. The prompt (sealed.reset)
// lives in resetSealedStore; a cancelled or unshowable prompt deletes nothing.
func adminSealedReset(ctx context.Context, r *appRouter, _ json.RawMessage) (json.RawMessage, error) {
	if r.resetSealed == nil {
		return nil, errSealedResetUnavailable
	}
	if err := r.resetSealed(ctx, auditViaCLI); err != nil {
		return nil, err
	}
	return marshalAdminResult(sealedResetResult{Reset: true})
}

// sealedReset permanently deletes every sealed value, so the only prompt is the
// presence gate: there is no --yes.
func sealedReset(args []string) {
	fs := flag.NewFlagSet("sealed reset", flag.ExitOnError)
	asJSON := fs.Bool("json", false, `print {"reset": true} as JSON`)
	fs.Parse(args)
	if fs.NArg() > 0 {
		exitError("unexpected argument %q", fs.Arg(0))
	}

	raw := adminCall("relay sealed reset", "sealed.store.reset", nil)
	if *asJSON {
		printJSONLine(raw)
		return
	}
	fmt.Println("sealed store reset: relay holds a fresh key and no sealed values")
}

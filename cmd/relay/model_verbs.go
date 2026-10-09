package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

// The model verb is the CLI door to ModelCatalogOps, the core behind the
// Settings model picker.

func adminModelList(ctx context.Context, r *appRouter, _ json.RawMessage) (json.RawMessage, error) {
	return marshalAdminResult(r.modelCatalog.List(ctx))
}

func modelList(args []string) {
	fs := flag.NewFlagSet("model list", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the model catalog as JSON")
	fs.Parse(args)

	raw := adminCall("relay model list", "model.list", nil)
	var view ModelCatalogView
	decodeCLIResult(raw, &view)
	if view.Error != "" {
		exitError("%s", view.Error)
	}
	if *asJSON {
		printJSONLine(raw)
	} else {
		w := newTabWriter()
		fmt.Fprintln(w, "ID\tLABEL\tGROUP\tKIND\tTARGET")
		for _, m := range view.Models {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", m.ID, m.Label, m.Group, m.Kind, m.Target)
		}
		w.Flush()
		for _, warn := range view.Warnings {
			fmt.Fprintf(os.Stderr, "warning: %s\n", warn)
		}
	}
}

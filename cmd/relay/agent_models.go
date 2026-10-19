package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

// ModelOffer lists the models a session kind offers, as the bare ids an agent
// entry stores: claude's aliases, codex's slugs without their prefix, and the
// model endpoint's ids for pi and chat.
type ModelOffer func(ctx context.Context, kind string) ([]string, error)

var errModelsUnavailable = errors.New("model list unavailable")

const codexOfferedPrefix = "codex/"

// newModelOffer builds the offer from the same two reads the Settings model
// picker uses. A read error and an empty list both answer errModelsUnavailable:
// a start never skips the check because the catalogue could not be read.
func newModelOffer(c *ModelCatalogOps) ModelOffer {
	return func(ctx context.Context, kind string) ([]string, error) {
		switch kind {
		case KindClaude:
			return []string{"haiku", "sonnet", "opus"}, nil
		case KindCodex:
			if c == nil || c.HostModels == nil {
				return nil, errModelsUnavailable
			}
			rows, err := c.HostModels(ctx)
			if err != nil {
				slog.Warn("model offer: session host model list failed", "kind", kind, "error", err)
				return nil, errModelsUnavailable
			}
			var out []string
			for _, r := range rows {
				if r.Provider == KindCodex {
					out = append(out, strings.TrimPrefix(r.Value, codexOfferedPrefix))
				}
			}
			return nonEmptyOffer(out)
		case KindPi, KindChat:
			if c == nil || c.BrokerRows == nil {
				return nil, errModelsUnavailable
			}
			rows, err := c.BrokerRows(ctx)
			if err != nil {
				slog.Warn("model offer: model broker list failed", "kind", kind, "error", err)
				return nil, errModelsUnavailable
			}
			var out []string
			for _, r := range rows {
				if !r.System {
					out = append(out, r.ID)
				}
			}
			return nonEmptyOffer(out)
		default:
			return nil, fmt.Errorf("%w: no model list for kind %q", errModelsUnavailable, kind)
		}
	}
}

func nonEmptyOffer(models []string) ([]string, error) {
	if len(models) == 0 {
		return nil, errModelsUnavailable
	}
	return models, nil
}

// rawModelOffered reports whether a raw session model names a model in its
// kind's offer: codex drops "codex/"; pi keeps <id> of "pi/<provider>/<id>"
// (a malformed pi model is never offered); chat matches the id, or <x> of
// "llama/<x>" or "mlx/<x>", the spellings docs/model-endpoint.md accepts.
func rawModelOffered(kind, model string, offered []string) bool {
	switch kind {
	case KindCodex:
		return slices.Contains(offered, strings.TrimPrefix(model, codexOfferedPrefix))
	case KindPi:
		parts := strings.SplitN(model, "/", 3)
		if len(parts) != 3 || parts[0] != KindPi || parts[1] == "" || parts[2] == "" {
			return false
		}
		return slices.Contains(offered, parts[2])
	case KindChat:
		if slices.Contains(offered, model) {
			return true
		}
		for _, prefix := range []string{"llama/", "mlx/"} {
			if rest, ok := strings.CutPrefix(model, prefix); ok && rest != "" && slices.Contains(offered, rest) {
				return true
			}
		}
	}
	return false
}

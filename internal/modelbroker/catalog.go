package modelbroker

import (
	"context"
	"sync"
)

// Row is one entry of the upstream /v1/models listing, trimmed to what the
// broker needs to classify and enforce a grant against (spec-model-broker.md
// §6.2). ID is the router id exactly as relayLLM dispatches it: a bare
// managed alias, "<endpoint>/<id>", a virtual model name, or an Anthropic
// modelMap key.
type Row struct {
	ID string
	// OwnedBy is the catalog row's owned_by field verbatim. It is a display
	// label, not a stable enum — see classify in normalise.go for how it is
	// read.
	OwnedBy string
	// Target is set only for a modelMap row (OwnedBy == "anthropic-map"):
	// the router id the key rewrites to before dispatch.
	Target string
}

// FetchFunc retrieves a fresh catalog snapshot, e.g. by calling relayLLM's
// GET /v1/models over router.sock. Injected so this package never dials
// anything itself.
type FetchFunc func(ctx context.Context) ([]Row, error)

// Cache holds the last-fetched catalog and refetches on demand. It is safe
// for concurrent use.
type Cache struct {
	fetch FetchFunc

	mu      sync.Mutex
	rows    []Row
	index   map[string]int
	fetched bool
}

// NewCache builds a Cache around fetch. The first call to Snapshot or
// Resolve performs the first fetch; Cache never fetches eagerly at
// construction, so building one has no side effect.
func NewCache(fetch FetchFunc) *Cache {
	return &Cache{fetch: fetch}
}

func (c *Cache) refetchLocked(ctx context.Context) error {
	rows, err := c.fetch(ctx)
	if err != nil {
		return err
	}
	index := make(map[string]int, len(rows))
	for i, row := range rows {
		index[row.ID] = i
	}
	c.rows = rows
	c.index = index
	c.fetched = true
	return nil
}

// Snapshot returns the current catalog, fetching first if this Cache has
// never fetched. It never refetches just because time has passed — TTL
// policy belongs to the caller (R-M1b), not this package.
func (c *Cache) Snapshot(ctx context.Context) ([]Row, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fetched {
		if err := c.refetchLocked(ctx); err != nil {
			return nil, err
		}
	}
	return append([]Row(nil), c.rows...), nil
}

// Resolve returns the current catalog, guaranteeing that id has been looked
// up against a fresh fetch at least once: if id is absent from what is
// currently cached, Resolve fetches exactly one more time before returning.
// This is deliberate and bounded — a model that just finished loading
// should not need a second request to appear, but a genuinely unknown id
// must not turn every request for it into a retry loop against the
// upstream, so at most one extra fetch happens per call regardless of how
// many times the miss recurs.
func (c *Cache) Resolve(ctx context.Context, id string) ([]Row, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fetched {
		if err := c.refetchLocked(ctx); err != nil {
			return nil, err
		}
	}
	if _, ok := c.index[id]; !ok {
		if err := c.refetchLocked(ctx); err != nil {
			return nil, err
		}
	}
	return append([]Row(nil), c.rows...), nil
}

// Filter narrows rows to what a caller holding grant may see in a /v1/models
// response, per spec §6.3: every row whose id (or, for a modelMap row, whose
// target) Allowed would grant. The Target field is always stripped from the
// output — it is relayLLM's internal dispatch detail, never the caller's
// business — including on rows Filter drops nothing from.
func Filter(rows []Row, grant []string) []Row {
	out := make([]Row, 0, len(rows))
	for _, row := range rows {
		if _, ok, _ := Allowed(row.ID, grant, rows); ok {
			out = append(out, Row{ID: row.ID, OwnedBy: row.OwnedBy})
		}
	}
	return out
}

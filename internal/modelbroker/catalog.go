package modelbroker

import (
	"context"
	"sync"
	"time"
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

// Cache holds the last-fetched catalog and refetches once ttl has elapsed
// since the last successful fetch, or once on a Resolve miss. It is safe for
// concurrent use.
type Cache struct {
	fetch FetchFunc
	ttl   time.Duration
	now   func() time.Time

	mu        sync.Mutex
	rows      []Row
	index     map[string]int
	fetched   bool
	fetchedAt time.Time
}

// NewCache builds a Cache around fetch with the given expiry. The first call
// to Snapshot or Resolve performs the first fetch; Cache never fetches
// eagerly at construction, so building one has no side effect. ttl <= 0
// means the cache never expires on its own — only a Resolve miss refetches.
func NewCache(fetch FetchFunc, ttl time.Duration) *Cache {
	return &Cache{fetch: fetch, ttl: ttl, now: time.Now}
}

// SetClock overrides the clock Cache uses to judge expiry; a test seam,
// defaulting to time.Now.
func (c *Cache) SetClock(now func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// expiredLocked reports whether the cached rows are stale enough to refetch:
// only once something has actually been fetched, and only when ttl is
// positive — a ttl <= 0 cache is never expired by time alone.
func (c *Cache) expiredLocked() bool {
	return c.fetched && c.ttl > 0 && c.now().Sub(c.fetchedAt) >= c.ttl
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
	c.fetchedAt = c.now()
	return nil
}

// Snapshot returns the current catalog, fetching first if this Cache has
// never fetched or if ttl has elapsed since the last successful fetch. A
// failed refetch returns the error and serves nothing stale: the prior rows
// and fetchedAt are left in place so the next call retries rather than
// papering over an unreadable upstream.
func (c *Cache) Snapshot(ctx context.Context) ([]Row, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fetched || c.expiredLocked() {
		if err := c.refetchLocked(ctx); err != nil {
			return nil, err
		}
	}
	return append([]Row(nil), c.rows...), nil
}

// Resolve returns the current catalog, guaranteeing that id has been looked
// up against a fresh-enough fetch: it fetches when the cache has never
// filled or has expired, or once more on a miss if this call hasn't already
// fetched. This is deliberate and bounded — a model that just finished
// loading should not need a second request to appear, but a genuinely
// unknown id must not turn every request for it into a retry loop against
// the upstream, so Resolve makes at most one fetch per call regardless of
// which branch triggers it. A failed fetch returns the error and serves
// nothing stale, exactly as Snapshot does.
func (c *Cache) Resolve(ctx context.Context, id string) ([]Row, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fetchedThisCall := false
	if !c.fetched || c.expiredLocked() {
		if err := c.refetchLocked(ctx); err != nil {
			return nil, err
		}
		fetchedThisCall = true
	}
	if _, ok := c.index[id]; !ok && !fetchedThisCall {
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

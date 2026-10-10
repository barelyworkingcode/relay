package harness

import "net/http"

// ModelHTTP returns a client for the model endpoint's TCP listener
// (ready.json listeners.model). token "" sends no Authorization header; set an
// X-Relay-Key through ReqOpts.Header.
func (i *Instance) ModelHTTP(token string) *Client {
	i.t.Helper()
	addr := i.Ready.Listeners["model"]
	if addr == "" {
		i.t.Fatalf("ready.json has no listeners.model; the model endpoint has no TCP listener")
	}
	return &Client{t: i.t, hc: &http.Client{Timeout: requestDeadline}, base: "http://" + addr, token: token}
}

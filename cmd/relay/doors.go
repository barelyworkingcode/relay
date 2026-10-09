package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/presence"
)

// doorsSchema is the version of the doors document. A change that removes or
// renames a field, or a vocabulary value, bumps it.
const doorsSchema = 1

// webauthnGate is the one gate outside presence.GatedOps: the login ceremony
// is verified against a registered passkey, not a presence prompt.
const webauthnGate = "webauthn"

const (
	doorKindHTTP   = "http"
	doorKindIPC    = "ipc"
	doorKindBridge = "bridge"
	doorKindCLI    = "cli"

	// credentialLocal is a CLI verb that reaches no door: it reads its own
	// files or starts the server.
	credentialLocal = "local"
)

// Door is one way in, with the credential that authorizes it and the owner
// gates its core may require. It holds compile-time facts only: no id, config
// value, path or address.
type Door struct {
	Kind       string   `json:"kind"`
	Name       string   `json:"name"`
	Method     string   `json:"method,omitempty"`
	Path       string   `json:"path,omitempty"`
	Listeners  []string `json:"listeners,omitempty"`
	Credential string   `json:"credential"`
	OwnerGated bool     `json:"owner_gated"`
	Gates      []string `json:"gates,omitempty"`
	Calls      []string `json:"calls,omitempty"`
}

type doorsDocument struct {
	Schema   int    `json:"schema"`
	Version  string `json:"version"`
	Headless bool   `json:"headless"`
	Doors    []Door `json:"doors"`
}

// Credential vocabularies, closed per kind. A value outside its set fails
// list(), so a new credential class cannot ship unnamed.
var (
	httpCredentials   = []string{"read", "configure", "grant", "execute", "proxy", "chief_of_staff", "public"}
	ipcCredentials    = []string{"settings_window"}
	bridgeCredentials = []string{"socket", "operator", "admin_token", "project", "launch_identity", "launch_secret"}
)

// routeDoor is one registered pattern with the transports it was registered on.
type routeDoor struct {
	info      control.RouteInfo
	listeners map[control.Transport]bool
}

// doorCatalog collects the routes the live muxes registered and, with the
// dispatch tables of the same binary, answers `relay doors`. It is built from
// registries, never from a hand-kept list.
type doorCatalog struct {
	headless bool

	mu     sync.Mutex
	routes map[string]*routeDoor
	login  []string
	errs   []error
}

func newDoorCatalog(headless bool) *doorCatalog {
	return &doorCatalog{headless: headless, routes: map[string]*routeDoor{}}
}

// recordRoute is wired to control.RouteRegistrar.Record on every mux.
func (c *doorCatalog) recordRoute(ri control.RouteInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, ok := c.routes[ri.Pattern]; ok {
		if prev.info.Class != ri.Class || !slices.Equal(prev.info.Gates, ri.Gates) {
			c.errs = append(c.errs, fmt.Errorf("http door %q is registered with different classes or gates on two listeners", ri.Pattern))
		}
		prev.listeners[ri.Transport] = true
		return
	}
	c.routes[ri.Pattern] = &routeDoor{info: ri, listeners: map[control.Transport]bool{ri.Transport: true}}
}

// recordLoginRoutes records the public login patterns of a bound TCP listener.
func (c *doorCatalog) recordLoginRoutes(patterns []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.login = append([]string(nil), patterns...)
}

// splitPattern returns the method and path of a ServeMux pattern. A pattern
// without a method matches every method.
func splitPattern(pattern string) (method, path string) {
	if m, p, ok := strings.Cut(pattern, " "); ok && m == strings.ToUpper(m) {
		return m, strings.TrimSpace(p)
	}
	return "", pattern
}

func gatedFields(d *Door, gates []string) {
	if len(gates) == 0 {
		return
	}
	d.Gates = append([]string(nil), gates...)
	sort.Strings(d.Gates)
	d.OwnerGated = true
}

func validGate(gate string) bool {
	return gate == webauthnGate || slices.Contains(presence.GatedOps, gate)
}

func checkDoor(d Door, credentials []string) error {
	if !slices.Contains(credentials, d.Credential) {
		return fmt.Errorf("%s door %q has credential %q outside its vocabulary", d.Kind, d.Name, d.Credential)
	}
	for _, g := range d.Gates {
		if !validGate(g) {
			return fmt.Errorf("%s door %q names gate %q, which is not a presence operation", d.Kind, d.Name, g)
		}
	}
	return nil
}

func (c *doorCatalog) httpDoors() ([]Door, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.errs) > 0 {
		return nil, c.errs[0]
	}
	var doors []Door
	for pattern, rd := range c.routes {
		method, path := splitPattern(pattern)
		d := Door{Kind: doorKindHTTP, Name: pattern, Method: method, Path: path, Credential: string(rd.info.Class)}
		for t := range rd.listeners {
			d.Listeners = append(d.Listeners, string(t))
		}
		sort.Strings(d.Listeners)
		gatedFields(&d, rd.info.Gates)
		if err := checkDoor(d, httpCredentials); err != nil {
			return nil, err
		}
		doors = append(doors, d)
	}
	for _, pattern := range c.login {
		method, path := splitPattern(pattern)
		d := Door{Kind: doorKindHTTP, Name: pattern, Method: method, Path: path, Listeners: []string{string(control.TransportTCP)}, Credential: "public"}
		if pattern == "POST /relay/login/verify" {
			gatedFields(&d, []string{webauthnGate})
		}
		if err := checkDoor(d, httpCredentials); err != nil {
			return nil, err
		}
		doors = append(doors, d)
	}
	return doors, nil
}

func ipcDoors() ([]Door, error) {
	var doors []Door
	for name, entry := range ipcHandlers {
		d := Door{Kind: doorKindIPC, Name: name, Credential: "settings_window"}
		gatedFields(&d, entry.gates)
		if err := checkDoor(d, ipcCredentials); err != nil {
			return nil, err
		}
		doors = append(doors, d)
	}
	return doors, nil
}

func adminOpDoorName(op string) string { return "admin_op:" + op }

func bridgeDoors() ([]Door, error) {
	var doors []Door
	for _, h := range bridge.Handlers() {
		// admin_op is listed per operation below, never as a bare type.
		if h.Type == bridge.ReqAdminOp {
			continue
		}
		d := Door{Kind: doorKindBridge, Name: h.Type, Credential: h.Credential}
		if err := checkDoor(d, bridgeCredentials); err != nil {
			return nil, err
		}
		doors = append(doors, d)
	}
	for op, entry := range adminOps {
		d := Door{Kind: doorKindBridge, Name: adminOpDoorName(op), Credential: string(entry.caller)}
		gatedFields(&d, entry.gates)
		if err := checkDoor(d, bridgeCredentials); err != nil {
			return nil, err
		}
		doors = append(doors, d)
	}
	return doors, nil
}

// cliDoors resolves each verb's calls against the bridge doors. A verb's
// credential is its calls' shared credential, or local when it calls nothing.
func cliDoors(bridgeByName map[string]Door) ([]Door, error) {
	var doors []Door
	for _, verb := range cliVerbTable() {
		d := Door{Kind: doorKindCLI, Name: "relay " + verb.Name, Credential: credentialLocal}
		var gates []string
		for _, call := range verb.Calls {
			target, ok := bridgeByName[call]
			if !ok {
				return nil, fmt.Errorf("cli door %q calls %q, which names no bridge door", d.Name, call)
			}
			if len(d.Calls) > 0 && target.Credential != d.Credential {
				return nil, fmt.Errorf("cli door %q calls doors with different credentials (%s and %s)", d.Name, d.Credential, target.Credential)
			}
			d.Credential = target.Credential
			d.Calls = append(d.Calls, call)
			gates = append(gates, target.Gates...)
		}
		sort.Strings(gates)
		gatedFields(&d, slices.Compact(gates))
		if d.Credential != credentialLocal {
			if err := checkDoor(d, bridgeCredentials); err != nil {
				return nil, err
			}
		}
		doors = append(doors, d)
	}
	return doors, nil
}

var doorKindOrder = map[string]int{doorKindHTTP: 0, doorKindIPC: 1, doorKindBridge: 2, doorKindCLI: 3}

func (c *doorCatalog) list() (doorsDocument, error) {
	var all []Door
	httpDoorList, err := c.httpDoors()
	if err != nil {
		return doorsDocument{}, err
	}
	ipc, err := ipcDoors()
	if err != nil {
		return doorsDocument{}, err
	}
	br, err := bridgeDoors()
	if err != nil {
		return doorsDocument{}, err
	}
	byName := make(map[string]Door, len(br))
	for _, d := range br {
		byName[d.Name] = d
	}
	cli, err := cliDoors(byName)
	if err != nil {
		return doorsDocument{}, err
	}
	all = append(all, httpDoorList...)
	all = append(all, ipc...)
	all = append(all, br...)
	all = append(all, cli...)
	sort.Slice(all, func(i, j int) bool {
		if all[i].Kind != all[j].Kind {
			return doorKindOrder[all[i].Kind] < doorKindOrder[all[j].Kind]
		}
		return all[i].Name < all[j].Name
	})
	return doorsDocument{Schema: doorsSchema, Version: buildVersion, Headless: c.headless, Doors: all}, nil
}

var errDoorsUnavailable = errors.New("the doors catalogue is not available in this relay process")

// adminDoorsList is doors.list. A read, so its event is written here: the
// catalogue is a view of tables other operations also read.
func adminDoorsList(ctx context.Context, r *appRouter, _ json.RawMessage) (_ json.RawMessage, err error) {
	ev := logging.BeginEvent(ctx, "doors.list")
	defer func() { endEvent(ev, err) }()
	if r.doors == nil {
		return nil, errDoorsUnavailable
	}
	doc, err := r.doors.list()
	if err != nil {
		return nil, err
	}
	ev.Set("count", len(doc.Doors))
	return marshalAdminResult(doc)
}

const doorsUsage = "Usage: relay [--config-dir DIR] doors [--json]"

// runDoorsCommand prints the doors document the running server builds from its
// own tables. It is the only CLI path to the catalogue: this process holds none.
func runDoorsCommand(args []string) {
	fs := flag.NewFlagSet("doors", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the doors document as one line of JSON")
	fs.Parse(args)
	if fs.NArg() > 0 {
		exitError("unexpected argument %q\n%s", fs.Arg(0), doorsUsage)
	}

	raw := adminCall("relay doors", "doors.list", nil)
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var doc doorsDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		exitError("parse response: %v", err)
	}
	counts := map[string]int{}
	for _, d := range doc.Doors {
		counts[d.Kind]++
	}
	fmt.Printf("%d doors: %d http, %d ipc, %d bridge, %d cli\n", len(doc.Doors), counts[doorKindHTTP], counts[doorKindIPC], counts[doorKindBridge], counts[doorKindCLI])
}

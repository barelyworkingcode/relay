package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// worldVersion is relay's pin: the world major these journeys were written
// against. The marker's world_version must equal it.
const worldVersion = 1

const (
	markerAbsent     = "not a test machine: run devboxWorld bootstrap on a VM"
	markerUnreadable = "marker is not readable"
	maxMarkerBytes   = 1 << 20
	maxWorldBytes    = 8 << 20
)

type worldMarker struct {
	Schema        int
	WorldCheckout string
	WorldRoot     string
	WorldVersion  int
	WrittenAt     string
}

type worldProject struct{ Key, Name, Mode, Folder string }

// worldMCP is world.json's relay_mcp: the MCP every world project grants and
// the tool pattern it grants.
type worldMCP struct{ ID, Tools string }

type world struct {
	Version        int
	Root, Checkout string
	Projects       map[string]worldProject
	Fixtures       map[string]bool
	MCP            worldMCP
}

// worldView is what one journey may see of the world: only the fixtures it
// declared. The zero value admits nothing.
type worldView struct {
	w     world
	needs map[string]bool
}

var intToken = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

func markerRefusal(reason string) error {
	return errors.New("not a test machine: " + reason + "; run devboxWorld bootstrap on a VM")
}

// markerPath reads HOME at call time, not the package's cached home, so a
// test that isolates HOME never reaches the machine's real marker.
func markerPath() string {
	if p := os.Getenv("DEVBOXWORLD_MARKER"); p != "" {
		return p
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "devboxWorld", "machine.json")
}

// hostIsVM deliberately has no override: a marker restored onto a host from
// a backup must still refuse.
func hostIsVM() (bool, error) {
	v, err := unix.SysctlUint32("kern.hv_vmm_present")
	if err != nil {
		return false, err
	}
	return v == 1, nil
}

// readMarker follows devboxWorld's reference reader check for check; the
// first failure wins and no reason carries OS error text.
func readMarker(path string, isVM func() (bool, error)) (worldMarker, error) {
	fi, lstatErr := os.Lstat(path)
	if errors.Is(lstatErr, fs.ErrNotExist) {
		return worldMarker{}, errors.New(markerAbsent)
	}
	if vm, err := isVM(); err != nil || !vm {
		return worldMarker{}, markerRefusal("not a VM: kern.hv_vmm_present is not 1")
	}
	if lstatErr != nil {
		return worldMarker{}, markerRefusal(markerUnreadable)
	}
	if !fi.Mode().IsRegular() {
		return worldMarker{}, markerRefusal("marker is not a regular file")
	}
	if mode := permBits(fi); mode&0o077 != 0 {
		return worldMarker{}, markerRefusal(fmt.Sprintf("marker is open to group or others (mode %04o)", mode))
	}
	// O_NOFOLLOW: a symlink swapped in after the lstat is refused, not followed.
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return worldMarker{}, markerRefusal(markerUnreadable)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxMarkerBytes))
	_ = f.Close()
	if err != nil {
		return worldMarker{}, markerRefusal(markerUnreadable)
	}
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return worldMarker{}, markerRefusal("marker is not valid JSON")
	}
	var doc map[string]json.RawMessage
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) || json.Unmarshal(raw, &doc) != nil {
		return worldMarker{}, markerRefusal("marker is not a JSON object")
	}
	if s, ok := doc["schema"]; !ok || string(bytes.TrimSpace(s)) != "1" {
		return worldMarker{}, markerRefusal("marker schema is not 1")
	}
	for _, field := range []string{"world_checkout", "world_root", "world_version", "written_at"} {
		if _, ok := doc[field]; !ok {
			return worldMarker{}, markerRefusal("marker lacks " + field)
		}
	}
	m := worldMarker{Schema: 1}
	for _, f := range []struct {
		name string
		dst  *string
	}{{"world_checkout", &m.WorldCheckout}, {"world_root", &m.WorldRoot}} {
		s, ok := jsonString(doc[f.name])
		if !ok || !filepath.IsAbs(s) {
			return worldMarker{}, markerRefusal("marker " + f.name + " is not an absolute path")
		}
		*f.dst = s
	}
	v, ok := positiveInt(doc["world_version"])
	if !ok {
		return worldMarker{}, markerRefusal("marker world_version is not a positive integer")
	}
	m.WorldVersion = v
	m.WrittenAt, _ = jsonString(doc["written_at"])
	return m, nil
}

// permBits is the stat mode's permission bits including setuid, setgid and
// sticky, which os.FileMode.Perm drops; the refusal prints them.
func permBits(fi os.FileInfo) uint32 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint32(st.Mode) & 0o7777
	}
	return uint32(fi.Mode().Perm())
}

func jsonString(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	var s string
	if !bytes.HasPrefix(raw, []byte(`"`)) || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// positiveInt accepts only an integer token: 1.0, "1" and true are refused.
func positiveInt(raw json.RawMessage) (int, bool) {
	tok := string(bytes.TrimSpace(raw))
	if !intToken.MatchString(tok) {
		return 0, false
	}
	n, err := strconv.Atoi(tok)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// loadWorld reports the first failure in devboxWorld's published order; main
// prefixes it with "BLOCKED fixture: ".
func loadWorld(m worldMarker) (world, error) {
	f, err := os.Open(filepath.Join(m.WorldCheckout, "data", "world.json"))
	if err != nil {
		return world{}, errors.New("world data is not readable")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxWorldBytes))
	_ = f.Close()
	if err != nil {
		return world{}, errors.New("world data is not readable")
	}
	var doc map[string]json.RawMessage
	if !utf8.Valid(raw) || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) || json.Unmarshal(raw, &doc) != nil {
		return world{}, errors.New("world data is not valid JSON")
	}
	v, ok := positiveInt(doc["world_version"])
	if !ok {
		return world{}, errors.New("world_version is not a positive integer")
	}
	ids, ok := stringList(doc["fixtures"])
	if !ok {
		return world{}, errors.New("fixtures is not a list of strings")
	}
	var projects []json.RawMessage
	if !bytes.HasPrefix(bytes.TrimSpace(doc["projects"]), []byte("[")) || json.Unmarshal(doc["projects"], &projects) != nil {
		return world{}, errors.New("projects is not a list")
	}
	w := world{Version: v, Root: m.WorldRoot, Checkout: m.WorldCheckout, Projects: map[string]worldProject{}, Fixtures: map[string]bool{}}
	for i, pr := range projects {
		p, ok := parseProject(pr)
		if _, dup := w.Projects[p.Key]; !ok || dup {
			return world{}, fmt.Errorf("project %d is malformed", i)
		}
		p.Folder = filepath.Join(m.WorldRoot, p.Name)
		w.Projects[p.Key] = p
	}
	if w.MCP, ok = parseMCP(doc["relay_mcp"]); !ok {
		return world{}, errors.New("relay_mcp is malformed")
	}
	for _, id := range ids {
		if !w.resolves(id) {
			return world{}, fmt.Errorf("fixture %s does not resolve", id)
		}
		w.Fixtures[id] = true
	}
	return w, nil
}

// nonEmptyStrings reads each named field of a JSON object into its
// destination; any field absent, not a string or empty refuses the object.
func nonEmptyStrings(raw json.RawMessage, fields map[string]*string) bool {
	var obj map[string]json.RawMessage
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) || json.Unmarshal(raw, &obj) != nil {
		return false
	}
	for name, dst := range fields {
		s, ok := jsonString(obj[name])
		if !ok || s == "" {
			return false
		}
		*dst = s
	}
	return true
}

func parseMCP(raw json.RawMessage) (worldMCP, bool) {
	var m worldMCP
	if !nonEmptyStrings(raw, map[string]*string{"id": &m.ID, "tools": &m.Tools}) {
		return worldMCP{}, false
	}
	return m, true
}

func stringList(raw json.RawMessage) ([]string, bool) {
	var items []json.RawMessage
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) || json.Unmarshal(raw, &items) != nil {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := jsonString(it)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func parseProject(raw json.RawMessage) (worldProject, bool) {
	var p worldProject
	if !nonEmptyStrings(raw, map[string]*string{"key": &p.Key, "name": &p.Name, "mode": &p.Mode}) {
		return worldProject{}, false
	}
	// The name becomes a folder under the world root, so it must stay one.
	if strings.ContainsRune(p.Name, '/') || p.Name == "." || p.Name == ".." {
		return worldProject{}, false
	}
	return p, true
}

// resolves checks an id's shape against the projects; the file content is
// devboxWorld's own validate-data concern.
func (w world) resolves(id string) bool {
	if key, ok := strings.CutPrefix(id, "project:"); ok {
		_, known := w.Projects[key]
		return known
	}
	rest, ok := strings.CutPrefix(id, "file:")
	if !ok {
		return false
	}
	key, rel, ok := strings.Cut(rest, "/")
	_, known := w.Projects[key]
	return ok && known && rel != "" && !strings.HasPrefix(rel, "/") && !strings.Contains(rel, "..")
}

func (w world) scoped(needs []string) worldView {
	v := worldView{w: w, needs: make(map[string]bool, len(needs))}
	for _, n := range needs {
		v.needs[n] = true
	}
	return v
}

func (v worldView) admit(id string) error {
	if !v.needs[id] {
		return errors.New("undeclared fixture " + id)
	}
	if !v.w.Fixtures[id] {
		return fmt.Errorf("fixture %s is not in world v%d", id, v.w.Version)
	}
	return nil
}

// relayMCP is a published constant, not a catalogue id, so it needs no
// declaration.
func (v worldView) relayMCP() worldMCP { return v.w.MCP }

func (v worldView) project(key string) (worldProject, error) {
	id := "project:" + key
	if err := v.admit(id); err != nil {
		return worldProject{}, err
	}
	p, ok := v.w.Projects[key]
	if !ok {
		return worldProject{}, fmt.Errorf("fixture %s is not in world v%d", id, v.w.Version)
	}
	return p, nil
}

func (v worldView) file(key, rel string) (string, error) {
	id := "file:" + key + "/" + rel
	if err := v.admit(id); err != nil {
		return "", err
	}
	p, ok := v.w.Projects[key]
	if !ok {
		return "", fmt.Errorf("fixture %s is not in world v%d", id, v.w.Version)
	}
	return filepath.Join(p.Folder, rel), nil
}

func missingFixtures(js []journey, w world) []string {
	var out []string
	for _, j := range js {
		var miss []string
		for _, n := range j.Needs {
			if !w.Fixtures[n] {
				miss = append(miss, n)
			}
		}
		if len(miss) > 0 {
			out = append(out, j.ID+" needs "+strings.Join(miss, ", "))
		}
	}
	return out
}

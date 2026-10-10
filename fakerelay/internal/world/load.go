package world

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// Load reads DIR/world.json. A missing file is the empty world. Every error
// reads "world.json: <json path>: <problem>".
func Load(dir string) (*World, error) {
	w := &World{Schema: 1, Listeners: Listeners{API: "127.0.0.1:0"}}
	b, err := os.ReadFile(filepath.Join(dir, "world.json"))
	if errors.Is(err, os.ErrNotExist) {
		return w, nil
	}
	if err != nil {
		return nil, fmt.Errorf("world.json: %w", err)
	}
	// A file must state its own schema; only the absent file is the empty world.
	w.Schema = 0
	var raw any
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("world.json: invalid JSON: %w", err)
	}
	if dec.More() {
		return nil, errors.New("world.json: invalid JSON: trailing data")
	}
	if err := checkKeys("", raw, reflect.TypeOf(*w)); err != nil {
		return nil, fmt.Errorf("world.json: %w", err)
	}
	if err := json.Unmarshal(b, w); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			return nil, fmt.Errorf("world.json: %s: expected %s, got %s", te.Field, te.Type, te.Value)
		}
		return nil, fmt.Errorf("world.json: %w", err)
	}
	for i := range w.Models {
		if w.Models[i].Reply.Kind == "" {
			w.Models[i].Reply.Kind = "echo"
		}
	}
	for i := range w.Hosts {
		if w.Hosts[i].Agent == "" {
			w.Hosts[i].Agent = "none"
		}
	}
	if err := Validate(w); err != nil {
		return nil, fmt.Errorf("world.json: %w", err)
	}
	return w, nil
}

var (
	unmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	rawType         = reflect.TypeOf(json.RawMessage(nil))
)

// checkKeys walks the decoded JSON beside the Go type so an unknown key is
// reported with its path; encoding/json alone names no path.
func checkKeys(path string, v any, t reflect.Type) error {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == rawType || reflect.PointerTo(t).Implements(unmarshalerType) {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		fields := jsonFields(t)
		keys := sortedKeys(m)
		for _, k := range keys {
			ft, ok := fields[k]
			if !ok {
				return fmt.Errorf("%s: unknown key", join(path, k))
			}
			if err := checkKeys(join(path, k), m[k], ft); err != nil {
				return err
			}
		}
	case reflect.Slice:
		a, ok := v.([]any)
		if !ok {
			return nil
		}
		for i, e := range a {
			if err := checkKeys(fmt.Sprintf("%s[%d]", path, i), e, t.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map:
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		for _, k := range sortedKeys(m) {
			if err := checkKeys(join(path, k), m[k], t.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "-" || !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func join(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

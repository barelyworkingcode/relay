package main

import (
	"fmt"
	"reflect"
	"testing"
)

// TestSettings_CloneCopiesEveryField populates a Settings with distinct,
// non-zero values in every reachable field via reflection, clones it, and
// walks both trees together looking for any slice, map or pointer the clone
// still shares with the original. A field added to Settings (or to anything
// it reaches) that Clone forgets to deep-copy is invisible to a plain
// equality check — a shallow struct copy carries the same values across —
// but it shows up here as shared backing storage, which is exactly the
// condition under which mutating the copy would corrupt the original.
func TestSettings_CloneCopiesEveryField(t *testing.T) {
	orig := &Settings{}
	counter := 0
	fillDistinct(reflect.ValueOf(orig).Elem(), &counter)

	clone := orig.Clone()

	if !reflect.DeepEqual(orig, clone) {
		t.Fatalf("Clone did not preserve values:\norig:  %+v\nclone: %+v", orig, clone)
	}

	checkIndependent(t, "Settings", reflect.ValueOf(orig).Elem(), reflect.ValueOf(clone).Elem())
}

// fillDistinct sets every leaf field reachable from v to a distinct
// non-zero value, and every slice/map it creates has exactly one element —
// enough for checkIndependent to have something to compare pointers on.
func fillDistinct(v reflect.Value, counter *int) {
	switch v.Kind() {
	case reflect.String:
		*counter++
		v.SetString(fmt.Sprintf("s%d", *counter))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		*counter++
		v.SetInt(int64(*counter))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		*counter++
		v.SetUint(uint64(*counter))
	case reflect.Ptr:
		p := reflect.New(v.Type().Elem())
		fillDistinct(p.Elem(), counter)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillDistinct(s.Index(0), counter)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		fillDistinct(key, counter)
		val := reflect.New(v.Type().Elem()).Elem()
		fillDistinct(val, counter)
		m.SetMapIndex(key, val)
		v.Set(m)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			fillDistinct(v.Field(i), counter)
		}
	default:
		panic(fmt.Sprintf("fillDistinct: unhandled kind %s at %s — Settings gained a field type this test does not know how to populate", v.Kind(), v.Type()))
	}
}

// checkIndependent walks orig and clone together and fails for any slice,
// map or pointer clone shares with orig. A shared slice or map with zero
// elements is not checked — an empty slice's backing pointer is not
// meaningful — but fillDistinct never produces one, so every reachable
// collection here has exactly one element to check.
func checkIndependent(t *testing.T, path string, orig, clone reflect.Value) {
	t.Helper()
	switch clone.Kind() {
	case reflect.Ptr:
		if orig.IsNil() || clone.IsNil() {
			return
		}
		if orig.Pointer() == clone.Pointer() {
			t.Errorf("%s: clone shares the original's pointer", path)
			return
		}
		checkIndependent(t, path, orig.Elem(), clone.Elem())
	case reflect.Slice:
		if orig.IsNil() || clone.IsNil() || orig.Len() == 0 {
			return
		}
		if orig.Pointer() == clone.Pointer() {
			t.Errorf("%s: clone shares the original's backing array", path)
			return
		}
		for i := 0; i < orig.Len() && i < clone.Len(); i++ {
			checkIndependent(t, fmt.Sprintf("%s[%d]", path, i), orig.Index(i), clone.Index(i))
		}
	case reflect.Map:
		if orig.IsNil() || clone.IsNil() || orig.Len() == 0 {
			return
		}
		if orig.Pointer() == clone.Pointer() {
			t.Errorf("%s: clone shares the original's map", path)
			return
		}
		iter := orig.MapRange()
		for iter.Next() {
			cv := clone.MapIndex(iter.Key())
			if !cv.IsValid() {
				t.Errorf("%s[%v]: key present in original, missing in clone", path, iter.Key())
				continue
			}
			checkIndependent(t, fmt.Sprintf("%s[%v]", path, iter.Key()), iter.Value(), cv)
		}
	case reflect.Struct:
		for i := 0; i < clone.NumField(); i++ {
			checkIndependent(t, path+"."+clone.Type().Field(i).Name, orig.Field(i), clone.Field(i))
		}
	}
}

// TestSettings_CloneNilStaysNil asserts Clone preserves the nil/non-nil
// distinction on every slice and map field it copies, rather than turning a
// nil into an empty value. normalize() and a hand-written settings.json both
// depend on that distinction surviving a copy: a nil `allowed_tools` means
// "no change", an empty one means "clear", and the two must not collapse
// into each other on the way through Clone.
func TestSettings_CloneNilStaysNil(t *testing.T) {
	orig := &Settings{
		ExternalMcps: []ExternalMcp{{ID: "mcp-nil"}},
		Services:     []ServiceConfig{{ID: "svc-nil"}},
		Projects:     []Project{{ID: "proj-nil"}},
	}

	clone := orig.Clone()

	nilChecks := []struct {
		name string
		nil  bool
	}{
		{"ExternalMcps[0].Args", clone.ExternalMcps[0].Args == nil},
		{"ExternalMcps[0].Env", clone.ExternalMcps[0].Env == nil},
		{"ExternalMcps[0].TccServices", clone.ExternalMcps[0].TccServices == nil},
		{"Services[0].Args", clone.Services[0].Args == nil},
		{"Services[0].Env", clone.Services[0].Env == nil},
		{"Projects[0].AllowedMcpIDs", clone.Projects[0].AllowedMcpIDs == nil},
		{"Projects[0].AllowedModels", clone.Projects[0].AllowedModels == nil},
		{"Projects[0].DisabledTools", clone.Projects[0].DisabledTools == nil},
		{"Projects[0].Context", clone.Projects[0].Context == nil},
		{"Projects[0].AllowedTools", clone.Projects[0].AllowedTools == nil},
		{"Projects[0].Access", clone.Projects[0].Access == nil},
		{"Projects[0].AllowExternal", clone.Projects[0].AllowExternal == nil},
		{"Projects[0].SessionFolders", clone.Projects[0].SessionFolders == nil},
		{"Enrolments", clone.Enrolments == nil},
		{"Audit", clone.Audit == nil},
		{"Remote", clone.Remote == nil},
		{"APICredentials", clone.APICredentials == nil},
		{"LoginBootstrap", clone.LoginBootstrap == nil},
		{"Passkeys", clone.Passkeys == nil},
	}
	for _, c := range nilChecks {
		if !c.nil {
			t.Errorf("Clone: %s should stay nil, got a non-nil value", c.name)
		}
	}

	// An explicit empty (non-nil) collection must stay non-nil too — the
	// other half of the same distinction.
	origEmpty := &Settings{
		Projects: []Project{{
			ID:            "proj-empty",
			AllowedMcpIDs: []string{},
			Access:        map[string]string{},
		}},
	}
	cloneEmpty := origEmpty.Clone()
	if cloneEmpty.Projects[0].AllowedMcpIDs == nil {
		t.Error("Clone: an empty non-nil slice became nil")
	}
	if cloneEmpty.Projects[0].Access == nil {
		t.Error("Clone: an empty non-nil map became nil")
	}
}

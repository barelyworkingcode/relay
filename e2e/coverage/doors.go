package coverage

import (
	"encoding/json"
	"fmt"
)

// Door is one entry of `relay doors --json`, schema 1.
type Door struct {
	Kind       string   `json:"kind"`
	Name       string   `json:"name"`
	Credential string   `json:"credential"`
	OwnerGated bool     `json:"owner_gated"`
	Gates      []string `json:"gates,omitempty"`
	Calls      []string `json:"calls,omitempty"`
}

// Ref is the door as a feature row names it: kind, a colon and the exact name.
func (d Door) Ref() string { return d.Kind + ":" + d.Name }

// DoorsDoc is the document `relay doors --json` prints.
type DoorsDoc struct {
	Schema   int    `json:"schema"`
	Headless bool   `json:"headless"`
	Doors    []Door `json:"doors"`
}

// ParseDoors decodes `relay doors --json` output and refuses any schema but 1.
func ParseDoors(data []byte) (DoorsDoc, error) {
	var d DoorsDoc
	if err := json.Unmarshal(data, &d); err != nil {
		return DoorsDoc{}, fmt.Errorf("decoding the doors document: %w", err)
	}
	if d.Schema != 1 {
		return DoorsDoc{}, fmt.Errorf("doors document schema is %d, expected 1", d.Schema)
	}
	return d, nil
}

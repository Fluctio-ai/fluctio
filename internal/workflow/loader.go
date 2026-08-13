package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Parse decodes a YAML definition. defID overrides the in-file id when non-empty
// (LoadFile derives it from the filename). Parse performs no validation — Seam A
// consumes already-validated definitions; Seam B (ticket 02) validates + round-trips.
func Parse(defID string, data []byte) (*Definition, error) {
	var d Definition
	if err := yaml.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("parse workflow yaml: %w", err)
	}
	normalizeEmptyMaps(&d)
	if defID != "" {
		d.ID = defID
	}
	return &d, nil
}

// normalizeEmptyMaps nils out empty map fields so a definition round-trips
// cleanly: yaml.v3 omits empty maps on marshal, so an explicit `output: {}`
// would otherwise parse to a non-nil empty map, marshal to nothing, re-parse
// to nil, and break semantic equality (spec decision 8 round-trip hard
// constraint). nil and empty map are semantically identical at runtime
// (resolveInput treats len==0 as nothing-to-resolve).
func normalizeEmptyMaps(d *Definition) {
	if len(d.Input.Schema) == 0 {
		d.Input.Schema = nil
	}
	for i := range d.Nodes {
		if len(d.Nodes[i].Input) == 0 {
			d.Nodes[i].Input = nil
		}
		if len(d.Nodes[i].Output) == 0 {
			d.Nodes[i].Output = nil
		}
	}
}

// LoadFile reads and parses one workflows/*.yaml file. The id is the filename
// stem (spec decision 8 — the definition's source of truth is the file itself).
func LoadFile(path string) (*Definition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read workflow %s: %w", path, err)
	}
	id := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return Parse(id, data)
}

// LoadDir loads every *.yaml file directly under dir, keyed by definition id.
func LoadDir(dir string) (map[string]*Definition, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read workflow dir %s: %w", dir, err)
	}
	out := make(map[string]*Definition, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		d, err := LoadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out[d.ID] = d
	}
	return out, nil
}

// Marshal serialises a Definition back to YAML. With Parse it gives the
// round-trip guarantee (spec decision 8): parse(marshal(parse(x))) is
// semantically equal to parse(x). Optional fields use ,omitempty so nil/zero
// values collapse to the same shape on both sides.
func Marshal(def *Definition) ([]byte, error) {
	return yaml.Marshal(def)
}

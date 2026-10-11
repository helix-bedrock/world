// Ported from df-mc/worldupgrader (MIT):
// https://github.com/df-mc/worldupgrader/blob/634e943988fa0e1b7985974863cba2463fdd431f/blockupgrader/upgrader.go
// https://github.com/df-mc/worldupgrader/blob/634e943988fa0e1b7985974863cba2463fdd431f/blockupgrader/registry.go
//
// Changed for Helix: schemas live in an Upgrader value instead of package
// state loaded in init, loading returns errors instead of panicking, and
// Upgrade does not modify the caller's property map.

// Package blockupgrader upgrades block states saved by older versions of
// Minecraft: Bedrock Edition to the current format, using pmmp's
// BedrockBlockUpgradeSchema files (CC0-1.0), which are embedded.
package blockupgrader

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"strings"
)

//go:embed schemas/*.json
var schemasFS embed.FS

// BlockState is a block's name, properties and the version that saved it.
type BlockState struct {
	Name       string         `nbt:"name"`
	Properties map[string]any `nbt:"states"`
	Version    int32          `nbt:"version"`
}

// Upgrader holds an ordered list of upgrade schemas.
//
// Register must not be called concurrently with anything else. Upgrade is
// safe to call from any goroutine once registration is finished.
type Upgrader struct {
	schemas []schema
}

// New returns an Upgrader with every embedded schema registered.
func New() (*Upgrader, error) {
	u := &Upgrader{}
	files, err := fs.ReadDir(schemasFS, "schemas")
	if err != nil {
		return nil, fmt.Errorf("blockupgrader: read embedded schemas: %w", err)
	}
	// ReadDir sorts by name, and the names start with the schema order.
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		file, err := schemasFS.Open("schemas/" + f.Name())
		if err != nil {
			return nil, fmt.Errorf("blockupgrader: open schema %s: %w", f.Name(), err)
		}
		err = u.Register(file)
		_ = file.Close()
		if err != nil {
			return nil, fmt.Errorf("blockupgrader: schema %s: %w", f.Name(), err)
		}
	}
	return u, nil
}

// Register decodes a schema in BedrockBlockUpgradeSchema's JSON format and
// adds it after the schemas already registered. Schemas must be registered
// oldest first.
func (u *Upgrader) Register(r io.Reader) error {
	var m schemaModel
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return fmt.Errorf("decode schema: %w", err)
	}
	s, err := parseSchemaModel(m)
	if err != nil {
		return err
	}
	u.schemas = append(u.schemas, s)
	return nil
}

// Upgrade returns state upgraded through every newer schema. A state that is
// already current is returned unchanged.
func (u *Upgrader) Upgrade(state BlockState) BlockState {
	// Schemas edit properties in place; never touch the caller's map.
	state.Properties = maps.Clone(state.Properties)
	if state.Properties == nil {
		state.Properties = map[string]any{}
	}
	version := state.Version
	for _, s := range u.schemas {
		resVersion := s.id
		if version > resVersion {
			continue
		}

		oldName := state.Name
		oldProperties := state.Properties
		if remaps, ok := s.remappedStates[oldName]; ok {
			var nextSchema bool
			for _, remap := range remaps {
				if len(remap.oldProperties) > len(oldProperties) {
					continue
				}
				matches := true
				for k, v := range remap.oldProperties {
					if oldValue, ok := oldProperties[k]; !ok || oldValue != v {
						matches = false
						break
					}
				}
				if !matches {
					continue
				}
				newProperties := maps.Clone(remap.newProperties)
				for _, k := range remap.copiedProperties {
					if v, ok := oldProperties[k]; ok {
						newProperties[k] = v
					}
				}

				newName := remap.newName
				if newName == "" {
					newName, _ = s.applyPropertyFlattened(remap.newFlattenedName, oldName, oldProperties)
				}

				state, nextSchema = BlockState{
					Name:       newName,
					Properties: newProperties,
					Version:    resVersion,
				}, true
				break
			}
			if nextSchema {
				continue
			}
		}

		properties := state.Properties
		name, nameRenamed := s.renamedIDs[oldName]
		info, flattened := s.flattenedProperties[oldName]
		// A block cannot be both renamed and flattened; parseSchemaModel
		// rejects such schemas.
		if flattened {
			name, properties = s.applyPropertyFlattened(info, oldName, properties)
		} else if !nameRenamed {
			name = oldName
		}

		propertyAdded := s.applyPropertyAdded(oldName, properties)
		propertyRemoved := s.applyPropertyRemoved(oldName, properties)
		propertyRenamedOrValueChanged := s.applyPropertyRenamedOrValueChanged(oldName, properties)
		propertyValueChanged := s.applyPropertyValueChanged(oldName, properties)

		if nameRenamed || flattened || propertyAdded || propertyRemoved || propertyRenamedOrValueChanged || propertyValueChanged {
			state = BlockState{
				Name:       name,
				Properties: properties,
				Version:    resVersion,
			}
		}
	}
	return state
}

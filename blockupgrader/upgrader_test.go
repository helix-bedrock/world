// Test cases ported from df-mc/worldupgrader (MIT):
// https://github.com/df-mc/worldupgrader/blob/634e943988fa0e1b7985974863cba2463fdd431f/upgrader_test.go

package blockupgrader

import (
	"maps"
	"strings"
	"testing"
)

func newUpgrader(t *testing.T) *Upgrader {
	t.Helper()
	u, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUpgrade(t *testing.T) {
	u := newUpgrader(t)
	tests := []struct {
		name string
		in   BlockState
		want BlockState
	}{
		{
			name: "flattened",
			in:   BlockState{Name: "minecraft:wool", Properties: map[string]any{"color": "red"}, Version: 17825806},
			want: BlockState{Name: "minecraft:red_wool", Properties: map[string]any{}, Version: 18040335},
		},
		{
			name: "flattened value remap",
			in:   BlockState{Name: "minecraft:wool", Properties: map[string]any{"color": "silver"}, Version: 17825806},
			want: BlockState{Name: "minecraft:light_gray_wool", Properties: map[string]any{}, Version: 18040335},
		},
		{
			name: "copied properties",
			in: BlockState{Name: "minecraft:log", Properties: map[string]any{
				"old_log_type": "spruce", "pillar_axis": "y",
			}, Version: 17825806},
			want: BlockState{Name: "minecraft:spruce_log", Properties: map[string]any{"pillar_axis": "y"}, Version: 18042891},
		},
		{
			name: "remapped property values",
			in: BlockState{Name: "minecraft:big_dripleaf", Properties: map[string]any{
				"big_dripleaf_head": byte(1), "big_dripleaf_tilt": "none", "direction": int32(2),
			}, Version: 18090528},
			want: BlockState{Name: "minecraft:big_dripleaf", Properties: map[string]any{
				"big_dripleaf_head": byte(1), "big_dripleaf_tilt": "none", "minecraft:cardinal_direction": "north",
			}, Version: 18095666},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := maps.Clone(tc.in.Properties)
			got := u.Upgrade(tc.in)
			if got.Name != tc.want.Name || got.Version != tc.want.Version || !maps.Equal(got.Properties, tc.want.Properties) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if !maps.Equal(tc.in.Properties, before) {
				t.Fatalf("Upgrade modified the input properties: %v", tc.in.Properties)
			}
		})
	}
}

func TestUpgradeCurrentUnchanged(t *testing.T) {
	u := newUpgrader(t)
	in := BlockState{Name: "minecraft:stone", Version: 1 << 30}
	if got := u.Upgrade(in); got.Name != in.Name || got.Version != in.Version || len(got.Properties) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestRegisterRejectsBadSchemas(t *testing.T) {
	bad := map[string]string{
		"not json":       `{`,
		"rename+flatten": `{"renamedIds":{"a":"b"},"flattenedProperties":{"a":{"flattenedProperty":"x"}}}`,
		"flatten type":   `{"flattenedProperties":{"a":{"flattenedProperty":"x","flattenedPropertyType":"long"}}}`,
	}
	for name, s := range bad {
		if err := new(Upgrader).Register(strings.NewReader(s)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

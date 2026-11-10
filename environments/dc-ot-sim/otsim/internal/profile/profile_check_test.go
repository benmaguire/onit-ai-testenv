package profile

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestModbusMapCheck(t *testing.T) {
	for _, c := range []struct{ yaml, want string }{
		{"input: [{addr: 0, point: a, type: float32}, {addr: 1, point: b}]", "overlaps"},
		{"input: [{addr: 0, point: a, type: double}]", "unknown type"},
		{"holding: [{addr: 0, type: string}]", "len >= 1"},
		{"holding: [{addr: 3}]", "needs point"},
		{"coils: [{addr: 1, point: a}, {addr: 1, point: b}]", "duplicates"},
		{"input: [{addr: 65535, point: a, type: uint32}]", "past 65535"},
	} {
		var m ModbusMap
		if err := yaml.UnmarshalStrict([]byte(c.yaml), &m); err != nil {
			t.Fatal(err)
		}
		if err := m.Check(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.yaml, err, c.want)
		}
	}
	// Every built-in map passes.
	if _, err := Load(""); err != nil {
		t.Fatal(err)
	}
}

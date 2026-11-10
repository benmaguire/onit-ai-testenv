package telegraf

import "testing"

func TestTOMLFloat(t *testing.T) {
	for in, want := range map[float64]string{1: "1.0", 0.1: "0.1", 10: "10.0", 0.01: "0.01", 1e-05: "1e-05"} {
		if got := tomlFloat(in); got != want {
			t.Errorf("tomlFloat(%v) = %s, want %s", in, got, want)
		}
	}
}

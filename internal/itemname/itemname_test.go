package itemname

import "testing"

func TestValid(t *testing.T) {
	for name, want := range map[string]bool{
		"impl": true, "a.b-c_d": true, "9x": true,
		"": false, ".hidden": false, "-x": false, "a/b": false, "a b": false,
	} {
		if got := Valid(name); got != want {
			t.Errorf("Valid(%q) = %v, want %v", name, got, want)
		}
	}
}

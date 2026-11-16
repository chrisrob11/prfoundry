package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b.json")
	for _, want := range []string{"first", "second"} {
		if err := Write(path, []byte(want)); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(path); err != nil || string(b) != want {
			t.Fatalf("read %q, %v; want %q", b, err, want)
		}
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, %v; want 0644", fi.Mode().Perm(), err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("left temporary files behind: %v", entries)
	}
}

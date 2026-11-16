package textfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteMakesTheFolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "f.txt")
	if err := Write(path, "hi"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "hi" {
		t.Fatalf("read back %q, %v", b, err)
	}
}

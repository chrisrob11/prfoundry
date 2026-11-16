// Package textfile writes a text file in place, making its folder first.
// For a file a reader may see mid-write, use atomicfile instead.
package textfile

import (
	"os"
	"path/filepath"
)

// Write writes s to path, creating parent directories.
func Write(path, s string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(s), 0o644)
}

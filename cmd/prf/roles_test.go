package main

import (
	"path/filepath"
	"testing"

	"github.com/chrisrob11/prfoundry/internal/textfile"
)

func writeRole(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := textfile.Write(filepath.Join(dir, name+".md"), text); err != nil {
		t.Fatal(err)
	}
}

package mise

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/chrisrob11/prfoundry/internal/textfile"
)

// fakeMise puts a mise on PATH that records "pwd|args|MISE_TRUSTED_CONFIG_PATHS"
// in the returned file.
func fakeMise(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	if err := textfile.Write(filepath.Join(bin, "mise"), "#!/bin/sh\necho \"$(pwd)|$*|$MISE_TRUSTED_CONFIG_PATHS\" >> "+log+"\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(bin, "mise"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

func TestInstallTools(t *testing.T) {
	log := fakeMise(t)
	withConfig, without := t.TempDir(), t.TempDir()
	if err := textfile.Write(filepath.Join(withConfig, ".config", "mise.toml"), ""); err != nil {
		t.Fatal(err)
	}
	var logged []string
	logf := func(format string, _ ...any) { logged = append(logged, format) }

	Install(without, Env("/work"), logf)
	if _, err := os.Stat(log); err == nil {
		t.Fatal("mise ran in a repo with no mise config")
	}

	Install(withConfig, Env("/work"), logf)
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal("mise did not run:", err)
	}
	fields := strings.Split(strings.TrimSpace(string(b)), "|")
	if len(fields) != 3 || !sameDir(fields[0], withConfig) || fields[1] != "install --yes" || !strings.HasPrefix(fields[2], "/work") {
		t.Errorf("mise call = %q, want install --yes in %s trusting /work", b, withConfig)
	}
	if !slices.Contains(logged, "installing tools with mise") {
		t.Errorf("logged %q, want the install announced", logged)
	}
}

func TestMiseEnv(t *testing.T) {
	t.Setenv("MISE_TRUSTED_CONFIG_PATHS", "/mine")
	if env := Env(""); slices.Contains(env, "MISE_TRUSTED_CONFIG_PATHS=/work") {
		t.Error("empty trusted path changed the environment")
	}
	env := Env("/work")
	if want := "MISE_TRUSTED_CONFIG_PATHS=/work" + string(os.PathListSeparator) + "/mine"; env[len(env)-1] != want {
		t.Errorf("last env entry = %q, want %q", env[len(env)-1], want)
	}
}

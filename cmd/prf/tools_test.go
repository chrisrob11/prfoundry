package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeMise puts a mise on PATH that records "pwd|args|MISE_TRUSTED_CONFIG_PATHS"
// in the returned file.
func fakeMise(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	writeFileT(t, filepath.Join(bin, "mise"), "#!/bin/sh\necho \"$(pwd)|$*|$MISE_TRUSTED_CONFIG_PATHS\" >> "+log+"\n")
	if err := os.Chmod(filepath.Join(bin, "mise"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestInstallTools(t *testing.T) {
	log := fakeMise(t)
	withConfig, without := t.TempDir(), t.TempDir()
	writeFileT(t, filepath.Join(withConfig, ".config", "mise.toml"), "")

	installTools(without, miseEnv("/work"))
	if _, err := os.Stat(log); err == nil {
		t.Fatal("mise ran in a repo with no mise config")
	}

	installTools(withConfig, miseEnv("/work"))
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal("mise did not run:", err)
	}
	fields := strings.Split(strings.TrimSpace(string(b)), "|")
	if len(fields) != 3 || !sameDir(fields[0], withConfig) || fields[1] != "install --yes" || !strings.HasPrefix(fields[2], "/work") {
		t.Errorf("mise call = %q, want install --yes in %s trusting /work", b, withConfig)
	}
}

func TestMiseEnv(t *testing.T) {
	t.Setenv("MISE_TRUSTED_CONFIG_PATHS", "/mine")
	if env := miseEnv(""); slices.Contains(env, "MISE_TRUSTED_CONFIG_PATHS=/work") {
		t.Error("empty trusted path changed the environment")
	}
	env := miseEnv("/work")
	if want := "MISE_TRUSTED_CONFIG_PATHS=/work" + string(os.PathListSeparator) + "/mine"; env[len(env)-1] != want {
		t.Errorf("last env entry = %q, want %q", env[len(env)-1], want)
	}
}

func TestGoCacheDirsWithoutGo(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no go here
	t.Setenv("HOME", "/h")
	t.Setenv("XDG_CACHE_HOME", "/h/.cache") // used by os.UserCacheDir on Linux
	for _, k := range []string{"GOPATH", "GOCACHE"} {
		t.Setenv(k, "")
	}
	t.Setenv("GOMODCACHE", "/custom/mod")

	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/custom/mod", filepath.Join(cacheDir, "go-build"), "/h/go/pkg/sumdb"}
	if got := goCacheDirs(); !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

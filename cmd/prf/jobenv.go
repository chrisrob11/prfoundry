package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// jobPlaceholder in a job_env value or a job_seed target is the job folder.
const jobPlaceholder = "{{job}}"

// jobEnvFile is written in the job folder, the clone's parent, so mise finds
// it from anywhere in the clone.
const jobEnvFile = "mise.toml"

// jobSeed is a copy made into a fresh job folder before the first step.
type jobSeed struct {
	From string `json:"from"`
	To   string `json:"to"`
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// checkJobEnv rejects job_env names mise can't set and job_seed targets
// outside the job folder.
func checkJobEnv(r repoConfig) error {
	for name := range r.JobEnv {
		if !envName.MatchString(name) {
			return fmt.Errorf("job_env: %q is not an environment variable name", name)
		}
	}
	for _, s := range r.JobSeed {
		if s.From == "" {
			return fmt.Errorf("job_seed: %q has no from", s.To)
		}
		if !underJob(expandJob(s.To, "/job"), "/job") {
			return fmt.Errorf("job_seed: to %q must be under %s", s.To, jobPlaceholder)
		}
	}
	return nil
}

func expandJob(s, jobDir string) string {
	return strings.ReplaceAll(s, jobPlaceholder, jobDir)
}

// underJob reports whether path is inside jobDir, not jobDir itself.
func underJob(path, jobDir string) bool {
	return filepath.IsAbs(path) && strings.HasPrefix(filepath.Clean(path), jobDir+string(filepath.Separator))
}

// writeJobEnv writes <job>/mise.toml with an [env] table from job_env, so
// steps, and anyone working in the clone, get the job's own tool caches.
// With no job_env it writes nothing.
func writeJobEnv(jobDir, repo string, env map[string]string) error {
	if len(env) == 0 {
		return nil
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Written by prf from conf.json's job_env for %s; removed with the job.\n[env]\n", repo)
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		fmt.Fprintf(&b, "%s = %s\n", name, tomlString(expandJob(env[name], jobDir)))
	}
	return writeFile(filepath.Join(jobDir, jobEnvFile), b.String())
}

// tomlString quotes s as a TOML basic string, whose escapes are JSON's.
func tomlString(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	return strings.TrimSuffix(b.String(), "\n")
}

// jobWriteDirs are the folders under the job folder that job_env and
// job_seed point at, which steps may write to inside the sandbox.
func jobWriteDirs(jobDir string, r repoConfig) []string {
	var dirs []string
	add := func(s string) {
		if p := filepath.Clean(expandJob(s, jobDir)); underJob(p, jobDir) && !slices.Contains(dirs, p) {
			dirs = append(dirs, p)
		}
	}
	for _, v := range r.JobEnv {
		add(v)
	}
	for _, s := range r.JobSeed {
		add(s.To)
	}
	slices.Sort(dirs)
	return dirs
}

// seedJob makes each job_seed copy, as an APFS clone where cp -c can. A
// missing source or a failed copy is only a warning: the job starts with
// that folder empty instead.
func seedJob(jobDir string, seeds []jobSeed) {
	for _, s := range seeds {
		from, to := expandHome(s.From), filepath.Clean(expandJob(s.To, jobDir))
		if _, err := os.Stat(from); err != nil {
			logf("warning: job_seed: not copying %s: %v", from, err)
			continue
		}
		if _, err := os.Lstat(to); err == nil {
			logf("warning: job_seed: not copying %s: %s already exists", from, to)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			logf("warning: job_seed: %v", err)
			continue
		}
		logf("seeding %s from %s", to, from)
		if exec.Command("cp", "-c", "-R", from, to).Run() == nil {
			continue
		}
		_ = os.RemoveAll(to) // whatever a failed clone left
		if out, err := exec.Command("cp", "-R", from, to).CombinedOutput(); err != nil {
			logf("warning: job_seed: copying %s to %s: %v: %s", from, to, err, bytes.TrimSpace(out))
			_ = os.RemoveAll(to)
		}
	}
}

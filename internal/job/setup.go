package job

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/chrisrob11/prfoundry/internal/config"
	"github.com/chrisrob11/prfoundry/internal/gitx"
	"github.com/chrisrob11/prfoundry/internal/jobenv"
	"github.com/chrisrob11/prfoundry/internal/mise"
	"github.com/chrisrob11/prfoundry/internal/sandbox"
)

// Clone is the repo Run clones, and the setup its job folder gets.
type Clone struct {
	URL       string
	Base      string // the branch to clone; "" for the remote's default
	Branch    string // the branch to create or switch to
	Reference string // a local checkout of the repo to copy objects from, or ""
	JobEnv    map[string]string
	JobSeed   []jobenv.Seed
	MaxDisk   int // refuse to clone when the workdir's disk is this full, in percent; 0 is off
}

// Run clones c into <job>/repo, switches to its branch, sets up the job
// folder and the checkout, and runs the job's phases there. Its error is
// the job's outcome, with prf's exit code.
func (j *Job) Run(c Clone) error {
	j.branch = c.Branch
	if err := j.setJobDir(); err != nil {
		return exitError{"prf: " + err.Error(), ExitSetup}
	}
	j.dir = filepath.Join(j.jobDir, "repo")
	// Clones under workdir are ours, so mise may trust their config files.
	j.env = mise.Env(j.workdir)

	// An earlier job's folder is left alone: its status and state stay as they were.
	if _, err := os.Stat(j.dir); err == nil {
		return exitError{fmt.Sprintf("prf: %s already exists from an earlier job; remove %s to start over, or resume it with prf apply --repo %s --dir %s", j.dir, j.jobDir, j.name, j.dir), ExitSetup}
	}
	if err := checkDisk(j.workdir, c.MaxDisk, j.rt.Statfs); err != nil {
		return exitError{"prf: " + err.Error(), ExitSetup}
	}
	j.setupStage("cloning")
	if err := cloneRepo(c.URL, c.Base, j.branch, j.dir, c.Reference, j.logf); err != nil {
		j.logf("%v", err)
		return j.finish("setup", ExitSetup)
	}
	if err := jobenv.Write(j.jobDir, j.name, c.JobEnv); err != nil {
		j.logf("writing the job's mise.toml: %v", err)
		return j.finish("setup", ExitSetup)
	}
	if len(c.JobSeed) > 0 {
		j.setupStage("seeding the job folder")
		seeds := slices.Clone(c.JobSeed)
		for i := range seeds {
			seeds[i].From = config.ExpandHome(seeds[i].From)
		}
		jobenv.Copy(j.jobDir, seeds, j.logf)
	}
	j.setupStage("installing tools")
	mise.Install(j.dir, j.env, j.logf)
	if err := j.prepareContext(); err != nil {
		j.logf("%v", err)
		return j.finish("setup", ExitSetup)
	}
	return j.runPhases(j.phases)
}

// Apply runs the job's phases in the existing checkout dir, switching to
// branch first (creating it if needed) unless it's "". Its error is the
// job's outcome, with prf's exit code.
func (j *Job) Apply(dir, branch string) error {
	j.dir = dir
	if !gitx.OK(j.dir, "rev-parse", "--is-inside-work-tree") {
		return exitError{"prf: not a git repository: " + j.dir, ExitUsage}
	}
	if branch != "" {
		if err := switchBranch(j.dir, branch, j.logf); err != nil {
			j.logf("%v", err)
			j.branch = branch
			if err := j.setJobDir(); err != nil {
				return exitError{"prf: " + err.Error(), ExitSetup}
			}
			return j.finish("setup", ExitSetup)
		}
	}
	j.branch = cmp.Or(gitx.Output(j.dir, "branch", "--show-current"), "HEAD")
	if err := j.setJobDir(); err != nil {
		return exitError{"prf: " + err.Error(), ExitSetup}
	}
	if gitx.Output(j.dir, "status", "--porcelain") != "" {
		j.logf("warning: %s has uncommitted changes; Claude's commits may include them", j.dir)
	}
	j.env = mise.Env(mise.TrustedDir(j.dir, j.workdir)) // your own checkout keeps mise's trust as you set it
	j.setupStage("installing tools")
	mise.Install(j.dir, j.env, j.logf)
	if err := j.prepareContext(); err != nil {
		j.logf("%v", err)
		return j.finish("setup", ExitSetup)
	}
	return j.runPhases(j.phases)
}

// setJobDir places the job folder under workdir, named after the PR if there
// is one and the branch otherwise, points the log and status file into it
// unless they were given, and finds the Claude settings file.
func (j *Job) setJobDir() error {
	workdir, err := filepath.Abs(j.workdir)
	if err != nil {
		return err
	}
	leaf := strings.ReplaceAll(j.branch, "/", "-")
	if j.pr != "" {
		leaf = strings.TrimSuffix(filepath.Base(j.pr), ".md")
	}
	j.workdir, j.jobDir = workdir, filepath.Join(workdir, j.name, leaf)

	j.out, j.tee = j.outFlag, false
	if j.out == "" {
		j.out, j.tee = filepath.Join(j.jobDir, "claude.log"), true
	}
	j.status = cmp.Or(j.statusFlag, filepath.Join(j.jobDir, "status"))
	return j.useSandbox()
}

// useSandbox picks the Claude settings file, unless the sandbox is off.
func (j *Job) useSandbox() error {
	j.settings = ""
	if !j.sandbox {
		return nil
	}
	var err error
	j.settings, err = sandbox.SettingsPath(j.home, j.name, j.logf)
	return err
}

// checkDisk refuses to start when the filesystem holding path is at least
// limit percent full. A limit of 0 checks nothing.
func checkDisk(path string, limit int, statfs func(string, *syscall.Statfs_t) error) error {
	if limit <= 0 {
		return nil
	}
	used, err := diskUsed(path, statfs)
	if err != nil {
		return fmt.Errorf("checking free space for %s: %w", path, err)
	}
	if used >= float64(limit) {
		return fmt.Errorf("the disk holding %s is %.0f%% full, at or above max_disk_percent (%d%%) in %s; free some space or raise it", path, used, limit, config.FileName)
	}
	return nil
}

// diskUsed is how full the filesystem holding path is, in percent, counted
// as df does: blocks in use over those in use plus those you may still use.
// A path that doesn't exist yet is on its nearest parent's filesystem.
func diskUsed(path string, statfs func(string, *syscall.Statfs_t) error) (float64, error) {
	for {
		var st syscall.Statfs_t
		err := statfs(path, &st)
		if err == nil {
			used := st.Blocks - st.Bfree
			if used+st.Bavail == 0 {
				return 0, nil
			}
			return 100 * float64(used) / float64(used+st.Bavail), nil
		}
		parent := filepath.Dir(path)
		if !errors.Is(err, fs.ErrNotExist) || parent == path {
			return 0, err
		}
		path = parent
	}
}

// cloneRepo clones repo into dir and switches to branch. If reference is a
// local checkout of the same repo, its objects are copied instead of
// downloaded (--reference --dissociate), so the clone is fast and doesn't
// depend on the checkout afterwards.
func cloneRepo(repo, base, branch, dir, reference string, logf func(format string, args ...any)) error {
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("destination already exists: %s", dir)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}

	cloneArgs := []string{"clone", "--quiet"}
	if reference != "" && gitx.OK(reference, "rev-parse", "--git-dir") {
		logf("cloning %s -> %s (objects from %s)", repo, dir, reference)
		cloneArgs = append(cloneArgs, "--reference", reference, "--dissociate")
	} else {
		logf("cloning %s -> %s", repo, dir)
	}
	if base != "" {
		cloneArgs = append(cloneArgs, "--branch", base)
	}
	if err := gitx.Run("", append(cloneArgs, repo, dir)...); err != nil {
		return err
	}
	return switchBranch(dir, branch, logf)
}

// switchBranch switches to branch if it exists locally or on origin, and
// creates it from the current HEAD otherwise.
func switchBranch(dir, branch string, logf func(format string, args ...any)) error {
	if gitx.OK(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) ||
		gitx.OK(dir, "ls-remote", "--exit-code", "--heads", "origin", branch) {
		logf("switching to existing branch %s", branch)
		return gitx.Run(dir, "switch", "--quiet", branch)
	}
	logf("creating branch %s", branch)
	return gitx.Run(dir, "switch", "--quiet", "-c", branch)
}

package main

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/urfave/cli/v3"
)

// The release build sets these with -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
// Left empty, buildVersion reads them from the module's build info instead.
var (
	version string
	commit  string
	date    string
)

// build is what `prf version` and `prf --version` report.
type build struct {
	Version, Commit, Date string
}

func (b build) String() string {
	return fmt.Sprintf("%s (commit %s, built %s)", b.Version, b.Commit, b.Date)
}

// buildVersion is this binary's build: the ldflags values where set, else
// what `go install` and `go build` record.
func buildVersion() build {
	return resolveBuild(build{Version: version, Commit: commit, Date: date}, debug.ReadBuildInfo)
}

// resolveBuild fills each field ldflags left empty from the build info:
// the module version, vcs.revision and vcs.time.
func resolveBuild(b build, read func() (*debug.BuildInfo, bool)) build {
	if info, ok := read(); ok {
		if b.Version == "" && info.Main.Version != "(devel)" {
			b.Version = info.Main.Version
		}
		for _, s := range info.Settings {
			switch {
			case s.Key == "vcs.revision" && b.Commit == "":
				b.Commit = s.Value
			case s.Key == "vcs.time" && b.Date == "":
				b.Date = s.Value
			}
		}
	}
	if b.Version == "" {
		b.Version = "dev"
	}
	if b.Commit == "" {
		b.Commit = "unknown"
	}
	if b.Date == "" {
		b.Date = "unknown"
	}
	return b
}

func versionCmd() *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "print prf's version, commit and build date",
		Action: func(_ context.Context, c *cli.Command) error {
			cli.DefaultPrintVersion(c.Root())
			return nil
		},
	}
}

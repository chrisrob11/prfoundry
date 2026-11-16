package main

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestResolveBuild(t *testing.T) {
	installed := func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Main: debug.Module{Version: "v0.1.0"},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abc123"},
				{Key: "vcs.time", Value: "2026-11-16T00:00:00Z"},
			},
		}, true
	}
	devel := func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true
	}
	none := func() (*debug.BuildInfo, bool) { return nil, false }

	for _, tc := range []struct {
		name    string
		ldflags build
		read    func() (*debug.BuildInfo, bool)
		want    build
	}{
		{"ldflags win over build info", build{"v1.2.3", "fff", "2026-01-01"}, installed, build{"v1.2.3", "fff", "2026-01-01"}},
		{"go install fills from build info", build{}, installed, build{"v0.1.0", "abc123", "2026-11-16T00:00:00Z"}},
		{"a devel build with no vcs", build{}, devel, build{"dev", "unknown", "unknown"}},
		{"no build info", build{}, none, build{"dev", "unknown", "unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveBuild(tc.ldflags, tc.read); got != tc.want {
				t.Errorf("resolveBuild = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestVersionCommandAndFlagAgree(t *testing.T) {
	t.Setenv("PRF_HOME", t.TempDir())
	want := "prf version " + buildVersion().String() + "\n"
	for _, args := range [][]string{{"version"}, {"--version"}} {
		code, out := runPrfIO(t, "", args...)
		if code != 0 || out != want {
			t.Errorf("prf %s = %d %q, want 0 %q", strings.Join(args, " "), code, out, want)
		}
	}
}

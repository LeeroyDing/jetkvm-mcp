package buildinfo

import (
	"regexp"
	"runtime"
	"runtime/debug"
	"testing"
)

func TestVersionIsStableSemanticVersion(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(Version) {
		t.Fatalf("Version = %q, want stable semantic version without v prefix", Version)
	}
}

func TestCurrentMetadata(t *testing.T) {
	originalVersion, originalCommit, originalBuildDate := Version, Commit, BuildDate
	t.Cleanup(func() {
		Version, Commit, BuildDate = originalVersion, originalCommit, originalBuildDate
	})

	fallbackCommit := vcsRevisionFromBuildInfo(debug.ReadBuildInfo())
	if fallbackCommit == "" {
		fallbackCommit = "unknown"
	}

	tests := []struct {
		name          string
		version       string
		commit        string
		buildDate     string
		wantCommit    string
		wantBuildDate string
	}{
		{
			name:          "release metadata is authoritative",
			version:       "1.2.3",
			commit:        "0123456789abcdef",
			buildDate:     "2026-08-22T12:34:56Z",
			wantCommit:    "0123456789abcdef",
			wantBuildDate: "2026-08-22T12:34:56Z",
		},
		{
			name:          "local build uses default version and metadata fallbacks",
			version:       originalVersion,
			wantCommit:    fallbackCommit,
			wantBuildDate: "unknown",
		},
		{
			name:          "missing date does not replace injected commit",
			version:       "2.0.0",
			commit:        "fedcba9876543210",
			wantCommit:    "fedcba9876543210",
			wantBuildDate: "unknown",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			Version, Commit, BuildDate = tc.version, tc.commit, tc.buildDate

			got := Current()
			want := Info{
				Version:   tc.version,
				Commit:    tc.wantCommit,
				BuildDate: tc.wantBuildDate,
				GoVersion: runtime.Version(),
				Platform:  runtime.GOOS + "/" + runtime.GOARCH,
			}
			if got != want {
				t.Fatalf("Current() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestVCSRevisionFromBuildInfo(t *testing.T) {
	tests := []struct {
		name      string
		buildInfo *debug.BuildInfo
		ok        bool
		want      string
	}{
		{
			name: "build info unavailable",
		},
		{
			name: "nil build info marked available",
			ok:   true,
		},
		{
			name:      "no settings",
			buildInfo: &debug.BuildInfo{},
			ok:        true,
		},
		{
			name: "unrelated settings",
			buildInfo: &debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "GOOS", Value: "linux"},
				{Key: "vcs.time", Value: "2026-08-22T12:34:56Z"},
			}},
			ok: true,
		},
		{
			name: "clean revision",
			buildInfo: &debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "0123456789abcdef"},
				{Key: "vcs.modified", Value: "false"},
			}},
			ok:   true,
			want: "0123456789abcdef",
		},
		{
			name: "dirty revision with settings in reverse order",
			buildInfo: &debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.modified", Value: "true"},
				{Key: "vcs.revision", Value: "fedcba9876543210"},
			}},
			ok:   true,
			want: "fedcba9876543210+dirty",
		},
		{
			name: "padded dirty revision is normalized",
			buildInfo: &debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: " \tabcdef\n"},
				{Key: "vcs.modified", Value: "true"},
			}},
			ok:   true,
			want: "abcdef+dirty",
		},
		{
			name: "whitespace-only dirty revision is absent",
			buildInfo: &debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: " \t\n"},
				{Key: "vcs.modified", Value: "true"},
			}},
			ok: true,
		},
		{
			name: "malformed modified value is not dirty",
			buildInfo: &debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abcdef"},
				{Key: "vcs.modified", Value: "TRUE"},
			}},
			ok:   true,
			want: "abcdef",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := vcsRevisionFromBuildInfo(tc.buildInfo, tc.ok); got != tc.want {
				t.Fatalf("vcsRevisionFromBuildInfo() = %q, want %q", got, tc.want)
			}
		})
	}
}

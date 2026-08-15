package buildinfo

import (
	"regexp"
	"strings"
	"testing"
)

func TestVersionIsStableSemanticVersion(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(Version) {
		t.Fatalf("Version = %q, want stable semantic version without v prefix", Version)
	}
}

func TestCurrentIncludesAuthoritativeMetadata(t *testing.T) {
	info := Current()
	if info.Version != Version || info.Commit == "" || info.BuildDate == "" || info.GoVersion == "" || !strings.Contains(info.Platform, "/") {
		t.Fatalf("incomplete Current info: %+v", info)
	}
}

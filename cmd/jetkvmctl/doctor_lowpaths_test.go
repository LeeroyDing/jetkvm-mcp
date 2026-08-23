package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/buildinfo"
	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
	sharedfake "github.com/leeroyding/jetkvm-mcp/internal/testdata/fakedevice"
)

func TestDoctorBundleReportClassifiesBundleState(t *testing.T) {
	t.Run("unknown executable", func(t *testing.T) {
		got := doctorBundleReport("unknown")
		if got.Status != "unknown executable path" || got.InfoPlistPath != "" {
			t.Fatalf("doctorBundleReport(unknown) = %+v", got)
		}
	})

	t.Run("ordinary executable", func(t *testing.T) {
		exe := filepath.Join(t.TempDir(), "bin", "jetkvmctl")
		got := doctorBundleReport(exe)
		if got.Status != "not running from an app bundle" || got.InfoPlistPath != "" {
			t.Fatalf("doctorBundleReport(%q) = %+v", exe, got)
		}
	})

	tests := []struct {
		name       string
		plist      *string
		wantStatus string
		wantVer    string
		wantMatch  *bool
	}{
		{
			name:       "missing Info.plist",
			wantStatus: "app bundle without readable Info.plist",
		},
		{
			name:       "binary Info.plist",
			plist:      stringPointer("bplist00-not-xml"),
			wantStatus: "Info.plist not parseable (not an XML plist)",
		},
		{
			name:       "version matches build",
			plist:      stringPointer(testBundlePlist(buildinfo.Version)),
			wantStatus: "app bundle",
			wantVer:    buildinfo.Version,
			wantMatch:  boolPointer(true),
		},
		{
			name:       "version differs from build",
			plist:      stringPointer(testBundlePlist(buildinfo.Version + "-mismatch")),
			wantStatus: "app bundle",
			wantVer:    buildinfo.Version + "-mismatch",
			wantMatch:  boolPointer(false),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contentsDir := filepath.Join(t.TempDir(), "JetKVM.app", "Contents")
			macOSDir := filepath.Join(contentsDir, "MacOS")
			if err := os.MkdirAll(macOSDir, 0o755); err != nil {
				t.Fatal(err)
			}
			plistPath := filepath.Join(contentsDir, "Info.plist")
			if tt.plist != nil {
				if err := os.WriteFile(plistPath, []byte(*tt.plist), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			got := doctorBundleReport(filepath.Join(macOSDir, "jetkvmctl"))
			if got.Status != tt.wantStatus {
				t.Fatalf("bundle status = %q, want %q; report: %+v", got.Status, tt.wantStatus, got)
			}
			if got.InfoPlistPath != plistPath {
				t.Errorf("Info.plist path = %q, want %q", got.InfoPlistPath, plistPath)
			}
			if got.PlistVersion != tt.wantVer {
				t.Errorf("plist version = %q, want %q", got.PlistVersion, tt.wantVer)
			}
			switch {
			case tt.wantMatch == nil && got.MatchesBuildinfo != nil:
				t.Errorf("matchesBuildinfo = %v, want omitted", *got.MatchesBuildinfo)
			case tt.wantMatch != nil && got.MatchesBuildinfo == nil:
				t.Errorf("matchesBuildinfo omitted, want %v", *tt.wantMatch)
			case tt.wantMatch != nil && *got.MatchesBuildinfo != *tt.wantMatch:
				t.Errorf("matchesBuildinfo = %v, want %v", *got.MatchesBuildinfo, *tt.wantMatch)
			}
			if tt.wantMatch != nil && got.BuildinfoVersionUsed != buildinfo.Version {
				t.Errorf("reported buildinfo version = %q, want %q", got.BuildinfoVersionUsed, buildinfo.Version)
			}
		})
	}
}

func stringPointer(value string) *string { return &value }

func boolPointer(value bool) *bool { return &value }

func testBundlePlist(version string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
  <key>CFBundleShortVersionString</key>
  <string>%s</string>
</dict></plist>`, version)
}

func stubCodesign(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "codesign")
	contents := "#!/bin/sh\nset -eu\n" + script + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatalf("writing fake codesign binary: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	previous := codesignProgram
	codesignProgram = "codesign"
	t.Cleanup(func() { codesignProgram = previous })
}

func TestDoctorCodesignReportClassifiesAndParsesToolResults(t *testing.T) {
	t.Run("unknown executable skips tool", func(t *testing.T) {
		previous := codesignProgram
		codesignProgram = filepath.Join(t.TempDir(), "must-not-exist")
		t.Cleanup(func() { codesignProgram = previous })

		got := doctorCodesignReport("unknown")
		if got != (doctorCodesign{Status: "unknown executable path"}) {
			t.Fatalf("doctorCodesignReport(unknown) = %+v", got)
		}
	})

	t.Run("unsigned", func(t *testing.T) {
		stubCodesign(t, `
[ "$1" = "-d" ]
[ "$2" = "--verbose=2" ]
[ "$3" = "/Applications/JetKVM.app/Contents/MacOS/jetkvmctl" ]
printf '%s\n' 'jetkvmctl: code object is not signed at all' >&2
exit 1`)

		got := doctorCodesignReport("/Applications/JetKVM.app/Contents/MacOS/jetkvmctl")
		if got != (doctorCodesign{Status: "not signed"}) {
			t.Fatalf("unsigned report = %+v", got)
		}
	})

	t.Run("tool failure is bounded", func(t *testing.T) {
		const diagnosticCanary = "CODESIGN-RAW-DIAGNOSTIC-MUST-NOT-SURFACE"
		stubCodesign(t, "printf '%s\\n' '"+diagnosticCanary+"' >&2\nexit 2")

		got := doctorCodesignReport("/tmp/jetkvmctl")
		if got != (doctorCodesign{Status: "unavailable"}) {
			t.Fatalf("failed-tool report = %+v", got)
		}
		if strings.Contains(fmt.Sprintf("%+v", got), diagnosticCanary) {
			t.Fatal("codesign report surfaced unbounded third-party diagnostic output")
		}
	})

	t.Run("signed identity", func(t *testing.T) {
		stubCodesign(t, `
printf '%s\n' \
  'Executable=/tmp/jetkvmctl' \
  ' Authority=Developer ID Application: Example Corp (TEAM123456)' \
  'Authority=Developer ID Certification Authority' \
  'TeamIdentifier=TEAM123456' \
  'CDHash=0123456789abcdef' \
  'Identifier=com.example.jetkvmctl' >&2`)

		got := doctorCodesignReport("/tmp/jetkvmctl")
		want := doctorCodesign{
			Status:         "signed",
			Authority:      "Developer ID Application: Example Corp (TEAM123456)",
			TeamIdentifier: "TEAM123456",
			CDHash:         "0123456789abcdef",
		}
		if got != want {
			t.Fatalf("signed report = %+v, want %+v", got, want)
		}
	})
}

func clearDoctorCredentials(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"JETKVM_AUTH_TOKEN",
		"JETKVM_PASSWORD",
		"JETKVM_PASSWORD_KEYCHAIN_SERVICE",
		"JETKVM_PASSWORD_KEYCHAIN_ACCOUNT",
	} {
		t.Setenv(name, "")
	}
}

func TestDoctorDeviceReportProbesRealSession(t *testing.T) {
	clearDoctorCredentials(t)
	fd := sharedfake.Start(t, sharedfake.Options{})

	report, err := doctorDeviceReport(&commonFlags{url: fd.BaseURL(), timeout: livePathConnectTimeout(t)})
	if err != nil {
		t.Fatalf("doctorDeviceReport: %v", err)
	}
	want := doctorDevice{
		Reachable:       true,
		DeviceID:        "fake-device",
		FirmwareVersion: "0.4.7+dev",
		RPCReachable:    true,
	}
	if report == nil || *report != want {
		t.Fatalf("device report = %+v, want %+v", report, want)
	}
	if status, login, signaling, rpc := fd.Counts(); status != 1 || login != 0 || signaling != 1 || rpc != 1 {
		t.Fatalf("probe request counts = status %d login %d signaling %d rpc %d, want 1/0/1/1",
			status, login, signaling, rpc)
	}
}

func TestDoctorDeviceReportReturnsSafeAuthenticationFailure(t *testing.T) {
	clearDoctorCredentials(t)
	const wrongPassword = "DOCTOR-WRONG-PASSWORD-CANARY"
	t.Setenv("JETKVM_PASSWORD", wrongPassword)
	fd := sharedfake.Start(t, sharedfake.Options{Password: "expected-password"})

	report, err := doctorDeviceReport(&commonFlags{url: fd.BaseURL(), timeout: livePathConnectTimeout(t)})
	if err == nil {
		t.Fatal("doctorDeviceReport accepted an invalid password")
	}
	if kind := jetkvm.ErrorKindOf(err); kind != jetkvm.ErrorKindAuthFailed {
		t.Fatalf("probe error kind = %q, want %q: %v", kind, jetkvm.ErrorKindAuthFailed, err)
	}
	if report == nil || report.Reachable || report.Error != jetkvm.RedactError(err) {
		t.Fatalf("failed authentication report = %+v; error = %v", report, err)
	}
	if strings.Contains(report.Error, wrongPassword) {
		t.Fatalf("device report leaked the rejected password: %q", report.Error)
	}
	if status, login, signaling, rpc := fd.Counts(); status != 1 || login != 1 || signaling != 0 || rpc != 0 {
		t.Fatalf("authentication-failure counts = status %d login %d signaling %d rpc %d, want 1/1/0/0",
			status, login, signaling, rpc)
	}
}

func TestDoctorDeviceReportSurfacesStatusProtocolFailure(t *testing.T) {
	clearDoctorCredentials(t)
	fd := sharedfake.Start(t, sharedfake.Options{RPCResponseMode: sharedfake.RPCOversized})

	report, err := doctorDeviceReport(&commonFlags{url: fd.BaseURL(), timeout: livePathConnectTimeout(t)})
	if err == nil {
		t.Fatal("doctorDeviceReport accepted an oversized status response")
	}
	if kind := jetkvm.ErrorKindOf(err); kind != jetkvm.ErrorKindBadFrame {
		t.Fatalf("status error kind = %q, want %q: %v", kind, jetkvm.ErrorKindBadFrame, err)
	}
	if report == nil || report.Reachable || report.RPCReachable || report.DeviceID != "" || report.FirmwareVersion != "" {
		t.Fatalf("failed status report claimed partial reachability: %+v", report)
	}
	if report.Error != jetkvm.RedactError(err) {
		t.Fatalf("report error = %q, want safe rendered error %q", report.Error, jetkvm.RedactError(err))
	}
	if status, login, signaling, rpc := fd.Counts(); status != 1 || login != 0 || signaling != 1 || rpc != 1 {
		t.Fatalf("status-failure counts = status %d login %d signaling %d rpc %d, want 1/0/1/1",
			status, login, signaling, rpc)
	}
}

func TestDoctorDeviceReportHonorsProbeDeadline(t *testing.T) {
	clearDoctorCredentials(t)
	fd := sharedfake.Start(t, sharedfake.Options{DeviceStatusDelay: time.Second})

	started := time.Now()
	report, err := doctorDeviceReport(&commonFlags{url: fd.BaseURL(), timeout: 100 * time.Millisecond})
	if err == nil {
		t.Fatal("doctorDeviceReport ignored its probe deadline")
	}
	if kind := jetkvm.ErrorKindOf(err); kind != jetkvm.ErrorKindTimeout {
		t.Fatalf("deadline error kind = %q, want %q: %v", kind, jetkvm.ErrorKindTimeout, err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("deadline-bound probe returned after %v, want under 1s", elapsed)
	}
	if report == nil || report.Reachable || report.Error != jetkvm.RedactError(err) {
		t.Fatalf("deadline report = %+v; error = %v", report, err)
	}
	if status, login, signaling, rpc := fd.Counts(); status != 1 || login != 0 || signaling != 0 || rpc != 0 {
		t.Fatalf("deadline counts = status %d login %d signaling %d rpc %d, want 1/0/0/0",
			status, login, signaling, rpc)
	}
}

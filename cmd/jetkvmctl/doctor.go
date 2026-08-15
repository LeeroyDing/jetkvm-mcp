package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/buildinfo"
	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

// runVersion prints the authoritative version metadata. The same
// buildinfo.Version feeds the MCP serverInfo and (via the release build's
// Info.plist) the app bundle, so all three surfaces can never disagree by
// more than a stale build.
func runVersion(args []string) error {
	fs := newCommandFlagSet("version")
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	return printJSON(buildinfo.Current())
}

// codesignProgram is absolute in production so diagnostics cannot be
// redirected by a modified PATH; tests substitute a bare name resolved
// against a hermetic stub directory (same pattern as securityProgram).
var codesignProgram = "/usr/bin/codesign"

// doctorReport is the machine-readable local diagnostic. Every field is
// either static build metadata, a presence/verdict string, or a bounded
// line from a system tool: no secret value, credential, or environment
// variable content ever enters the report.
type doctorReport struct {
	Version     buildinfo.Info    `json:"version"`
	Executable  string            `json:"executable"`
	Bundle      doctorBundle      `json:"bundle"`
	Codesign    doctorCodesign    `json:"codesign"`
	Environment doctorEnvironment `json:"environment"`
	FFmpeg      doctorFFmpeg      `json:"ffmpeg"`
	Keychain    doctorKeychain    `json:"keychain"`
	Device      *doctorDevice     `json:"device,omitempty"`
}

type doctorBundle struct {
	Status               string `json:"status"`
	PlistVersion         string `json:"plistVersion,omitempty"`
	MatchesBuildinfo     *bool  `json:"matchesBuildinfo,omitempty"`
	InfoPlistPath        string `json:"infoPlistPath,omitempty"`
	BuildinfoVersionUsed string `json:"buildinfoVersion,omitempty"`
}

type doctorCodesign struct {
	Status         string `json:"status"`
	Authority      string `json:"authority,omitempty"`
	TeamIdentifier string `json:"teamIdentifier,omitempty"`
	CDHash         string `json:"cdHash,omitempty"`
}

type doctorEnvironment struct {
	URL            string `json:"url"`
	PasswordSource string `json:"passwordSource"`
	AuthToken      string `json:"authToken"`
	KeychainConfig string `json:"keychainConfig"`
}

type doctorFFmpeg struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type doctorKeychain struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type doctorDevice struct {
	Reachable       bool   `json:"reachable"`
	DeviceID        string `json:"deviceId,omitempty"`
	FirmwareVersion string `json:"firmwareVersion,omitempty"`
	RPCReachable    bool   `json:"rpcReachable,omitempty"`
	Error           string `json:"error,omitempty"`
}

// runDoctor produces local, read-only diagnostics. Without --probe-device
// it performs zero network I/O and zero secret access: environment checks
// are presence-only, and the Keychain check is an attribute-only search
// that never requests secret data (so it cannot trigger an unlock or ACL
// prompt). The report always goes to stdout; the command fails only when
// an explicitly requested device probe fails.
func runDoctor(args []string) error {
	fs := newCommandFlagSet("doctor")
	probeDevice := fs.Bool("probe-device", false,
		"opt-in: also connect to the device (--url / $JETKVM_URL) and run one status call")
	var cf commonFlags
	fs.StringVar(&cf.url, "url", os.Getenv("JETKVM_URL"), "device base URL (only used with --probe-device)")
	fs.DurationVar(&cf.timeout, "timeout", 10*time.Second, "device probe timeout (only used with --probe-device)")
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if err := requirePositiveTimeout(&cf); err != nil {
		return err
	}

	report := doctorReport{
		Version:     buildinfo.Current(),
		Executable:  executablePath(),
		Environment: doctorEnvironmentReport(),
		FFmpeg:      doctorFFmpegReport(),
		Keychain:    doctorKeychainReport(),
	}
	report.Bundle = doctorBundleReport(report.Executable)
	report.Codesign = doctorCodesignReport(report.Executable)

	var probeErr error
	if *probeDevice {
		device, err := doctorDeviceReport(&cf)
		report.Device = device
		probeErr = err
	}

	if err := printJSON(report); err != nil {
		return err
	}
	if probeErr != nil {
		return fmt.Errorf("device probe failed: %w", probeErr)
	}
	return nil
}

func executablePath() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	return exe
}

// doctorBundleReport ties the app-bundle story to the single version
// source: when the binary runs from <App>.app/Contents/MacOS, the bundle's
// Info.plist CFBundleShortVersionString must match buildinfo.Version.
func doctorBundleReport(exe string) doctorBundle {
	if exe == "unknown" {
		return doctorBundle{Status: "unknown executable path"}
	}
	macosDir := filepath.Dir(exe)
	contentsDir := filepath.Dir(macosDir)
	if filepath.Base(macosDir) != "MacOS" || filepath.Base(contentsDir) != "Contents" {
		return doctorBundle{Status: "not running from an app bundle"}
	}
	plistPath := filepath.Join(contentsDir, "Info.plist")
	data, err := os.ReadFile(plistPath)
	if err != nil {
		return doctorBundle{Status: "app bundle without readable Info.plist", InfoPlistPath: plistPath}
	}
	version, err := parsePlistStringValue(data, "CFBundleShortVersionString")
	if err != nil {
		return doctorBundle{Status: "Info.plist not parseable (" + err.Error() + ")", InfoPlistPath: plistPath}
	}
	matches := version == buildinfo.Version
	return doctorBundle{
		Status:               "app bundle",
		PlistVersion:         version,
		MatchesBuildinfo:     &matches,
		InfoPlistPath:        plistPath,
		BuildinfoVersionUsed: buildinfo.Version,
	}
}

// parsePlistStringValue extracts <key>name</key><string>value</string> from
// an XML plist. This deliberately avoids spawning any plist tool: reading
// the file is the only I/O. Binary plists report as not parseable.
func parsePlistStringValue(data []byte, key string) (string, error) {
	if !bytes.Contains(data, []byte("<?xml")) && !bytes.Contains(data, []byte("<plist")) {
		return "", errors.New("not an XML plist")
	}
	re := regexp.MustCompile(`<key>` + regexp.QuoteMeta(key) + `</key>\s*<string>([^<]*)</string>`)
	m := re.FindSubmatch(data)
	if m == nil {
		return "", fmt.Errorf("key %s not found", key)
	}
	return string(m[1]), nil
}

// doctorCodesignReport reports the binary's signing identity using
// codesign's read-only display mode. It parses only the bounded
// Authority/TeamIdentifier/CDHash lines; everything else is discarded.
func doctorCodesignReport(exe string) doctorCodesign {
	if exe == "unknown" {
		return doctorCodesign{Status: "unknown executable path"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, codesignProgram, "-d", "--verbose=2", exe)
	// codesign writes its display output to stderr.
	cmd.Stdout = io.Discard
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		if strings.Contains(out.String(), "not signed") {
			return doctorCodesign{Status: "not signed"}
		}
		return doctorCodesign{Status: "unavailable"}
	}
	report := doctorCodesign{Status: "signed"}
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case report.Authority == "" && strings.HasPrefix(line, "Authority="):
			report.Authority = strings.TrimPrefix(line, "Authority=")
		case strings.HasPrefix(line, "TeamIdentifier="):
			report.TeamIdentifier = strings.TrimPrefix(line, "TeamIdentifier=")
		case strings.HasPrefix(line, "CDHash="):
			report.CDHash = strings.TrimPrefix(line, "CDHash=")
		}
	}
	return report
}

// doctorEnvironmentReport reports configuration presence and shape only.
// No environment variable VALUE is ever included: the URL is reported as a
// fixed verdict from CanonicalBaseURL, and credentials as source names.
func doctorEnvironmentReport() doctorEnvironment {
	report := doctorEnvironment{}

	switch url := strings.TrimSpace(os.Getenv("JETKVM_URL")); {
	case url == "":
		report.URL = "unset"
	default:
		if _, err := jetkvm.CanonicalBaseURL(url); err != nil {
			// The validation error is a fixed message that never echoes
			// the raw URL.
			report.URL = "set (invalid: " + err.Error() + ")"
		} else {
			report.URL = "set (valid)"
		}
	}

	if os.Getenv("JETKVM_AUTH_TOKEN") != "" {
		report.AuthToken = "set (password resolution skipped)"
	} else {
		report.AuthToken = "unset"
	}

	service := strings.TrimSpace(os.Getenv("JETKVM_PASSWORD_KEYCHAIN_SERVICE"))
	account := strings.TrimSpace(os.Getenv("JETKVM_PASSWORD_KEYCHAIN_ACCOUNT"))
	switch {
	case service == "" && account == "":
		report.KeychainConfig = "unset"
	case service == "" || account == "":
		report.KeychainConfig = "misconfigured: JETKVM_PASSWORD_KEYCHAIN_SERVICE and JETKVM_PASSWORD_KEYCHAIN_ACCOUNT must both be set"
	default:
		report.KeychainConfig = "set"
	}

	hasPassword := os.Getenv("JETKVM_PASSWORD") != ""
	switch {
	case os.Getenv("JETKVM_AUTH_TOKEN") != "":
		report.PasswordSource = "auth token"
	case service != "" && account != "" && hasPassword:
		report.PasswordSource = "keychain (JETKVM_PASSWORD fallback present)"
	case service != "" && account != "":
		report.PasswordSource = "keychain"
	case hasPassword:
		report.PasswordSource = "JETKVM_PASSWORD"
	default:
		report.PasswordSource = "none"
	}
	return report
}

func doctorFFmpegReport() doctorFFmpeg {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := (&jetkvm.FFmpegDecoder{}).CheckAvailable(ctx); err != nil {
		return doctorFFmpeg{Status: "unavailable", Detail: jetkvm.RedactError(err)}
	}
	return doctorFFmpeg{Status: "available"}
}

// doctorKeychainReport checks item PRESENCE only, with a mechanism that
// cannot prompt: `security find-generic-password` WITHOUT -w or -g is an
// attribute-only search that never requests secret data, so it cannot
// trigger a Keychain unlock or ACL dialog. The item's secret value is
// never read, and no output from the tool is surfaced.
func doctorKeychainReport() doctorKeychain {
	service := strings.TrimSpace(os.Getenv("JETKVM_PASSWORD_KEYCHAIN_SERVICE"))
	account := strings.TrimSpace(os.Getenv("JETKVM_PASSWORD_KEYCHAIN_ACCOUNT"))
	if service == "" || account == "" {
		return doctorKeychain{Status: "not configured"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, securityProgram,
		"find-generic-password",
		"-s", service,
		"-a", account,
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if err == nil {
		return doctorKeychain{Status: "present", Detail: "attribute-only check; secret value not read"}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 44 {
		// security exits 44 (errSecItemNotFound) when no item matches.
		return doctorKeychain{Status: "missing"}
	}
	return doctorKeychain{Status: "not checkable", Detail: "attribute search failed; not retrying with any mechanism that could prompt"}
}

// doctorDeviceReport is the only part of doctor that touches the network,
// and it runs only behind the explicit --probe-device flag. It performs
// the same connect + one status call the `status` command would.
func doctorDeviceReport(cf *commonFlags) (*doctorDevice, error) {
	ctx, cancel := commandContext(cf.timeout)
	defer cancel()

	client, err := connectFromFlags(ctx, cf, false)
	if err != nil {
		return &doctorDevice{Reachable: false, Error: jetkvm.RedactError(err)}, err
	}
	defer client.Close(ctx)

	status, err := client.Status(ctx)
	if err != nil {
		return &doctorDevice{Reachable: false, Error: jetkvm.RedactError(err)}, err
	}
	return &doctorDevice{
		Reachable:       true,
		DeviceID:        status.DeviceID,
		FirmwareVersion: status.FirmwareVersion,
		RPCReachable:    status.RPCReachable,
	}, nil
}

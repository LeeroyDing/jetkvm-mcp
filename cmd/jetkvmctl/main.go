// Command jetkvmctl is a browser-free CLI for a JetKVM device: status
// checks, screenshots, an MCP stdio server, and (opt-in, gated) keyboard
// and mouse control.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
	"github.com/leeroyding/jetkvm-mcp/internal/mcpserver"
)

func main() {
	exitCode, err := runCLI(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, formatCLIError(err))
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

func runCLI(args []string) (int, error) {
	if len(args) < 1 {
		printUsage(os.Stderr)
		return 2, nil
	}

	var err error
	switch args[0] {
	case "version", "--version", "-version":
		err = runVersion(args[1:])
	case "doctor":
		err = runDoctor(args[1:])
	case "status":
		err = runStatus(args[1:])
	case "screenshot":
		err = runScreenshot(args[1:])
	case "serve":
		err = runServe(args[1:])
	case "keypress":
		err = runKeypress(args[1:])
	case "mouse-move":
		err = runMouseMove(args[1:])
	case "release-all":
		err = runReleaseAll(args[1:])
	case "-h", "--help", "help":
		printUsage(os.Stdout)
		return 0, nil
	default:
		printUsage(os.Stderr)
		// Do not reflect an arbitrary command token. It may itself be a URL
		// containing userinfo/query credentials, and all the useful recovery
		// information is already in the static usage text above.
		return 2, fmt.Errorf("unknown command")
	}

	if err != nil {
		return 1, err
	}
	return 0, nil
}

// formatCLIError is the single rendering boundary for every error this
// process prints. RedactError scrubs URLs (userinfo, query strings),
// key/value credential pairs, and long opaque tokens, so a wrapped error
// from any dependency cannot smuggle credential material onto stderr.
func formatCLIError(err error) string {
	return "jetkvmctl: " + jetkvm.RedactError(err)
}

func printUsage(w *os.File) {
	fmt.Fprint(w, `jetkvmctl - browser-free JetKVM controller

Usage:
  jetkvmctl --version
  jetkvmctl doctor       [--probe-device [--url URL] [--timeout DURATION]]
  jetkvmctl status       [--url URL]
  jetkvmctl screenshot   [--url URL] --output PATH [--diagnostics]
  jetkvmctl serve        [--url URL] [--allow-control]
  jetkvmctl keypress     [--url URL] --allow-control --key CODE [--modifier N]
  jetkvmctl mouse-move   [--url URL] --allow-control --x N --y N [--buttons N]
  jetkvmctl release-all  [--url URL] --allow-control

Connection:
  --url URL           Device base URL (required, or set $JETKVM_URL)
  --timeout DURATION  Overall one-shot operation timeout; serve uses it per
                      connection/tool operation (default 10s)

Credentials (never pass these as flags/arguments):
  JETKVM_PASSWORD_KEYCHAIN_SERVICE / JETKVM_PASSWORD_KEYCHAIN_ACCOUNT
                      macOS Keychain generic-password item to read first
  JETKVM_PASSWORD     fallback env var: log in with this password
  JETKVM_AUTH_TOKEN   env var: use this already-valid session cookie directly
  --password-stdin    read a password from stdin (first line) instead.
                      Rejected by 'serve': the MCP protocol owns stdin.

Local diagnostics (no device, no network, no secret values):
  doctor              Report version/build provenance, app-bundle and code-
                      signing state, configuration presence (never values),
                      FFmpeg availability, and whether the configured macOS
                      Keychain item exists (attribute-only check that cannot
                      prompt and never reads the secret). Add --probe-device
                      to also connect and run one status call.

Diagnosing a screenshot that never arrives:
  --diagnostics       Print a video-pipeline report to stderr naming the stage
                      the capture stopped at (negotiation, no RTP, no keyframe,
                      decode, ...). Counts, states and codec parameters only -
                      no addresses, credentials, SDP, ICE candidates or pixels.

Control commands (keypress, mouse-move, release-all) require --allow-control
and are otherwise refused. See SECURITY.md for why.

release-all clears every held key and mouse button without moving the cursor.
If neutralization cannot be confirmed on the wire it says so, rather than
reporting a success it cannot back up.

Transport warning: affected JetKVM firmware serves its web API and signaling
WebSocket over plaintext HTTP on the LAN, so the password and session cookie
travel unencrypted and are visible to anyone who can observe that network.
Use only on a trusted, isolated network or over a VPN. This client cannot
upgrade the device's transport. See SECURITY.md.
`)
}

// commonFlags are shared across every subcommand.
type commonFlags struct {
	url           string
	timeout       time.Duration
	allowControl  bool
	passwordStdin bool
}

// newCommandFlagSet builds the flag set every subcommand parses with:
// ContinueOnError so parse failures return instead of exiting mid-test,
// and discarded output so the flag package's raw, argument-reflecting
// diagnostics never reach stderr directly.
func newCommandFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseCommandFlags collapses flag's raw diagnostics to a fixed message and
// rejects positional arguments uniformly. The standard flag errors quote
// invalid values; those values can be credential canaries or URLs and must
// not cross the CLI's public error boundary in the first place.
func parseCommandFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("invalid %s arguments", fs.Name())
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments for %s", fs.Name())
	}
	return nil
}

func addCommonFlags(fs *flag.FlagSet, withControl bool) *commonFlags {
	cf := &commonFlags{}
	// No built-in default: a device address is a property of the operator's
	// network, not of this tool, and baking one in would both leak a
	// deployment detail and risk aiming a command at the wrong host.
	fs.StringVar(&cf.url, "url", os.Getenv("JETKVM_URL"), "device base URL (required, or set $JETKVM_URL)")
	fs.DurationVar(&cf.timeout, "timeout", 10*time.Second, "operation timeout")
	fs.BoolVar(&cf.passwordStdin, "password-stdin", false, "read password from stdin (first line)")
	if withControl {
		fs.BoolVar(&cf.allowControl, "allow-control", false, "opt in to keyboard/mouse control (required for this command)")
	}
	return cf
}

// requireURL validates that a device address was supplied, since there is
// deliberately no default.
func requireURL(cf *commonFlags) error {
	if strings.TrimSpace(cf.url) == "" {
		return fmt.Errorf("--url is required (or set $JETKVM_URL), e.g. --url http://jetkvm.local")
	}
	return nil
}

// canonicalURLFromFlags validates the device URL shape before anything
// else runs: before credential resolution (so a hostile URL never triggers
// a Keychain lookup), before any network dial, and before any error can
// echo it. The returned canonical form is what every downstream consumer
// uses.
func canonicalURLFromFlags(cf *commonFlags) (string, error) {
	if err := requireURL(cf); err != nil {
		return "", err
	}
	return jetkvm.CanonicalBaseURL(cf.url)
}

func requirePositiveTimeout(cf *commonFlags) error {
	if cf.timeout <= 0 {
		return fmt.Errorf("--timeout must be greater than zero")
	}
	return nil
}

// errPasswordStdinWithServe explains why the two stdin consumers cannot
// coexist. `serve` speaks JSON-RPC over stdin/stdout; consuming a line of
// stdin for a password would eat the client's first protocol message (and
// a mis-sequenced client would send that message where a password was
// expected). Failing fast is the only safe resolution - see SECURITY.md.
var errPasswordStdinWithServe = errors.New(
	"--password-stdin cannot be used with 'serve': the MCP protocol owns stdin. " +
		"Configure the macOS Keychain with JETKVM_PASSWORD_KEYCHAIN_SERVICE and " +
		"JETKVM_PASSWORD_KEYCHAIN_ACCOUNT, or pass JETKVM_PASSWORD or JETKVM_AUTH_TOKEN instead")

// securityProgram is absolute in production so credential resolution cannot
// be redirected by a modified PATH. Tests replace it with "security" and put
// a hermetic stub first on PATH; no real Keychain is touched by the suite.
var securityProgram = "/usr/bin/security"

var errMalformedKeychainPassword = errors.New("macOS Keychain returned a malformed password")

// passwordFromKeychain reads one generic-password item with Apple's native
// security tool. -w writes only the secret to stdout; stderr is deliberately
// not included in returned errors because third-party output is not part of
// this program's redaction boundary.
func passwordFromKeychain(service, account string) (string, error) {
	output, err := exec.Command(
		securityProgram,
		"find-generic-password",
		"-s", service,
		"-a", account,
		"-w",
	).Output()
	if err != nil {
		return "", fmt.Errorf("macOS Keychain lookup failed: %w", err)
	}
	return parseKeychainPassword(output)
}

// parseKeychainPassword enforces the one-line rule on raw `security …
// find-generic-password -w` output. /usr/bin/security terminates -w output
// with one newline. Preserve all other bytes (including spaces), but reject
// empty or multi-line output so a diagnostic or otherwise unexpected
// response is never used as a secret. It is a pure function so the rule can
// be fuzzed without spawning subprocesses.
func parseKeychainPassword(output []byte) (string, error) {
	password := strings.TrimSuffix(string(output), "\n")
	password = strings.TrimSuffix(password, "\r")
	if password == "" || strings.ContainsAny(password, "\r\n\x00") {
		return "", errMalformedKeychainPassword
	}
	return password, nil
}

// passwordFromConfiguredSources resolves the password in priority order:
// configured macOS Keychain item, then the legacy JETKVM_PASSWORD value. A
// working fallback deliberately absorbs missing-item and malformed-output
// failures so an existing deployment keeps working during migration.
func passwordFromConfiguredSources() (string, error) {
	fallback := os.Getenv("JETKVM_PASSWORD")
	service := strings.TrimSpace(os.Getenv("JETKVM_PASSWORD_KEYCHAIN_SERVICE"))
	account := strings.TrimSpace(os.Getenv("JETKVM_PASSWORD_KEYCHAIN_ACCOUNT"))

	if service == "" && account == "" {
		return fallback, nil
	}
	if service == "" || account == "" {
		if fallback != "" {
			return fallback, nil
		}
		return "", fmt.Errorf(
			"macOS Keychain password configuration requires both " +
				"JETKVM_PASSWORD_KEYCHAIN_SERVICE and JETKVM_PASSWORD_KEYCHAIN_ACCOUNT")
	}

	password, err := passwordFromKeychain(service, account)
	if err == nil {
		return password, nil
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", err
}

// credentialsFromEnv builds jetkvm.Credentials from the non-logging
// mechanisms this tool supports: environment variables, or (if
// --password-stdin was passed) the first line of stdin. Never from a CLI
// argument, so a password can never appear in `ps`, shell history, or
// process listings.
func credentialsFromEnv(cf *commonFlags) (jetkvm.Credentials, error) {
	var creds jetkvm.Credentials
	if tok := os.Getenv("JETKVM_AUTH_TOKEN"); tok != "" {
		creds.AuthToken = jetkvm.NewSecret(tok)
	}
	if creds.AuthToken.Empty() {
		pw, err := passwordFromConfiguredSources()
		if err != nil {
			return creds, err
		}
		if pw != "" {
			creds.Password = jetkvm.NewSecret(pw)
		}
	}
	if cf.passwordStdin {
		line, err := readLine(os.Stdin)
		if err != nil {
			return creds, fmt.Errorf("reading password from stdin: %w", err)
		}
		creds.Password = jetkvm.NewSecret(line)
	}
	return creds, nil
}

func readLine(f *os.File) (string, error) {
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("no input")
	}
	return strings.TrimRight(scanner.Text(), "\r\n"), nil
}

func connectFromFlags(ctx context.Context, cf *commonFlags, allowControl bool) (*jetkvm.Client, error) {
	baseURL, err := canonicalURLFromFlags(cf)
	if err != nil {
		return nil, err
	}
	creds, err := credentialsFromEnv(cf)
	if err != nil {
		return nil, err
	}
	return jetkvm.Connect(ctx, jetkvm.Options{
		BaseURL:      baseURL,
		Credentials:  creds,
		AllowControl: allowControl,
		HTTPTimeout:  cf.timeout,
	})
}

// rootContext returns a context canceled on SIGINT/SIGTERM, so an
// in-flight control command is guaranteed to hit its cancellation path
// (and therefore release-all) on Ctrl-C rather than leaving keys stuck.
func rootContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// commandContext gives every one-shot command a real deadline in addition to
// SIGINT/SIGTERM cancellation. Without it, a connected peer that never emits a
// usable video frame can hold the CLI open forever.
func commandContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	root, stopSignals := rootContext()
	if timeout <= 0 {
		return root, stopSignals
	}
	ctx, cancelTimeout := context.WithTimeout(root, timeout)
	return ctx, func() {
		cancelTimeout()
		stopSignals()
	}
}

func runStatus(args []string) error {
	fs := newCommandFlagSet("status")
	cf := addCommonFlags(fs, false)
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if err := requirePositiveTimeout(cf); err != nil {
		return err
	}

	ctx, cancel := commandContext(cf.timeout)
	defer cancel()

	client, err := connectFromFlags(ctx, cf, false)
	if err != nil {
		return err
	}
	defer client.Close(ctx)

	status, err := client.Status(ctx)
	if err != nil {
		return err
	}

	out := map[string]any{
		"deviceId":        status.DeviceID,
		"firmwareVersion": status.FirmwareVersion,
		"rpcReachable":    status.RPCReachable,
	}
	return printJSON(out)
}

func runScreenshot(args []string) error {
	fs := newCommandFlagSet("screenshot")
	cf := addCommonFlags(fs, false)
	output := fs.String("output", "", "output PNG path (required)")
	diagnostics := fs.Bool("diagnostics", false,
		"print a privacy-safe video-pipeline diagnostic report to stderr (counts, states, codec parameters only)")
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if err := requirePositiveTimeout(cf); err != nil {
		return err
	}
	if *output == "" {
		return fmt.Errorf("--output is required")
	}

	ctx, cancel := commandContext(cf.timeout)
	defer cancel()
	if _, err := canonicalURLFromFlags(cf); err != nil {
		return err
	}
	if err := (&jetkvm.FFmpegDecoder{}).CheckAvailable(ctx); err != nil {
		return err
	}

	client, err := connectFromFlags(ctx, cf, false)
	if err != nil {
		return err
	}
	defer client.Close(ctx)

	shot, err := client.SaveScreenshot(ctx, *output)

	// Emitted on success and failure alike: a successful capture's counts
	// are the baseline that makes a failing run's counts interpretable.
	// stderr, so the result JSON on stdout stays machine-readable.
	if *diagnostics {
		if reportErr := printDiagnostics(client.VideoDiagnostics()); reportErr != nil {
			fmt.Fprintf(os.Stderr, "jetkvmctl: writing diagnostics: %v\n", reportErr)
		}
	}
	if err != nil {
		return err
	}

	out := map[string]any{
		"path":       shot.Path,
		"width":      shot.Width,
		"height":     shot.Height,
		"capturedAt": shot.CapturedAt.Format(time.RFC3339Nano),
		"fresh":      shot.Fresh,
	}
	return printJSON(out)
}

func runServe(args []string) error {
	fs := newCommandFlagSet("serve")
	cf := addCommonFlags(fs, true)
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}

	// Reject the stdin conflict before anything reads a byte of stdin or
	// touches the network.
	if cf.passwordStdin {
		return errPasswordStdinWithServe
	}
	if err := requirePositiveTimeout(cf); err != nil {
		return err
	}
	baseURL, err := canonicalURLFromFlags(cf)
	if err != nil {
		return err
	}

	ctx, cancel := rootContext()
	defer cancel()

	creds, err := credentialsFromEnv(cf)
	if err != nil {
		return err
	}

	return mcpserver.Run(ctx, mcpserver.Options{
		BaseURL:      baseURL,
		Credentials:  creds,
		AllowControl: cf.allowControl,
		HTTPTimeout:  cf.timeout,
	})
}

func runKeypress(args []string) error {
	fs := newCommandFlagSet("keypress")
	cf := addCommonFlags(fs, true)
	key := fs.Int("key", -1, "USB HID key code (required)")
	modifier := fs.Int("modifier", 0, "modifier bitmask")
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if err := requirePositiveTimeout(cf); err != nil {
		return err
	}
	if !cf.allowControl {
		return fmt.Errorf("keypress requires --allow-control")
	}
	// Validate integer input before any narrowing to a wire byte and before
	// any connection attempt. CLI and MCP share this exact function.
	if err := jetkvm.ValidateKeypress(*key, *modifier); err != nil {
		return fmt.Errorf("invalid keypress: %w", err)
	}

	ctx, cancel := commandContext(cf.timeout)
	defer cancel()

	client, err := connectFromFlags(ctx, cf, true)
	if err != nil {
		return err
	}
	defer client.Close(ctx)

	lease, err := client.Control()
	if err != nil {
		return err
	}
	held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
	if err != nil {
		return err
	}
	// Release runs on the way out however this function ends, and reports
	// truthfully if neutralization could not be confirmed.
	defer func() {
		if relErr := held.Release(); relErr != nil {
			fmt.Fprintln(os.Stderr, formatCLIError(relErr))
		}
	}()

	if err := held.SendKeyboardReport(ctx, byte(*modifier), []byte{byte(*key)}); err != nil {
		return err
	}
	return printJSON(map[string]any{"sent": "keypress", "key": *key, "modifier": *modifier})
}

func runMouseMove(args []string) error {
	fs := newCommandFlagSet("mouse-move")
	cf := addCommonFlags(fs, true)
	x := fs.Int("x", -1, "absolute X in [0,32767] (required)")
	y := fs.Int("y", -1, "absolute Y in [0,32767] (required)")
	buttons := fs.Int("buttons", 0, "mouse button bitmask")
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if err := requirePositiveTimeout(cf); err != nil {
		return err
	}
	if !cf.allowControl {
		return fmt.Errorf("mouse-move requires --allow-control")
	}
	if err := jetkvm.ValidatePointer(*x, *y, *buttons); err != nil {
		return fmt.Errorf("invalid mouse move: %w", err)
	}

	ctx, cancel := commandContext(cf.timeout)
	defer cancel()

	client, err := connectFromFlags(ctx, cf, true)
	if err != nil {
		return err
	}
	defer client.Close(ctx)

	lease, err := client.Control()
	if err != nil {
		return err
	}
	held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
	if err != nil {
		return err
	}
	defer func() {
		if relErr := held.Release(); relErr != nil {
			fmt.Fprintln(os.Stderr, formatCLIError(relErr))
		}
	}()

	if err := held.SendPointerReport(ctx, int32(*x), int32(*y), byte(*buttons)); err != nil {
		return err
	}
	return printJSON(map[string]any{"sent": "mouse-move", "x": *x, "y": *y, "buttons": *buttons})
}

func runReleaseAll(args []string) error {
	fs := newCommandFlagSet("release-all")
	cf := addCommonFlags(fs, true)
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if err := requirePositiveTimeout(cf); err != nil {
		return err
	}
	if !cf.allowControl {
		return fmt.Errorf("release-all requires --allow-control")
	}

	ctx, cancel := commandContext(cf.timeout)
	defer cancel()

	client, err := connectFromFlags(ctx, cf, true)
	if err != nil {
		return err
	}
	defer client.Close(ctx)

	lease, err := client.Control()
	if err != nil {
		return err
	}
	held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
	if err != nil {
		return err
	}
	// Release() itself performs the neutralization; that's the whole point
	// of this command existing as an explicit, on-demand safety valve. A
	// non-nil error means it could not be confirmed on the wire, which must
	// surface as a failure rather than a reassuring "sent".
	if err := held.Release(); err != nil {
		return err
	}
	return printJSON(map[string]any{"sent": "release-all", "cursorMoved": false})
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// printDiagnostics writes the video-pipeline report to stderr. The report
// is built entirely from counts, durations, bounded enums and codec
// parameters (see internal/jetkvm/diagnostics.go), so it is safe to paste
// into an issue: it contains no address, credential, SDP, ICE candidate,
// filesystem path, or packet payload.
func printDiagnostics(diag jetkvm.VideoDiagnostics) error {
	fmt.Fprintf(os.Stderr, "\njetkvmctl: video pipeline diagnostics\n  boundary: %s\n\n", diag.FailureBoundary)
	enc := json.NewEncoder(os.Stderr)
	enc.SetIndent("  ", "  ")
	return enc.Encode(diag)
}

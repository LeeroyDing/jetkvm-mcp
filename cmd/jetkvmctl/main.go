// Command jetkvmctl is a browser-free CLI for a JetKVM device: status
// checks, screenshots, stable-screen readiness gating, an MCP stdio server,
// and (opt-in, gated) keyboard and mouse control.
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
	case "wait-stable":
		err = runWaitStable(args[1:])
	case "serve":
		err = runServe(args[1:])
	case "keypress":
		err = runKeypress(args[1:])
	case "type":
		err = runType(args[1:])
	case "key-combo":
		err = runKeyCombo(args[1:])
	case "mouse-move":
		err = runMouseMove(args[1:])
	case "scroll":
		err = runScroll(args[1:])
	case "click":
		err = runClick(args[1:])
	case "drag":
		err = runDrag(args[1:])
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
  jetkvmctl wait-stable  [--url URL] [--threshold F] [--stable-frames N] [--poll-interval DURATION]
  jetkvmctl serve        [--url URL] [--allow-control]
  jetkvmctl keypress     [--url URL] --allow-control --key CODE [--modifier N]
  jetkvmctl type         [--url URL] --allow-control --text TEXT [--delay-ms N]
  jetkvmctl key-combo    [--url URL] --allow-control --combo NAME
  jetkvmctl mouse-move   [--url URL] --allow-control --x N --y N [--buttons N]
  jetkvmctl scroll       [--url URL] --allow-control --dy N [--dx N]
  jetkvmctl click        [--url URL] --allow-control --x N --y N [--button N]
  jetkvmctl drag         [--url URL] --allow-control --x1 N --y1 N --x2 N --y2 N [--button N] [--steps N]
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

Control commands (keypress, type, key-combo, mouse-move, scroll, click, drag,
release-all) require --allow-control and are otherwise refused. See SECURITY.md
for why.

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
func passwordFromKeychain(ctx context.Context, service, account string) (string, error) {
	output, err := exec.CommandContext(
		ctx,
		securityProgram,
		"find-generic-password",
		"-s", service,
		"-a", account,
		"-w",
	).Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("macOS Keychain lookup canceled: %w", ctxErr)
		}
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
// failures so an existing deployment keeps working during migration. Caller
// cancellation/deadline errors remain authoritative.
func passwordFromConfiguredSources(ctx context.Context) (string, error) {
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

	password, err := passwordFromKeychain(ctx, service, account)
	if err == nil {
		return password, nil
	}
	// A canceled or expired command must not silently continue with a legacy
	// fallback after its Keychain subprocess has been stopped.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", err
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", err
}

// credentialsFromEnv builds jetkvm.Credentials from the non-logging
// mechanisms this tool supports. An explicit --password-stdin wins over all
// configured environment and Keychain sources; otherwise an auth token wins,
// followed by Keychain/password fallback ordering. A password is never read
// from a CLI argument, so it cannot appear in `ps` or process listings.
func credentialsFromEnv(ctx context.Context, cf *commonFlags) (jetkvm.Credentials, error) {
	var creds jetkvm.Credentials
	if cf.passwordStdin {
		line, err := readLine(os.Stdin)
		if err != nil {
			return creds, fmt.Errorf("reading password from stdin: %w", err)
		}
		creds.Password = jetkvm.NewSecret(line)
		return creds, nil
	}
	if tok := os.Getenv("JETKVM_AUTH_TOKEN"); tok != "" {
		creds.AuthToken = jetkvm.NewSecret(tok)
	}
	if creds.AuthToken.Empty() {
		pw, err := passwordFromConfiguredSources(ctx)
		if err != nil {
			return creds, err
		}
		if pw != "" {
			creds.Password = jetkvm.NewSecret(pw)
		}
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
	creds, err := credentialsFromEnv(ctx, cf)
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

func runWaitStable(args []string) error {
	fs := newCommandFlagSet("wait-stable")
	cf := addCommonFlags(fs, false)
	threshold := fs.Float64(
		"threshold",
		jetkvm.DefaultWaitStableThreshold,
		fmt.Sprintf("maximum changed-pixel fraction for a stable comparison [0,1] (default %g)", jetkvm.DefaultWaitStableThreshold),
	)
	stableFrames := fs.Int(
		"stable-frames",
		jetkvm.DefaultWaitStableFrames,
		fmt.Sprintf("consecutive stable comparisons required (default %d)", jetkvm.DefaultWaitStableFrames),
	)
	pollInterval := fs.Duration(
		"poll-interval",
		jetkvm.DefaultWaitStablePollInterval,
		fmt.Sprintf("minimum gap between fresh-frame polls (default %s)", jetkvm.DefaultWaitStablePollInterval),
	)
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if err := requirePositiveTimeout(cf); err != nil {
		return err
	}

	opts := jetkvm.WaitStableOptions{
		Threshold:    threshold,
		StableFrames: stableFrames,
		PollInterval: pollInterval,
	}
	// Validate every option before URL/credential resolution, decoder
	// preflight, or network I/O. In particular, flag.Float64 accepts NaN and
	// infinities, which the shared validator must reject explicitly.
	if err := jetkvm.ValidateWaitStableOptions(opts); err != nil {
		return fmt.Errorf("invalid wait-stable options: %w", err)
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

	result, err := client.WaitStable(ctx, opts)
	if err != nil {
		return err
	}

	return printJSON(map[string]any{
		"settled":             result.Settled,
		"framesSampled":       result.FramesSampled,
		"finalChangeFraction": result.FinalChangeFraction,
		"elapsed":             result.Elapsed.String(),
	})
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

	creds, err := credentialsFromEnv(ctx, cf)
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
	if err := sendControlAndRelease(
		func() error { return held.SendKeyboardReport(ctx, byte(*modifier), []byte{byte(*key)}) },
		held.Release,
	); err != nil {
		return err
	}
	return printJSON(map[string]any{"sent": "keypress", "key": *key, "modifier": *modifier})
}

func runType(args []string) error {
	fs := newCommandFlagSet("type")
	cf := addCommonFlags(fs, true)
	text := fs.String("text", "", "UTF-8 text to type using a US keyboard layout (required)")
	delayMS := fs.Int("delay-ms", jetkvm.DefaultTypeDelayMS, fmt.Sprintf("delay between keypresses in milliseconds [0,%d]", jetkvm.MaxTypeDelayMS))
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if err := requirePositiveTimeout(cf); err != nil {
		return err
	}
	if !cf.allowControl {
		return fmt.Errorf("type requires --allow-control")
	}
	textWasSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "text" {
			textWasSet = true
		}
	})
	if !textWasSet {
		return fmt.Errorf("type requires --text")
	}
	if err := jetkvm.ValidateTypeDelay(*delayMS); err != nil {
		return fmt.Errorf("invalid type delay: %w", err)
	}
	keypresses, err := jetkvm.MapTypeString(*text)
	if err != nil {
		return err
	}
	runes := []rune(*text)
	for i, keypress := range keypresses {
		if err := jetkvm.ValidateKeypress(keypress.HIDUsageCode, keypress.Modifier); err != nil {
			return fmt.Errorf("invalid mapped keypress for character %d %q: %w", i+1, runes[i], err)
		}
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
	for i, keypress := range keypresses {
		held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
		if err != nil {
			return fmt.Errorf("%w (before typing character %d %q)", err, i+1, runes[i])
		}
		if err := sendControlAndRelease(
			func() error {
				return held.SendKeyboardReport(ctx, byte(keypress.Modifier), []byte{byte(keypress.HIDUsageCode)})
			},
			held.Release,
		); err != nil {
			return fmt.Errorf("%w (typing character %d %q)", err, i+1, runes[i])
		}
		if i+1 < len(keypresses) && *delayMS > 0 {
			if err := waitTypeDelay(ctx, time.Duration(*delayMS)*time.Millisecond); err != nil {
				return fmt.Errorf("%w (before typing character %d %q)", err, i+2, runes[i+1])
			}
		}
	}

	return printJSON(map[string]any{"sent": "type", "runes": len(keypresses), "delayMs": *delayMS})
}

func waitTypeDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type keyComboSender func(context.Context, *commonFlags, byte, []byte) error

func runKeyCombo(args []string) error {
	return runKeyComboWithSender(args, sendKeyCombo)
}

// runKeyComboWithSender keeps flag parsing, gating, resolution and result
// rendering testable without opening a WebRTC session. Production always
// supplies sendKeyCombo, which owns the same lease/release flow as keypress.
func runKeyComboWithSender(args []string, sender keyComboSender) error {
	fs := newCommandFlagSet("key-combo")
	cf := addCommonFlags(fs, true)
	combo := fs.String("combo", "", "named keyboard chord (required)")
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if err := requirePositiveTimeout(cf); err != nil {
		return err
	}
	if !cf.allowControl {
		return fmt.Errorf("key-combo requires --allow-control")
	}
	if strings.TrimSpace(*combo) == "" {
		return fmt.Errorf("--combo is required")
	}

	// ResolveKeyCombo runs the shared key-combo validator before narrowing
	// the named report to wire bytes. Resolve before any connection attempt.
	modifier, keys, err := jetkvm.ResolveKeyCombo(*combo)
	if err != nil {
		return fmt.Errorf("invalid key combo: %w", err)
	}
	keyCodes := make([]int, len(keys))
	for i, key := range keys {
		keyCodes[i] = int(key)
	}
	if err := jetkvm.ValidateKeyCombo(int(modifier), keyCodes); err != nil {
		return fmt.Errorf("invalid key combo: %w", err)
	}

	ctx, cancel := commandContext(cf.timeout)
	defer cancel()
	if err := sender(ctx, cf, modifier, keys); err != nil {
		return err
	}

	return printJSON(map[string]any{
		"sent":     "key-combo",
		"combo":    *combo,
		"modifier": int(modifier),
		"keys":     keyCodes,
	})
}

func sendKeyCombo(ctx context.Context, cf *commonFlags, modifier byte, keys []byte) error {
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
	return sendControlAndRelease(
		func() error { return held.SendKeyboardReport(ctx, modifier, keys) },
		held.Release,
	)
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
	if err := sendControlAndRelease(
		func() error { return held.SendPointerReport(ctx, int32(*x), int32(*y), byte(*buttons)) },
		held.Release,
	); err != nil {
		return err
	}
	return printJSON(map[string]any{"sent": "mouse-move", "x": *x, "y": *y, "buttons": *buttons})
}

type scrollSender func(context.Context, *commonFlags, int8, int8) error

func runScroll(args []string) error {
	return runScrollWithSender(args, sendScroll)
}

// runScrollWithSender keeps flag parsing, gating, validation and result
// rendering testable without opening a WebRTC session. Production always
// supplies sendScroll, which owns the firmware-specific JSON-RPC call.
func runScrollWithSender(args []string, sender scrollSender) error {
	fs := newCommandFlagSet("scroll")
	cf := addCommonFlags(fs, true)
	dx := fs.Int("dx", 0, fmt.Sprintf("horizontal wheel delta [-%d,%d] (positive = right)", jetkvm.MaxScrollDelta, jetkvm.MaxScrollDelta))
	dy := fs.Int("dy", 0, fmt.Sprintf("vertical wheel delta [-%d,%d] (positive = up; required)", jetkvm.MaxScrollDelta, jetkvm.MaxScrollDelta))
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if err := requirePositiveTimeout(cf); err != nil {
		return err
	}
	if !cf.allowControl {
		return fmt.Errorf("scroll requires --allow-control")
	}
	dyWasSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "dy" {
			dyWasSet = true
		}
	})
	if !dyWasSet {
		return fmt.Errorf("scroll requires --dy")
	}
	// Validate the full-width CLI integers before narrowing to the signed-byte
	// wire representation and before any connection attempt.
	if err := jetkvm.ValidateScroll(*dx, *dy); err != nil {
		return fmt.Errorf("invalid scroll: %w", err)
	}

	ctx, cancel := commandContext(cf.timeout)
	defer cancel()
	if err := sender(ctx, cf, int8(*dx), int8(*dy)); err != nil {
		return err
	}
	return printJSON(map[string]any{"sent": "scroll", "dx": *dx, "dy": *dy})
}

func sendScroll(ctx context.Context, cf *commonFlags, dx, dy int8) error {
	client, err := connectFromFlags(ctx, cf, true)
	if err != nil {
		return err
	}
	defer client.Close(ctx)

	// Current firmware silently drops wheel reports on the hidrpc path. Scroll
	// therefore uses the legacy wheelReport JSON-RPC method instead of a
	// control lease, while the CLI gate above still requires --allow-control.
	return client.Scroll(ctx, dx, dy)
}

func runClick(args []string) error {
	fs := newCommandFlagSet("click")
	cf := addCommonFlags(fs, true)
	x := fs.Int("x", -1, "absolute X in [0,32767] (required)")
	y := fs.Int("y", -1, "absolute Y in [0,32767] (required)")
	button := fs.Int("button", 1, "mouse button bitmask (default 1 = left)")
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if err := requirePositiveTimeout(cf); err != nil {
		return err
	}
	if !cf.allowControl {
		return fmt.Errorf("click requires --allow-control")
	}
	// Validate integer input before any narrowing to the wire types and before
	// any connection attempt. CLI and MCP share this exact function.
	if err := jetkvm.ValidatePointer(*x, *y, *button); err != nil {
		return fmt.Errorf("invalid click: %w", err)
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
	if err := sendControlAndRelease(
		func() error {
			return sendPointerClick(
				func(x, y int32, buttons byte) error {
					return held.SendPointerReport(ctx, x, y, buttons)
				},
				int32(*x), int32(*y), byte(*button),
			)
		},
		held.Release,
	); err != nil {
		return err
	}
	return printJSON(map[string]any{"sent": "click", "x": *x, "y": *y, "button": *button})
}

// sendPointerClick sends both halves of a click at one absolute coordinate.
// The enclosing control lease remains responsible for terminal
// neutralization if either report cannot be confirmed on the wire.
func sendPointerClick(send func(x, y int32, buttons byte) error, x, y int32, button byte) error {
	if err := send(x, y, button); err != nil {
		return err
	}
	return send(x, y, 0)
}

type dragSender func(context.Context, *commonFlags, []jetkvm.PointerDragReport) error

func runDrag(args []string) error {
	return runDragWithSender(args, sendDrag)
}

// runDragWithSender keeps flag parsing, gating, full-width validation, and
// result rendering testable without opening a WebRTC session. Production uses
// sendDrag, which owns the exclusive control lease for the complete gesture.
func runDragWithSender(args []string, sender dragSender) error {
	fs := newCommandFlagSet("drag")
	cf := addCommonFlags(fs, true)
	x1 := fs.Int("x1", -1, "absolute starting X in [0,32767] (required)")
	y1 := fs.Int("y1", -1, "absolute starting Y in [0,32767] (required)")
	x2 := fs.Int("x2", -1, "absolute destination X in [0,32767] (required)")
	y2 := fs.Int("y2", -1, "absolute destination Y in [0,32767] (required)")
	button := fs.Int("button", 1, "mouse button bitmask (default 1 = left)")
	steps := fs.Int("steps", 0, fmt.Sprintf("intermediate held-button moves [0,%d]", jetkvm.MaxDragSteps))
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if err := requirePositiveTimeout(cf); err != nil {
		return err
	}
	if !cf.allowControl {
		return fmt.Errorf("drag requires --allow-control")
	}
	// Validate both endpoints at the CLI adapter boundary before any value can
	// be narrowed to the HID wire representation or any connection is opened.
	if err := jetkvm.ValidatePointer(*x1, *y1, *button); err != nil {
		return fmt.Errorf("invalid drag start: %w", err)
	}
	if err := jetkvm.ValidatePointer(*x2, *y2, *button); err != nil {
		return fmt.Errorf("invalid drag destination: %w", err)
	}
	reports, err := jetkvm.BuildPointerDragReports(*x1, *y1, *x2, *y2, *button, *steps)
	if err != nil {
		return fmt.Errorf("invalid drag: %w", err)
	}

	ctx, cancel := commandContext(cf.timeout)
	defer cancel()
	if err := sender(ctx, cf, reports); err != nil {
		return err
	}
	return printJSON(map[string]any{
		"sent":   "drag",
		"x1":     *x1,
		"y1":     *y1,
		"x2":     *x2,
		"y2":     *y2,
		"button": *button,
		"steps":  *steps,
	})
}

func sendDrag(ctx context.Context, cf *commonFlags, reports []jetkvm.PointerDragReport) error {
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
	return sendControlAndRelease(
		func() error {
			return sendPointerDrag(func(x, y int32, buttons byte) error {
				return held.SendPointerReport(ctx, x, y, buttons)
			}, reports)
		},
		held.Release,
	)
}

// sendPointerDrag sends every report in one already-validated drag while the
// enclosing control lease remains held. It validates the full sequence again
// before narrowing any report to HID wire types.
func sendPointerDrag(send func(x, y int32, buttons byte) error, reports []jetkvm.PointerDragReport) error {
	for i, report := range reports {
		if err := jetkvm.ValidatePointer(report.X, report.Y, report.Buttons); err != nil {
			return fmt.Errorf("drag report %d: %w", i+1, err)
		}
	}
	for _, report := range reports {
		if err := send(int32(report.X), int32(report.Y), byte(report.Buttons)); err != nil {
			return err
		}
	}
	return nil
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

// sendControlAndRelease makes neutralization part of a control command's
// success boundary. Release is attempted exactly once even when send fails;
// the caller cannot print success or exit zero if neutralization was not
// confirmed. Joining both errors preserves the primary send failure and the
// independent safety failure for the top-level redaction boundary.
func sendControlAndRelease(send, release func() error) error {
	sendErr := send()
	releaseErr := release()
	return errors.Join(sendErr, releaseErr)
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

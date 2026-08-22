// Command jetkvmctl is a browser-free CLI for a JetKVM device: status
// checks, screenshots, OCR text reads, content/stability readiness gating, an
// MCP stdio server, and (opt-in, gated) keyboard and mouse control.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
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
	case "read-text":
		err = runReadText(args[1:])
	case "wait-stable":
		err = runWaitStable(args[1:])
	case "wait-for-text":
		err = runWaitForText(args[1:])
	case "serve":
		err = runServe(args[1:])
	case "keypress":
		err = runKeypress(args[1:])
	case "type":
		err = runType(args[1:])
	case "key-combo":
		err = runKeyCombo(args[1:])
	case "hold-key":
		err = runHoldKey(args[1:])
	case "key-sequence":
		err = runKeySequence(args[1:])
	case "mouse-button":
		err = runMouseButton(args[1:])
	case "mouse-move":
		err = runMouseMove(args[1:])
	case "scroll":
		err = runScroll(args[1:])
	case "click":
		err = runClick(args[1:])
	case "double-click":
		err = runDoubleClick(args[1:])
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

	if errors.Is(err, flag.ErrHelp) {
		return 0, nil
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

func printUsage(w io.Writer) {
	fmt.Fprint(w, `jetkvmctl - browser-free JetKVM controller

Usage:
  jetkvmctl --version
  jetkvmctl doctor       [--probe-device [--url URL] [--timeout DURATION]]
  jetkvmctl status       [--url URL]
  jetkvmctl screenshot   [--url URL] --output PATH [--diagnostics]
  jetkvmctl read-text    [--url URL] [--scale F] [--region X,Y,WIDTH,HEIGHT]
  jetkvmctl wait-stable  [--url URL] [--threshold F] [--stable-frames N] [--poll-interval DURATION]
  jetkvmctl wait-for-text [--url URL] --text TEXT [--regex] [--interval DURATION]
  jetkvmctl serve        [--url URL] [--allow-control]
  jetkvmctl keypress     [--url URL] --allow-control --key CODE [--modifier N]
  jetkvmctl type         [--url URL] --allow-control --text TEXT [--delay-ms N]
  jetkvmctl key-combo    [--url URL] --allow-control --combo NAME
  jetkvmctl hold-key     [--url URL] --allow-control --combo NAME --hold-ms N
  jetkvmctl key-sequence [--url URL] --allow-control --combo NAME [--combo NAME ...] [--delay-ms N]
  jetkvmctl mouse-button [--url URL] --allow-control --button NAME --action ACTION
  jetkvmctl mouse-move   [--url URL] --allow-control --x N --y N [--buttons N]
  jetkvmctl scroll       [--url URL] --allow-control --dy N [--dx N]
  jetkvmctl click        [--url URL] --allow-control --x N --y N [--button N]
  jetkvmctl double-click [--url URL] --allow-control --x N --y N [--button N]
  jetkvmctl drag         [--url URL] --allow-control --x1 N --y1 N --x2 N --y2 N [--button N] [--steps N]
  jetkvmctl release-all  [--url URL] --allow-control

Connection:
  --url URL           Device base URL (required, or set $JETKVM_URL)
  --timeout DURATION  Operation timeout (default 10s); wait-for-text returns
                      polling expiry as a structured timeout, while serve
                      uses it per connection/tool operation

Credentials (never pass these as flags/arguments):
  JETKVM_PASSWORD_KEYCHAIN_SERVICE / JETKVM_PASSWORD_KEYCHAIN_ACCOUNT
                      macOS Keychain generic-password item to read first
  JETKVM_PASSWORD     fallback env var: log in with this password
  JETKVM_AUTH_TOKEN   env var: use this already-valid session cookie directly
  --password-stdin    read a password from stdin (first line) instead; accepted
                      by direct device commands, including read-text and
                      wait-for-text. Rejected by 'serve': MCP owns stdin.

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

Control commands (keypress, type, key-combo, hold-key, key-sequence,
mouse-button, mouse-move, scroll, click, double-click, drag, release-all)
require --allow-control and are
otherwise refused.
See SECURITY.md for why.

release-all sends canonical neutral reports for every input interface the session
may have left holding state, using zero relative deltas and the last recorded
absolute coordinates.
Success means those reports were acknowledged by the peer SCTP transport; it
does not prove firmware USB application or attached-host action.

Transport warning: affected JetKVM firmware serves its web API and signaling
WebSocket over plaintext HTTP on the LAN, so the password and session cookie
travel unencrypted and are visible to anyone who can observe that network.
Use only on a trusted, isolated network or over a VPN. This client cannot
upgrade the device's transport. See SECURITY.md.
`)
}

type commandHelp struct {
	synopsis    string
	description string
}

var commandHelpByName = map[string]commandHelp{
	"version": {
		synopsis:    "version",
		description: "Print version and build provenance as JSON.",
	},
	"doctor": {
		synopsis:    "doctor [--probe-device [--url URL] [--timeout DURATION]]",
		description: "Report local diagnostics and optionally probe one device.",
	},
	"status": {
		synopsis:    "status [--url URL]",
		description: "Report device identity, firmware, and RPC reachability.",
	},
	"screenshot": {
		synopsis:    "screenshot [--url URL] --output PATH [--diagnostics]",
		description: "Capture a fresh video frame and save it as a PNG.",
	},
	"read-text": {
		synopsis:    "read-text [--url URL] [--scale F] [--region X,Y,WIDTH,HEIGHT]",
		description: "Capture a fresh frame and print its OCR text.",
	},
	"wait-stable": {
		synopsis:    "wait-stable [--url URL] [--threshold F] [--stable-frames N] [--poll-interval DURATION]",
		description: "Wait until successive video frames remain stable.",
	},
	"wait-for-text": {
		synopsis:    "wait-for-text [--url URL] --text TEXT [--regex] [--interval DURATION]",
		description: "Poll OCR until literal or regular-expression text appears.",
	},
	"serve": {
		synopsis:    "serve [--url URL] [--allow-control]",
		description: "Run the JetKVM MCP server over standard input and output.",
	},
	"keypress": {
		synopsis:    "keypress [--url URL] --allow-control --key CODE [--modifier N]",
		description: "Send one USB HID keypress and release it.",
	},
	"type": {
		synopsis:    "type [--url URL] --allow-control --text TEXT [--delay-ms N]",
		description: "Type non-empty UTF-8 text using a US keyboard layout.",
	},
	"key-combo": {
		synopsis:    "key-combo [--url URL] --allow-control --combo NAME",
		description: "Send one named keyboard chord and release it.",
	},
	"hold-key": {
		synopsis:    "hold-key [--url URL] --allow-control --combo NAME --hold-ms N",
		description: "Hold one named keyboard chord for a bounded duration.",
	},
	"key-sequence": {
		synopsis:    "key-sequence [--url URL] --allow-control --combo NAME [--combo NAME ...] [--delay-ms N]",
		description: "Send an ordered sequence of named keyboard chords.",
	},
	"mouse-button": {
		synopsis:    "mouse-button [--url URL] --allow-control --button NAME --action ACTION",
		description: "Press or release one mouse button without moving the pointer.",
	},
	"mouse-move": {
		synopsis:    "mouse-move [--url URL] --allow-control --x N --y N [--buttons N]",
		description: "Move the pointer to absolute coordinates.",
	},
	"scroll": {
		synopsis:    "scroll [--url URL] --allow-control --dy N [--dx N]",
		description: "Send a horizontal or vertical mouse-wheel event.",
	},
	"click": {
		synopsis:    "click [--url URL] --allow-control --x N --y N [--button N]",
		description: "Click once at absolute pointer coordinates.",
	},
	"double-click": {
		synopsis:    "double-click [--url URL] --allow-control --x N --y N [--button N]",
		description: "Click twice at absolute pointer coordinates.",
	},
	"drag": {
		synopsis:    "drag [--url URL] --allow-control --x1 N --y1 N --x2 N --y2 N [--button N] [--steps N]",
		description: "Drag between two absolute pointer coordinates.",
	},
	"release-all": {
		synopsis:    "release-all [--url URL] --allow-control",
		description: "Send canonical neutral reports for every input interface.",
	},
}

func printCommandUsage(w io.Writer, fs *flag.FlagSet) {
	help, ok := commandHelpByName[fs.Name()]
	if !ok {
		help = commandHelp{
			synopsis:    fs.Name(),
			description: "Run the " + fs.Name() + " command.",
		}
	}

	fmt.Fprintf(w, "%s\n\nUsage:\n  jetkvmctl %s\n\nFlags:\n", help.description, help.synopsis)
	hasFlags := false
	fs.VisitAll(func(f *flag.Flag) {
		hasFlags = true
		valueName, usage := flag.UnquoteUsage(f)
		fmt.Fprintf(w, "  --%s", f.Name)
		if valueName != "" {
			fmt.Fprintf(w, " %s", strings.ToUpper(valueName))
		}
		fmt.Fprintf(w, "\n      %s", usage)
		if !strings.Contains(usage, "(default ") && !strings.Contains(usage, "(default:") {
			fmt.Fprintf(w, " (default: %s)", commandFlagDefault(f, usage))
		}
		fmt.Fprintln(w)
	})
	if !hasFlags {
		fmt.Fprintln(w, "  (none)")
	}
}

// commandFlagDefault must never render the environment-derived URL value:
// it can contain userinfo or query credentials. All other defaults are fixed
// by the program and are safe to show.
func commandFlagDefault(f *flag.Flag, usage string) string {
	if f.Name == "url" {
		return "$JETKVM_URL if set; otherwise unset"
	}
	// Required flags have no behavioral default. Several integer parsers use
	// -1 or 0 only as an omission sentinel; advertising those rejected values
	// as defaults makes the generated help contradict the command contract.
	if strings.Contains(usage, "required") {
		return "unset"
	}
	if f.DefValue == "" {
		return "unset"
	}
	return f.DefValue
}

// commonFlags groups connection, credential, deadline, and optional control
// switches reused by device-facing subcommands. Doctor populates its
// probe-only subset separately; version has no common flags.
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
	fs.Usage = func() { printCommandUsage(os.Stdout, fs) }
	return fs
}

// parseCommandFlags collapses flag's raw diagnostics to a fixed message and
// rejects positional arguments uniformly. The standard flag errors quote
// invalid values; those values can be credential canaries or URLs and must
// not cross the CLI's public error boundary in the first place.
func parseCommandFlags(fs *flag.FlagSet, args []string) error {
	// The flag package calls Usage for malformed flags as well as for help.
	// Suppress that callback while parsing, then render it ourselves only for
	// ErrHelp so genuine failures retain their fixed, non-reflective output.
	printHelp := fs.Usage
	fs.Usage = func() {}
	err := fs.Parse(args)
	fs.Usage = printHelp
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// flag also returns ErrHelp for undocumented forms such as
			// -help and --help=value. Only the two public aliases are help;
			// retain the fixed invalid-arguments failure for every other form.
			consumed := len(args) - len(fs.Args())
			if consumed > 0 && (args[consumed-1] == "-h" || args[consumed-1] == "--help") {
				if printHelp != nil {
					printHelp()
				}
				return flag.ErrHelp
			}
		}
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

// addServeFlags gives the two security-sensitive switches their serve-specific
// meaning. Unlike a one-shot control command, serve is valid without
// --allow-control, and --password-stdin is parsed only so it can fail with the
// actionable MCP-stdin conflict instead of a generic unknown-flag error.
func addServeFlags(fs *flag.FlagSet) *commonFlags {
	cf := addCommonFlags(fs, false)
	fs.Lookup("password-stdin").Usage = "rejected by serve because the MCP protocol owns stdin; use Keychain or environment credentials instead"
	fs.BoolVar(&cf.allowControl, "allow-control", false, "expose opt-in readiness gates and dangerous keyboard/mouse control tools")
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

const maxReadTextRegionValue = 1<<31 - 1

// readTextRegionFlag parses the CLI's compact source-pixel crop syntax while
// retaining whether the optional flag was supplied at all. Its Set method
// never includes the caller's raw value in an error: flag values may contain
// credential canaries, and parseCommandFlags deliberately collapses all such
// diagnostics at the public boundary.
type readTextRegionFlag struct {
	set    bool
	region mcpserver.ScreenshotRegion
}

func (f *readTextRegionFlag) String() string {
	if f == nil || !f.set {
		return ""
	}
	return fmt.Sprintf("%d,%d,%d,%d", f.region.X, f.region.Y, f.region.Width, f.region.Height)
}

func (f *readTextRegionFlag) Set(value string) error {
	region, err := parseReadTextRegion(value)
	if err != nil {
		return err
	}
	f.region = region
	f.set = true
	return nil
}

// parseReadTextRegion accepts exactly x,y,width,height in source pixels.
// ParseInt's 32-bit bound matches the MCP screenshot schema and prevents a
// platform-sized int from admitting coordinates the shared renderer rejects.
func parseReadTextRegion(value string) (mcpserver.ScreenshotRegion, error) {
	parts := strings.Split(value, ",")
	if len(parts) != 4 {
		return mcpserver.ScreenshotRegion{}, errors.New("region must contain x,y,width,height")
	}

	var values [4]int64
	for i, part := range parts {
		parsed, err := strconv.ParseInt(strings.TrimSpace(part), 10, 32)
		if err != nil {
			return mcpserver.ScreenshotRegion{}, errors.New("region values must be 32-bit integers")
		}
		values[i] = parsed
	}
	if values[0] < 0 || values[1] < 0 {
		return mcpserver.ScreenshotRegion{}, errors.New("region x and y must be non-negative")
	}
	if values[2] <= 0 || values[3] <= 0 {
		return mcpserver.ScreenshotRegion{}, errors.New("region width and height must be positive")
	}
	if values[0] > maxReadTextRegionValue || values[1] > maxReadTextRegionValue ||
		values[2] > maxReadTextRegionValue || values[3] > maxReadTextRegionValue {
		return mcpserver.ScreenshotRegion{}, errors.New("region values exceed the supported range")
	}
	return mcpserver.ScreenshotRegion{
		X: int(values[0]), Y: int(values[1]),
		Width: int(values[2]), Height: int(values[3]),
	}, nil
}

type readTextCapture func(context.Context, *commonFlags) (jetkvm.Screenshot, error)

// readTextDependencies keeps the CLI adapter testable without PATH lookups,
// subprocesses, credentials, or a WebRTC session. Production supplies the
// real decoder preflight, Tesseract engine, and one-frame capture path.
type readTextDependencies struct {
	checkDecoder func(context.Context) error
	ocr          jetkvm.OCREngine
	capture      readTextCapture
	stdout       io.Writer
}

func runReadText(args []string) error {
	return runReadTextWithDependencies(args, readTextDependencies{
		checkDecoder: (&jetkvm.FFmpegDecoder{}).CheckAvailable,
		ocr:          &jetkvm.TesseractOCREngine{},
		capture:      captureReadTextScreenshot,
		stdout:       os.Stdout,
	})
}

func runReadTextWithDependencies(args []string, deps readTextDependencies) error {
	fs := newCommandFlagSet("read-text")
	cf := addCommonFlags(fs, false)
	scale := fs.Float64("scale", 1, "positive output scale factor; values above 1 clamp to 1")
	var regionFlag readTextRegionFlag
	fs.Var(&regionFlag, "region", "source-pixel crop as x,y,width,height")
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if err := requirePositiveTimeout(cf); err != nil {
		return err
	}
	if math.IsNaN(*scale) || math.IsInf(*scale, 0) || *scale <= 0 {
		return errors.New("--scale must be a positive finite number")
	}
	*scale = min(*scale, 1)

	options := mcpserver.ScreenshotTransformOptions{Scale: scale}
	if regionFlag.set {
		region := regionFlag.region
		options.Region = &region
	}

	ctx, cancel := commandContext(cf.timeout)
	defer cancel()
	// Validate the URL before any PATH lookup or credential resolution. The
	// capture dependency re-validates it through connectFromFlags as defense
	// in depth, matching the other one-shot read-only commands.
	if _, err := canonicalURLFromFlags(cf); err != nil {
		return err
	}
	if deps.checkDecoder == nil {
		return errors.New("jetkvm: screenshot decoder is unavailable")
	}
	if err := deps.checkDecoder(ctx); err != nil {
		return err
	}
	if deps.ocr == nil {
		return errors.New("jetkvm: OCR engine is unavailable")
	}
	if err := deps.ocr.CheckAvailable(ctx); err != nil {
		return err
	}
	if deps.capture == nil {
		return errors.New("jetkvm: screenshot capture is unavailable")
	}

	shot, err := deps.capture(ctx, cf)
	if err != nil {
		return err
	}
	rendered, err := mcpserver.RenderScreenshotForText(ctx, shot, options)
	if err != nil {
		return err
	}
	text, err := deps.ocr.ReadText(ctx, rendered.Data)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("jetkvm: read-text canceled after OCR: %w", err)
	}
	if deps.stdout == nil {
		return errors.New("jetkvm: text output is unavailable")
	}
	_, err = fmt.Fprint(deps.stdout, text)
	return err
}

func captureReadTextScreenshot(ctx context.Context, cf *commonFlags) (jetkvm.Screenshot, error) {
	client, err := connectFromFlags(ctx, cf, false)
	if err != nil {
		return jetkvm.Screenshot{}, err
	}
	defer client.Close(ctx)
	return client.CaptureScreenshot(ctx)
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

type waitForTextRunner func(
	context.Context,
	*commonFlags,
	jetkvm.WaitForTextOptions,
	jetkvm.OCREngine,
) (jetkvm.WaitForTextResult, error)

// waitForTextDependencies keeps flag validation, preflight ordering, and JSON
// rendering hermetic in tests. Production supplies the real FFmpeg check,
// Tesseract engine, and request-fresh screenshot capture path.
type waitForTextDependencies struct {
	checkDecoder func(context.Context) error
	ocr          jetkvm.OCREngine
	run          waitForTextRunner
}

func runWaitForText(args []string) error {
	return runWaitForTextWithDependencies(args, waitForTextDependencies{
		checkDecoder: (&jetkvm.FFmpegDecoder{}).CheckAvailable,
		ocr:          &jetkvm.TesseractOCREngine{},
		run:          waitForTextOnDevice,
	})
}

func runWaitForTextWithDependencies(args []string, deps waitForTextDependencies) error {
	fs := newCommandFlagSet("wait-for-text")
	cf := addCommonFlags(fs, false)
	wantedText := fs.String("text", "", "required substring or regular expression to wait for")
	useRegex := fs.Bool("regex", false, "interpret --text as a Go regular expression")
	interval := fs.Duration(
		"interval",
		jetkvm.DefaultWaitForTextInterval,
		fmt.Sprintf("minimum gap between OCR polls (default %s)", jetkvm.DefaultWaitForTextInterval),
	)
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if err := requirePositiveTimeout(cf); err != nil {
		return err
	}
	if *wantedText == "" {
		return errors.New("--text is required")
	}

	opts := jetkvm.WaitForTextOptions{
		Text:     *wantedText,
		Regex:    *useRegex,
		Interval: interval,
		Timeout:  &cf.timeout,
	}
	// Validate every caller-controlled option before URL parsing, credential
	// resolution, executable lookup, screenshot decode, OCR, or network I/O.
	if err := jetkvm.ValidateWaitForTextOptions(opts); err != nil {
		return fmt.Errorf("invalid wait-for-text options: %w", err)
	}

	// Keep the existing CLI contract: --timeout bounds the complete one-shot
	// command, including local preflights, credential resolution, connection,
	// capture, and OCR. WaitForText converts a deadline reached during polling
	// into its structured timedOut result.
	ctx, cancel := commandContext(cf.timeout)
	defer cancel()
	if _, err := canonicalURLFromFlags(cf); err != nil {
		return err
	}
	if deps.checkDecoder == nil {
		return errors.New("jetkvm: screenshot decoder is unavailable")
	}
	if err := deps.checkDecoder(ctx); err != nil {
		return err
	}
	if deps.ocr == nil {
		return errors.New("jetkvm: OCR engine is unavailable")
	}
	if err := deps.ocr.CheckAvailable(ctx); err != nil {
		return err
	}
	if deps.run == nil {
		return errors.New("jetkvm: wait-for-text runner is unavailable")
	}

	result, err := deps.run(ctx, cf, opts, deps.ocr)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{
		"matched":    result.Matched,
		"match":      result.Match,
		"timedOut":   result.TimedOut,
		"elapsed":    result.Elapsed.String(),
		"frameCount": result.FrameCount,
	})
}

func waitForTextOnDevice(
	ctx context.Context,
	cf *commonFlags,
	opts jetkvm.WaitForTextOptions,
	engine jetkvm.OCREngine,
) (jetkvm.WaitForTextResult, error) {
	client, err := connectFromFlags(ctx, cf, false)
	if err != nil {
		return jetkvm.WaitForTextResult{}, err
	}
	defer client.Close(ctx)
	return jetkvm.WaitForText(ctx, opts, client.CaptureScreenshot, engine)
}

func runServe(args []string) error {
	fs := newCommandFlagSet("serve")
	cf := addServeFlags(fs)
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

type typeKeyboardControl interface {
	SendKeyboardReport(context.Context, byte, []byte) error
	Release() error
}

type typeControlAcquirer func(context.Context, time.Duration) (typeKeyboardControl, error)

func runType(args []string) error {
	fs := newCommandFlagSet("type")
	cf := addCommonFlags(fs, true)
	text := fs.String("text", "", "non-empty UTF-8 text to type using a US keyboard layout (required)")
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
			return fmt.Errorf("invalid mapped keypress for %s: %w", jetkvm.TypeCharacterContext(i+1, runes[i]), err)
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
	if err := sendTypeKeypresses(
		ctx,
		keypresses,
		runes,
		time.Duration(*delayMS)*time.Millisecond,
		func(ctx context.Context, timeout time.Duration) (typeKeyboardControl, error) {
			held, err := lease.Acquire(ctx, timeout)
			if err != nil {
				return nil, err
			}
			return held, nil
		},
		waitInterKeyDelay,
	); err != nil {
		return err
	}

	return printJSON(map[string]any{"sent": "type", "runes": len(keypresses), "delayMs": *delayMS})
}

func sendTypeKeypresses(
	ctx context.Context,
	keypresses []jetkvm.TypeKeypress,
	runes []rune,
	delay time.Duration,
	acquire typeControlAcquirer,
	wait func(context.Context, time.Duration) error,
) error {
	if len(keypresses) != len(runes) {
		return fmt.Errorf("mapped keypress count %d does not match character count %d", len(keypresses), len(runes))
	}
	for i, keypress := range keypresses {
		held, err := acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
		if err != nil {
			return fmt.Errorf("%w before typing %s", err, jetkvm.TypeCharacterContext(i+1, runes[i]))
		}
		if err := sendControlAndRelease(
			func() error {
				return held.SendKeyboardReport(ctx, byte(keypress.Modifier), []byte{byte(keypress.HIDUsageCode)})
			},
			held.Release,
		); err != nil {
			return fmt.Errorf("%w while typing %s", err, jetkvm.TypeCharacterContext(i+1, runes[i]))
		}
		if i+1 < len(keypresses) && delay > 0 {
			if err := wait(ctx, delay); err != nil {
				return fmt.Errorf("%w before typing %s", err, jetkvm.TypeCharacterContext(i+2, runes[i+1]))
			}
		}
	}
	return nil
}

func waitInterKeyDelay(ctx context.Context, delay time.Duration) error {
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

type holdKeySender func(context.Context, *commonFlags, byte, []byte, int) error

func runHoldKey(args []string) error {
	return runHoldKeyWithSender(args, sendHoldKey)
}

// runHoldKeyWithSender keeps gating, complete input validation, and result
// rendering testable without opening a device session. Production supplies a
// sender that holds one control lease until the timer or context ends.
func runHoldKeyWithSender(args []string, sender holdKeySender) error {
	fs := newCommandFlagSet("hold-key")
	cf := addCommonFlags(fs, true)
	combo := fs.String("combo", "", "named keyboard chord (required)")
	holdMS := fs.Int("hold-ms", 0, fmt.Sprintf("duration to hold the chord in milliseconds [1,%d] (required)", jetkvm.MaxHoldMS))
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if err := requirePositiveTimeout(cf); err != nil {
		return err
	}
	if !cf.allowControl {
		return fmt.Errorf("hold-key requires --allow-control")
	}
	if strings.TrimSpace(*combo) == "" {
		return fmt.Errorf("--combo is required")
	}
	holdMSWasSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "hold-ms" {
			holdMSWasSet = true
		}
	})
	if !holdMSWasSet {
		return fmt.Errorf("hold-key requires --hold-ms")
	}

	// Resolve and validate every input before constructing a command context,
	// resolving credentials, connecting, acquiring a lease, or sending HID.
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
	if err := jetkvm.ValidateHoldMS(*holdMS); err != nil {
		return fmt.Errorf("invalid hold duration: %w", err)
	}

	ctx, cancel := commandContext(cf.timeout)
	defer cancel()
	if err := sender(ctx, cf, modifier, keys, *holdMS); err != nil {
		return err
	}

	return printJSON(map[string]any{
		"sent":     "hold-key",
		"combo":    *combo,
		"holdMs":   *holdMS,
		"modifier": int(modifier),
		"keys":     keyCodes,
	})
}

func sendHoldKey(ctx context.Context, cf *commonFlags, modifier byte, keys []byte, holdMS int) error {
	// Keep the production sender independently safe if a future caller bypasses
	// runHoldKeyWithSender: validate before credentials, connection, or HID.
	keyCodes := make([]int, len(keys))
	for i, key := range keys {
		keyCodes[i] = int(key)
	}
	if err := jetkvm.ValidateKeyCombo(int(modifier), keyCodes); err != nil {
		return fmt.Errorf("invalid key combo: %w", err)
	}
	if err := jetkvm.ValidateHoldMS(holdMS); err != nil {
		return fmt.Errorf("invalid hold duration: %w", err)
	}

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
	return sendControlHoldAndRelease(
		ctx,
		holdMS,
		func() error { return held.SendKeyboardReport(ctx, modifier, keys) },
		held.Release,
	)
}

// sendControlHoldAndRelease installs terminal neutralization before sending
// key-down, waits interruptibly, and joins an independent release failure with
// the primary send/wait error. Held.Release supplies the production cleanup's
// bounded background context, so a canceled ctx cannot suppress it.
func sendControlHoldAndRelease(ctx context.Context, holdMS int, send, release func() error) (err error) {
	defer func() { err = errors.Join(err, release()) }()
	if err := send(); err != nil {
		return err
	}
	timer := time.NewTimer(time.Duration(holdMS) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("holding key combo: %w", ctx.Err())
	}
}

type keySequenceSender func(context.Context, *commonFlags, []jetkvm.ResolvedKeyCombo, int) error

func runKeySequence(args []string) error {
	return runKeySequenceWithSender(args, sendKeySequence)
}

// runKeySequenceWithSender keeps parsing, gating and complete sequence
// validation testable without opening a WebRTC session. The sender is not
// called until every named chord has resolved and passed the wire validator.
func runKeySequenceWithSender(args []string, sender keySequenceSender) error {
	fs := newCommandFlagSet("key-sequence")
	cf := addCommonFlags(fs, true)
	var comboNames []string
	fs.Func("combo", "named keyboard chord (required; repeat in execution order)", func(value string) error {
		comboNames = append(comboNames, value)
		return nil
	})
	delayMS := fs.Int("delay-ms", jetkvm.DefaultTypeDelayMS, fmt.Sprintf("delay between chords in milliseconds [0,%d]", jetkvm.MaxTypeDelayMS))
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if err := requirePositiveTimeout(cf); err != nil {
		return err
	}
	if !cf.allowControl {
		return fmt.Errorf("key-sequence requires --allow-control")
	}
	if len(comboNames) == 0 {
		return fmt.Errorf("key-sequence requires --combo")
	}
	if err := jetkvm.ValidateTypeDelay(*delayMS); err != nil {
		return fmt.Errorf("invalid key sequence delay: %w", err)
	}
	if err := jetkvm.ValidateKeySequenceLength(len(comboNames)); err != nil {
		return fmt.Errorf("invalid key sequence: %w", err)
	}

	resolved, err := jetkvm.ResolveKeySequence(comboNames)
	if err != nil {
		return fmt.Errorf("invalid key sequence: %w", err)
	}

	ctx, cancel := commandContext(cf.timeout)
	defer cancel()
	if err := sender(ctx, cf, resolved, *delayMS); err != nil {
		return err
	}

	return printJSON(map[string]any{
		"sent":    "key-sequence",
		"combos":  len(resolved),
		"delayMs": *delayMS,
	})
}

func sendKeySequence(ctx context.Context, cf *commonFlags, combos []jetkvm.ResolvedKeyCombo, delayMS int) error {
	client, err := connectFromFlags(ctx, cf, true)
	if err != nil {
		return err
	}
	defer client.Close(ctx)

	lease, err := client.Control()
	if err != nil {
		return err
	}
	return sendResolvedKeySequence(ctx, combos, delayMS, func(modifier byte, keys []byte) error {
		held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
		if err != nil {
			return fmt.Errorf("acquiring control lease: %w", err)
		}
		return sendControlAndRelease(
			func() error { return held.SendKeyboardReport(ctx, modifier, keys) },
			held.Release,
		)
	})
}

// sendResolvedKeySequence executes the already-prevalidated reports in order.
// sendAndRelease is synchronous: production supplies the same lease-backed
// send-and-neutralize operation used by key-combo, so it completes before the
// delay and next chord.
func sendResolvedKeySequence(ctx context.Context, combos []jetkvm.ResolvedKeyCombo, delayMS int, sendAndRelease func(byte, []byte) error) error {
	for i, combo := range combos {
		if err := sendAndRelease(combo.Modifier, combo.Keys); err != nil {
			return fmt.Errorf("sending key sequence combo at index %d: %w", i, err)
		}
		if i+1 < len(combos) && delayMS > 0 {
			if err := waitInterKeyDelay(ctx, time.Duration(delayMS)*time.Millisecond); err != nil {
				return fmt.Errorf("waiting before key sequence combo at index %d: %w", i+1, err)
			}
		}
	}
	return nil
}

type mouseButtonSender func(context.Context, *commonFlags, byte, bool) error

func runMouseButton(args []string) error {
	return runMouseButtonWithSender(args, sendMouseButton)
}

// runMouseButtonWithSender keeps flag parsing, gating, exact enum resolution,
// and result rendering testable without opening a device session. Production
// sends one zero-delta relative-mouse report, then neutralizes at the end of
// the one-shot CLI session.
func runMouseButtonWithSender(args []string, sender mouseButtonSender) error {
	fs := newCommandFlagSet("mouse-button")
	cf := addCommonFlags(fs, true)
	button := fs.String("button", "", "mouse button: left, right, or middle (required)")
	action := fs.String("action", "", "button action: press or release (required)")
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if err := requirePositiveTimeout(cf); err != nil {
		return err
	}
	if !cf.allowControl {
		return fmt.Errorf("mouse-button requires --allow-control")
	}
	if *button == "" {
		return fmt.Errorf("mouse-button requires --button")
	}
	if *action == "" {
		return fmt.Errorf("mouse-button requires --action")
	}

	// Resolve both names before constructing a command context, resolving
	// credentials, or connecting. The shared resolver accepts only the exact
	// public enum values and returns a wire-safe one-bit mask plus action.
	buttonMask, press, err := jetkvm.ResolveMouseButton(*button, *action)
	if err != nil {
		return fmt.Errorf("invalid mouse-button parameters: %w", err)
	}

	ctx, cancel := commandContext(cf.timeout)
	defer cancel()
	if err := sender(ctx, cf, buttonMask, press); err != nil {
		return err
	}
	return printJSON(map[string]any{
		"sent":   "mouse-button",
		"button": *button,
		"action": *action,
	})
}

func sendMouseButton(ctx context.Context, cf *commonFlags, buttonMask byte, press bool) (err error) {
	// Defend the production boundary if a future caller bypasses the CLI
	// resolver: reject zero, combined, or unknown masks before connecting.
	if err := jetkvm.ValidateMouseButton(buttonMask); err != nil {
		return fmt.Errorf("invalid mouse-button parameters: %w", err)
	}

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
	// Even a one-shot press must end in terminal neutralization when the CLI
	// session exits. Join an independent release failure with any send failure
	// so success is never printed unless both boundaries were confirmed.
	defer func() { err = errors.Join(err, held.Release()) }()

	buttons := byte(0)
	if press {
		buttons = buttonMask
	}
	return sendMouseButtonReport(func(dx, dy int8, buttons byte) error {
		return held.SendMouseReport(ctx, dx, dy, buttons)
	}, buttons)
}

// sendMouseButtonReport changes only the button state. A zero-delta relative
// report preserves the cursor position for both press and release actions.
func sendMouseButtonReport(send func(dx, dy int8, buttons byte) error, buttons byte) error {
	return send(0, 0, buttons)
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

	// Current firmware silently drops wheel reports on the binary hidrpc path,
	// so Client.Scroll uses the legacy wheelReport JSON-RPC method instead. It
	// still acquires and neutralizes the control lease internally, in addition
	// to the CLI's --allow-control gate above.
	return client.Scroll(ctx, dx, dy)
}

func runClick(args []string) error {
	fs := newCommandFlagSet("click")
	cf := addCommonFlags(fs, true)
	x := fs.Int("x", -1, "absolute X in [0,32767] (required)")
	y := fs.Int("y", -1, "absolute Y in [0,32767] (required)")
	button := fs.Int("button", 1, fmt.Sprintf("nonzero mouse button bitmask [1,%d] (default 1 = left)", jetkvm.MaxPointerButtonMask))
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
	if err := jetkvm.ValidatePointerGesture(*x, *y, *button); err != nil {
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
	if err := jetkvm.ValidatePointerGesture(int(x), int(y), int(button)); err != nil {
		return err
	}
	if err := send(x, y, button); err != nil {
		return err
	}
	return send(x, y, 0)
}

type doubleClickSender func(context.Context, *commonFlags, int32, int32, byte) error

func runDoubleClick(args []string) error {
	return runDoubleClickWithSender(args, sendDoubleClick)
}

// runDoubleClickWithSender keeps flag parsing, gating, full-width validation,
// and result rendering testable without opening a WebRTC session. Production
// uses sendDoubleClick, which owns the control lease for the complete gesture.
func runDoubleClickWithSender(args []string, sender doubleClickSender) error {
	fs := newCommandFlagSet("double-click")
	cf := addCommonFlags(fs, true)
	x := fs.Int("x", -1, "absolute X in [0,32767] (required)")
	y := fs.Int("y", -1, "absolute Y in [0,32767] (required)")
	button := fs.Int("button", 1, fmt.Sprintf("nonzero mouse button bitmask [1,%d] (default 1 = left)", jetkvm.MaxPointerButtonMask))
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if err := requirePositiveTimeout(cf); err != nil {
		return err
	}
	if !cf.allowControl {
		return fmt.Errorf("double-click requires --allow-control")
	}
	// Validate full-width CLI integers before narrowing them to HID wire types
	// or allowing the sender to open a device connection.
	if err := jetkvm.ValidatePointerGesture(*x, *y, *button); err != nil {
		return fmt.Errorf("invalid double-click: %w", err)
	}

	ctx, cancel := commandContext(cf.timeout)
	defer cancel()
	if err := sender(ctx, cf, int32(*x), int32(*y), byte(*button)); err != nil {
		return err
	}
	return printJSON(map[string]any{"sent": "double-click", "x": *x, "y": *y, "button": *button})
}

func sendDoubleClick(ctx context.Context, cf *commonFlags, x, y int32, button byte) error {
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
	return sendPointerDoubleClickAndRelease(
		func(x, y int32, buttons byte) error {
			return held.SendPointerReport(ctx, x, y, buttons)
		},
		held.Release,
		x,
		y,
		button,
	)
}

// sendPointerDoubleClick sends two complete clicks at one absolute
// coordinate, stopping immediately if any report is not confirmed.
func sendPointerDoubleClick(send func(x, y int32, buttons byte) error, x, y int32, button byte) error {
	if err := sendPointerClick(send, x, y, button); err != nil {
		return err
	}
	return sendPointerClick(send, x, y, button)
}

// sendPointerDoubleClickAndRelease makes terminal neutralization part of the
// gesture's success contract and retains both failures when sending and
// releasing independently fail.
func sendPointerDoubleClickAndRelease(
	send func(x, y int32, buttons byte) error,
	release func() error,
	x, y int32,
	button byte,
) error {
	return sendControlAndRelease(
		func() error { return sendPointerDoubleClick(send, x, y, button) },
		release,
	)
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
	button := fs.Int("button", 1, fmt.Sprintf("nonzero mouse button bitmask [1,%d] (default 1 = left)", jetkvm.MaxPointerButtonMask))
	steps := fs.Int("steps", 0, fmt.Sprintf("intermediate moves with requested button state [0,%d]", jetkvm.MaxDragSteps))
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
	if err := jetkvm.ValidatePointerGesture(*x1, *y1, *button); err != nil {
		return fmt.Errorf("invalid drag start: %w", err)
	}
	if err := jetkvm.ValidatePointerGesture(*x2, *y2, *button); err != nil {
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
	if err := jetkvm.ValidatePointerDragReports(reports); err != nil {
		return err
	}

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
	if err := jetkvm.ValidatePointerDragReports(reports); err != nil {
		return err
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
	// non-nil error means the canonical reports were not acknowledged by the
	// peer transport, which must surface as a failure rather than a reassuring
	// "sent".
	if err := held.Release(); err != nil {
		return err
	}
	return printJSON(releaseAllSuccessResult())
}

func releaseAllSuccessResult() map[string]any {
	return map[string]any{
		"sent":                      "release-all",
		"peerTransportAcknowledged": true,
	}
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

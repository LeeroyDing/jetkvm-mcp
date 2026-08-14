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
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/buildinfo"
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

func formatCLIError(err error) string {
	return "jetkvmctl: " + jetkvm.RedactError(err)
}

func printUsage(w *os.File) {
	fmt.Fprint(w, `jetkvmctl - browser-free JetKVM controller

Usage:
  jetkvmctl --version
  jetkvmctl doctor
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
  JETKVM_PASSWORD     env var: log in with this password
  JETKVM_AUTH_TOKEN   env var: use this already-valid session cookie directly
  --password-stdin    read a password from stdin (first line) instead.
                      Rejected by 'serve': the MCP protocol owns stdin.

Diagnosing a screenshot that never arrives:
  doctor              Check the local FFmpeg runtime without contacting a
                      device. Status and serve startup do not require FFmpeg.
  --diagnostics       Print a video-pipeline report to stderr naming the stage
                      the capture stopped at (negotiation, no RTP, no keyframe,
                      decode, ...). Counts, states and codec parameters only -
                      no addresses, credentials, SDP, ICE candidates or pixels.

A successful screenshot is captured after that request begins. If a strictly
newer frame does not arrive before the timeout, the command fails rather than
returning a cached image.

The MCP server holds no idle device connection. Each tool call connects one
fresh session, executes once, and closes it before returning.

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

func runVersion(args []string) error {
	fs := newCommandFlagSet("version")
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	return printJSON(buildinfo.Current())
}

func runDoctor(args []string) error {
	fs := newCommandFlagSet("doctor")
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	ctx, cancel := commandContext(5 * time.Second)
	defer cancel()
	if err := (&jetkvm.FFmpegDecoder{}).CheckAvailable(ctx); err != nil {
		return err
	}
	return printJSON(map[string]any{
		"ffmpeg":      "available",
		"screenshots": "ready",
		"status":      "does not require FFmpeg",
	})
}

// commonFlags are shared across every subcommand.
type commonFlags struct {
	url           string
	timeout       time.Duration
	allowControl  bool
	passwordStdin bool
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

// newCommandFlagSet keeps flag.Parse from writing raw, attacker-controlled
// argument text directly to stderr. Parse errors return to main and cross the
// same RedactError boundary as every other CLI failure.
func newCommandFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseCommandFlags collapses flag's raw, argument-reflecting diagnostics to a
// fixed message and rejects positional arguments uniformly. The standard flag
// errors quote invalid values; those values can be credential canaries or URLs
// and must not cross the CLI's public error boundary in the first place.
func parseCommandFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("invalid %s arguments", fs.Name())
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments for %s", fs.Name())
	}
	return nil
}

// requireURL validates that a device address was supplied, since there is
// deliberately no default.
func requireURL(cf *commonFlags) error {
	if strings.TrimSpace(cf.url) == "" {
		return fmt.Errorf("--url is required (or set $JETKVM_URL), e.g. --url http://jetkvm.local")
	}
	return nil
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
		"Pass credentials via the JETKVM_PASSWORD or JETKVM_AUTH_TOKEN environment variable instead")

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
	if pw := os.Getenv("JETKVM_PASSWORD"); pw != "" {
		creds.Password = jetkvm.NewSecret(pw)
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
	if err := requirePositiveTimeout(cf); err != nil {
		return nil, err
	}
	if err := requireURL(cf); err != nil {
		return nil, err
	}
	creds, err := credentialsFromEnv(cf)
	if err != nil {
		return nil, err
	}
	return jetkvm.Connect(ctx, jetkvm.Options{
		BaseURL:      cf.url,
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

const cliCloseTimeout = 3 * time.Second

// closeClient gives cleanup its own deadline. The operation context commonly
// expires precisely because a peer dropped or a frame stalled; reusing it
// would skip or truncate control neutralization at the moment it matters most.
func closeClient(client *jetkvm.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), cliCloseTimeout)
	defer cancel()
	return client.Close(ctx)
}

func runStatus(args []string) (err error) {
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
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, closeClient(client))
		}
	}()

	status, operationErr := client.Status(ctx)
	closeErr := closeClient(client)
	closed = true
	if err := errors.Join(operationErr, closeErr); err != nil {
		return err
	}

	out := map[string]any{
		"deviceId":        status.DeviceID,
		"firmwareVersion": status.FirmwareVersion,
		"rpcReachable":    status.RPCReachable,
	}
	return printJSON(out)
}

func runScreenshot(args []string) (err error) {
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
	if err := requireURL(cf); err != nil {
		return err
	}
	if _, err := jetkvm.CanonicalBaseURL(cf.url); err != nil {
		return err
	}

	ctx, cancel := commandContext(cf.timeout)
	defer cancel()
	if err := (&jetkvm.FFmpegDecoder{}).CheckAvailable(ctx); err != nil {
		return err
	}

	client, err := connectFromFlags(ctx, cf, false)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, closeClient(client))
		}
	}()

	shot, operationErr := client.SaveScreenshot(ctx, *output)

	// Emitted on success and failure alike: a successful capture's counts
	// are the baseline that makes a failing run's counts interpretable.
	// stderr, so the result JSON on stdout stays machine-readable.
	if *diagnostics {
		if reportErr := printDiagnostics(client.VideoDiagnostics()); reportErr != nil {
			fmt.Fprintf(os.Stderr, "jetkvmctl: writing diagnostics: %s\n", jetkvm.RedactError(reportErr))
		}
	}
	closeErr := closeClient(client)
	closed = true
	if err := errors.Join(operationErr, closeErr); err != nil {
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
	if err := requirePositiveTimeout(cf); err != nil {
		return err
	}

	// Reject the stdin conflict before anything reads a byte of stdin or
	// touches the network.
	if cf.passwordStdin {
		return errPasswordStdinWithServe
	}
	if err := requireURL(cf); err != nil {
		return err
	}

	ctx, cancel := rootContext()
	defer cancel()

	creds, err := credentialsFromEnv(cf)
	if err != nil {
		return err
	}

	return mcpserver.Run(ctx, mcpserver.Options{
		BaseURL:      cf.url,
		Credentials:  creds,
		AllowControl: cf.allowControl,
		HTTPTimeout:  cf.timeout,
	})
}

func runKeypress(args []string) (err error) {
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
	if err := jetkvm.ValidateKeypress(*key, *modifier); err != nil {
		return fmt.Errorf("invalid keypress: %w", err)
	}

	ctx, cancel := commandContext(cf.timeout)
	defer cancel()

	client, err := connectFromFlags(ctx, cf, true)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, closeClient(client))
		}
	}()

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
	sendErr := held.SendKeyboardReport(ctx, byte(*modifier), []byte{byte(*key)})
	releaseErr := held.Release()
	closeErr := closeClient(client)
	closed = true
	if err := errors.Join(sendErr, releaseErr, closeErr); err != nil {
		return err
	}
	return printJSON(map[string]any{"sent": "keypress", "key": *key, "modifier": *modifier})
}

func runMouseMove(args []string) (err error) {
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
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, closeClient(client))
		}
	}()

	lease, err := client.Control()
	if err != nil {
		return err
	}
	held, err := lease.Acquire(ctx, jetkvm.DefaultControlLeaseTimeout)
	if err != nil {
		return err
	}
	sendErr := held.SendPointerReport(ctx, int32(*x), int32(*y), byte(*buttons))
	releaseErr := held.Release()
	closeErr := closeClient(client)
	closed = true
	if err := errors.Join(sendErr, releaseErr, closeErr); err != nil {
		return err
	}
	return printJSON(map[string]any{"sent": "mouse-move", "x": *x, "y": *y, "buttons": *buttons})
}

func runReleaseAll(args []string) (err error) {
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
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, closeClient(client))
		}
	}()

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
	releaseErr := held.Release()
	closeErr := closeClient(client)
	closed = true
	if err := errors.Join(releaseErr, closeErr); err != nil {
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

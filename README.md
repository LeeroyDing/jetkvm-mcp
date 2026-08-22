# jetkvmctl

A native, browser-free controller for a [JetKVM](https://jetkvm.com) device: one Go binary with a CLI and an MCP
stdio server, so an agent (or a human without a browser handy) can check a JetKVM's status, inspect its display,
and wait for the screen to settle or show expected text without opening the web UI.

This is a v1 compatibility spike, not a full remote-control client. It deliberately implements a narrow slice of
JetKVM's protocol - enough for read-only inspection, plus opt-in, heavily gated keyboard/mouse paths - and says
so explicitly where it's relying on undocumented behavior. See [Firmware compatibility](#firmware-compatibility)
and [Limitations](#limitations) before depending on it.

## What it does

- `status` - authenticates, opens a WebRTC session, and confirms the RPC data channel responds.
- `screenshot` - receives the live H.264 video track and decodes one fresh frame via FFmpeg. The CLI saves a PNG;
  MCP returns an in-memory PNG or JPEG with optional crop and down-scale controls.
- `read-text` - captures a fresh frame, optionally crops/down-scales it, and returns Tesseract OCR as plain text
  without returning or writing the image.
- `wait-stable` - polls successive fresh decoded frames until the changed-pixel fraction stays at or below a threshold,
  providing read-only readiness gating before an agent acts.
- `wait-for-text` - polls fresh frames with Tesseract OCR until a literal substring or regular expression appears,
  providing a read-only content readiness gate with a structured timeout result.
- `serve` - the same functionality as an MCP server over stdio, for an agent to call as tools.
- `--version` - prints JSON build provenance from the same version source used by MCP `serverInfo`.
- `doctor` - reports local build, bundle, signing, configuration, FFmpeg, and Keychain-presence diagnostics;
  a device probe is opt-in.
- Keyboard, pointer, and scroll control - implemented and unit-tested, but only reachable behind
  `--allow-control`, and never exercised against a live device by this project's own test suite (see
  [Security model](#security-model)).

> [!WARNING]
> **Affected JetKVM firmware serves its local web API and signaling WebSocket over plaintext HTTP.** Your device
> password and session cookie travel the LAN unencrypted, and anyone able to observe that network can read them
> and authenticate as you. This client cannot upgrade the device's transport - only the firmware can. Use a
> trusted, isolated network segment or a VPN, and pick a device password that protects nothing else. See
> [SECURITY.md](SECURITY.md#the-single-most-important-warning-plaintext-credentials-on-the-lan).

## Quickstart

This README tracks the code on `main`. The published `v0.4.0` tag is the current release, while the expanded
MCP catalog documented below landed on `main` after that tag and remains under [Unreleased](CHANGELOG.md#unreleased).
Install the tag when you specifically want the v0.4.0 release; build a current source checkout when you need the
complete seventeen-tool surface described here.

### Install / build

Requires `ffmpeg` on `PATH` for H.264 frame decoding. The optional `read-text` and `wait-for-text` commands/tools
also detect `tesseract` on `PATH` at runtime; when it is absent, OCR fails before any device connection while the
other tools remain usable. Install both on macOS with `brew install ffmpeg tesseract`, or use your Linux
distribution's FFmpeg and Tesseract packages (for example, `ffmpeg` and `tesseract-ocr` on Debian/Ubuntu).

No OCR library, native binding, trained-data bundle, or platform-specific executable is vendored. Common Go
bindings still require Tesseract/Leptonica through CGO, while shipping OCR models would substantially increase
the release size and update surface. Runtime detection keeps the normal Go build unchanged and makes the local
dependency explicit only for the features that need it.

`go.mod` requires Go 1.26 or newer. `.go-version` records the canonical CI compiler (Go 1.26.6), and CI runs
native tests on `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64` runners. Locally built and
tested with Go 1.26.6 on darwin/arm64.

Build from a source checkout:

```sh
git clone https://github.com/LeeroyDing/jetkvm-mcp.git
cd jetkvm-mcp
go build -o jetkvmctl ./cmd/jetkvmctl
./jetkvmctl --version
```

Or install straight from the module path (pin a tag rather than tracking `latest` blindly):

```sh
go install github.com/leeroyding/jetkvm-mcp/cmd/jetkvmctl@v0.4.0
```

The v0.4.0 release provides reproducibly built archives for `darwin`/`linux` on `amd64`/`arm64` — see
[Verifying a release](#verifying-a-release) before running a downloaded binary.

After installing, `jetkvmctl --version` prints build provenance and `jetkvmctl doctor` checks the local
environment (FFmpeg, configuration, Keychain presence) without contacting any device.

### First read-only check

Set the device address, configure one of the credential mechanisms described below if the device requires a
password, then start with read-only operations:

```sh
export JETKVM_URL=http://jetkvm.local     # or http://<your-device-ip>

jetkvmctl status
jetkvmctl screenshot --output /tmp/shot.png
jetkvmctl read-text
jetkvmctl wait-stable
jetkvmctl wait-for-text --text 'login:'
```

## CLI

```
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
```

The synopsis omits common flags for readability. Every device-facing command accepts `--timeout` (default
`10s`). `status`, `screenshot`, `read-text`, `wait-stable`, `wait-for-text`, and every CLI control command also
accept `--password-stdin`; `doctor` does not, and `serve` rejects it because MCP owns stdin.

`jetkvmctl key-combo` and the `jetkvm_key_combo` MCP tool send a named keyboard chord of at most 64 runes as one
HID report, then release it. Built-in names are `ctrl+alt+del`, `cmd+space` (meta+space), `alt+tab`, `ctrl+c`,
`ctrl+v`, `ctrl+z`,
`ctrl+shift+t`, `win`, `cmd`, `esc`, `enter`, and the bare keys `e`, `m`, `r`, and `t`. Names are
case-insensitive, and plus signs, hyphens, and whitespace are interchangeable separators. Both interfaces
require `--allow-control`.

`jetkvmctl hold-key` and the `jetkvm_hold_key` MCP tool press one of the same named chords, hold it for the
required duration, then release it. Durations must be between 1 and 5000 milliseconds. The chord and duration
are fully validated before any HID call; cancellation or timeout ends the hold early and
still runs release-all through the control lease's independent safety context. Both interfaces require
`--allow-control`.

`jetkvmctl key-sequence` sends between 1 and 64 named chords in the order of repeated `--combo` flags. Its
optional `--delay-ms` is the pause between chords, from 0 through 500 milliseconds (default 0). The complete
sequence is resolved and validated before the device connection or first HID send, so an invalid later entry
cannot cause an earlier chord to be sent. Each chord follows the same send-and-release path as `key-combo` and is
released before the delay and next chord. The command also requires `--allow-control`.

### Version and diagnostics

`--version` prints the semantic version, source commit, build date, Go version, and target platform as JSON.
Release builds inject the commit and build date; an ordinary local build may report an embedded VCS revision and
an unknown build date.

`doctor` is local and read-only by default. It reports configuration presence rather than values, checks a
configured macOS Keychain item by attributes without reading its secret or prompting, and does not contact the
device. Add `--probe-device` to opt in to one connect-and-status call; that probe uses `--url`/`JETKVM_URL`, the
configured Keychain item, `JETKVM_PASSWORD`, or `JETKVM_AUTH_TOKEN`, plus the command timeout. `doctor` does not
accept `--password-stdin`.

For device commands — and for `doctor --probe-device` — `--url` is required, or set `$JETKVM_URL`. There is
deliberately no built-in default: a device address belongs to your network, not to this tool. The value must be
an `http` or `https` origin; userinfo, query strings, fragments, and non-root paths are rejected before credential
resolution or network I/O.

### Credentials

Credentials are never accepted as flags, so they do not appear in the process arguments shown by `ps`. On macOS,
the preferred mechanism reads a generic-password item directly from the local Keychain with
`/usr/bin/security`; it does not contact an external secret provider. A literal environment assignment typed at
a shell can still enter shell history: use Keychain, load it through your secret manager, or use
`--password-stdin` for device commands that accept it. Treat the process environment itself as sensitive.

| Mechanism | Effect |
|---|---|
| `JETKVM_PASSWORD_KEYCHAIN_SERVICE` + `JETKVM_PASSWORD_KEYCHAIN_ACCOUNT` | Reads that macOS Keychain generic-password item first, using `/usr/bin/security find-generic-password` |
| `JETKVM_PASSWORD` env var | Fallback when no Keychain item is configured or its lookup fails for a non-cancellation reason; logs in via the device's real `/auth/login-local` flow |
| `JETKVM_AUTH_TOKEN` env var | Uses an already-valid session cookie directly, skipping login |
| `--password-stdin` | Explicitly reads a password from the first line of stdin and overrides configured token, Keychain, and password environment sources for that device command |

If the device is in "noPassword" mode, none of these are needed.

Create a dedicated generic-password item, then configure only its non-secret identifiers in the MCP process:

```sh
/usr/bin/security add-generic-password -U -s jetkvmctl -a jetkvm.local -w
export JETKVM_PASSWORD_KEYCHAIN_SERVICE=jetkvmctl
export JETKVM_PASSWORD_KEYCHAIN_ACCOUNT=jetkvm.local
```

The `-w` prompt above is handled by the native Keychain tool; do not put the password itself on the command line.
Both configuration variables are required. Keychain wins when it returns one well-formed password;
`JETKVM_PASSWORD` remains a migration fallback for a missing item, a denied lookup, or malformed tool output,
but it never overrides cancellation or the command deadline.
If no fallback exists, lookup/configuration failures are reported without printing command output or the secret.
An explicit `JETKVM_AUTH_TOKEN` skips password lookup entirely.

`--password-stdin` is accepted by `status`, `screenshot`, `read-text`, `wait-stable`, `wait-for-text`, `keypress`,
`type`, `key-combo`, `hold-key`, `key-sequence`, `mouse-button`, `mouse-move`, `click`, `double-click`, `scroll`,
`drag`, and `release-all`. Because it is an explicit per-command choice, it takes precedence over
`JETKVM_AUTH_TOKEN`, Keychain configuration, and `JETKVM_PASSWORD`; those sources are not consulted. It is not a
`doctor` option, and is **rejected by `serve`**: the MCP protocol owns stdin, and reading a password line from it
would consume the client's first JSON-RPC message. Use the environment variables for MCP and device probes.

## MCP configuration

`jetkvmctl serve` is a standard **stdio MCP server**: any MCP client that can launch a subprocess and speak
JSON-RPC over stdin/stdout can use it. The generic recipe is: command = the `jetkvmctl` binary, args =
`["serve", "--url", "http://<device>"]`, plus credential environment variables.

`jetkvmctl` has no separate runtime config file. `serve` validates the URL and resolves its credential source once
at startup, but does not contact the device until the first tool call. Restart the process after rotating a
password, token, or Keychain selection.

For clients that use the common `mcpServers` JSON shape (Claude Desktop, Claude Code, and most compatible
clients), add this to the client's server config (adjust the path to the built binary):

```json
{
  "mcpServers": {
    "jetkvm": {
      "command": "/path/to/jetkvmctl",
      "args": ["serve", "--url", "http://jetkvm.local"],
      "env": {
        "JETKVM_PASSWORD_KEYCHAIN_SERVICE": "jetkvmctl",
        "JETKVM_PASSWORD_KEYCHAIN_ACCOUNT": "jetkvm.local"
      }
    }
  }
}
```

The service and account strings identify the Keychain item; neither is the password. Restrict access to the MCP
configuration anyway because it also contains the device address and may later carry other sensitive settings.

Add `"--allow-control"` to `args` only if you accept exposing the full opt-in catalog: the read-only
`jetkvm_wait_stable` and `jetkvm_wait_for_text` tools plus dangerous keyboard/mouse input tools. See
[Security model](#security-model) first.

### MCP tools

Every input is a strict JSON object. “Required” and “optional” below refer to object properties; `{}` is the
complete argument object for a no-argument tool.

Read-only catalog:

| Tool | Exact arguments | Result |
|---|---|---|
| `jetkvm_status` | `{}` | Device ID, firmware version, and RPC reachability |
| `jetkvm_screenshot` | Optional `format`: `"png"` (default) or `"jpeg"`; `quality`: integer 1–100 (JPEG only, default 80); `scale`: positive finite number (default 1, values above 1 clamp to 1); `region`: `{x,y,width,height}` source-pixel rectangle | One request-fresh PNG or JPEG in the response, optionally cropped/down-scaled, with truthful MIME and final/source dimensions; no filesystem write |
| `jetkvm_read_text` | Optional `scale`: positive finite number (default 1, values above 1 clamp to 1); `region`: `{x,y,width,height}` source-pixel rectangle | Plain text recognized from one request-fresh frame; no image content or filesystem write |

Opt-in catalog — all fourteen additional tools are registered only with `--allow-control`:

| Tool | Exact arguments | Result or action |
|---|---|---|
| `jetkvm_wait_stable` | Optional `threshold`: finite number 0–1 (default 0.01); `stable_frames`: integer 1–2,147,483,647 (default 2); `poll_interval_ms`: integer 0–9,223,372,036,854 (default 250) | Read-only; compares successive fresh frames and returns settling state, frames sampled, final changed-pixel fraction, and elapsed time |
| `jetkvm_wait_for_text` | Required `text`: non-empty string up to 4,096 runes; optional `regex`: boolean (default false); `interval_ms`: integer 100–10,000 (default 500); `timeout_ms`: integer 100–300,000 (default 10,000), with interval no greater than timeout | Read-only; polls request-fresh frames with OCR and returns whether and what matched, whether the wait timed out, elapsed time, and frame count |
| `jetkvm_release_all` | `{}` | **Dangerous** — sends canonical neutral reports for every input interface the session may have left holding state, using zero relative deltas and the last recorded absolute coordinates; success proves peer-SCTP acknowledgement, not firmware USB application or host action |
| `jetkvm_keypress` | Required `key`: integer 0–255; optional `modifier`: integer 0–255 (default 0) | **Dangerous** — sends one live USB HID key usage |
| `jetkvm_type` | Required `text`: string of at most 4,096 runes; optional `delay_ms`: integer 0–500 (default 0) | **Dangerous** — types printable ASCII, newline, and tab using a US layout |
| `jetkvm_key_combo` | Required `combo`: one supported named chord, at most 64 runes | **Dangerous** — sends the chord in one keyboard report, then releases it |
| `jetkvm_hold_key` | Required `combo`: one supported named chord, at most 64 runes; required `hold_ms`: integer 1–5,000 | **Dangerous** — presses the chord in one keyboard report, holds it for the requested duration, then releases it; cancellation or timeout still triggers a release attempt |
| `jetkvm_key_sequence` | Required `combos`: array of 1–64 supported named chords, each at most 64 runes; optional `delay_ms`: integer 0–500 (default 0) | **Dangerous** — sends an ordered, fully prevalidated sequence, releasing each chord before the delay and next chord |
| `jetkvm_mouse_button` | Required `button`: exactly `"left"`, `"right"`, or `"middle"`; required `action`: exactly `"press"` or `"release"` | **Dangerous** — changes one tracked button without moving the cursor, allowing custom held-button gestures across calls |
| `jetkvm_mouse_move` | Required `x`, `y`: integers 0–32,767; optional `buttons`: integer 0–31 (default 0) | **Dangerous** — sends an absolute pointer/button state |
| `jetkvm_click` | Required `x`, `y`: integers 0–32,767; optional `button`: integer 0–31 (default 1 = left) | **Dangerous** — moves, then sends the requested button state and a zero-button state; `button=0` only moves the pointer |
| `jetkvm_double_click` | Required `x`, `y`: integers 0–32,767; optional `button`: integer 0–31 (default 1 = left) | **Dangerous** — moves, then sends two immediate requested-button/zero-button cycles; `button=0` only moves the pointer, and there is no delay parameter |
| `jetkvm_scroll` | Required `dy`: integer −127–127; optional `dx`: integer −127–127 (default 0); the two axes cannot both be zero | **Dangerous** — positive `dy` scrolls up; positive `dx` scrolls right |
| `jetkvm_drag` | Required `x1`, `y1`, `x2`, `y2`: integers 0–32,767; optional `button`: integer 0–31 (default 1); optional `steps`: integer 0–256 (default 0) | **Dangerous** — sends the requested button state, moves directly or through optional steps, then sends a zero-button state; `button=0` is a stepped pointer move without a held button, and there is no duration/delay parameter |

When the server is started without `--allow-control`, it registers **exactly three tools**: `jetkvm_status`,
`jetkvm_screenshot`, and `jetkvm_read_text`. Every opt-in tool, including the read-only `jetkvm_wait_stable` and
`jetkvm_wait_for_text` readiness gates and `jetkvm_release_all`, is not merely refused - it is never registered,
so it doesn't appear in `tools/list` at all. With control enabled, the catalog contains exactly seventeen tools.

`jetkvm_wait_stable` is read-only but is advertised by MCP only with `--allow-control`. It accepts an optional
changed-pixel `threshold` from 0.0 through 1.0
(default 0.01), `stable_frames` from 1 through 2,147,483,647 consecutive stable comparisons (default 2), and a non-negative
`poll_interval_ms` between fresh-frame polls (default 250). A resolution change always counts as unstable, even
when the threshold is 1.0. The call compares only successive request-fresh decoded frames and remains bounded by
the caller deadline or the server's `--timeout` (default 10s). The matching CLI command uses `--threshold`,
`--stable-frames`, and a Go duration such as `250ms` for `--poll-interval`; unlike the MCP catalog entry, the
CLI command does not require or accept `--allow-control`.

`jetkvm_wait_for_text` is also read-only but is advertised by MCP only with `--allow-control`. It requires a
non-empty `text` value of at most 4,096 runes and treats it as a case-sensitive literal substring by default; set
`regex: true` to use Go regular-expression syntax.
`interval_ms` is bounded from 100 through 10,000 milliseconds (default 500), and `timeout_ms` from 100 through
300,000 milliseconds (default 10,000); the interval cannot exceed the timeout. Each poll captures a
request-fresh screenshot and runs the locally installed Tesseract engine. A match returns the recognized match,
elapsed time, and frame count. Reaching the deadline is a successful, structured response with `timedOut: true`,
not a tool error once polling has begun. The CLI uses the same contract via `--text`, `--regex`, `--interval` (a
Go duration such as `500ms`), and the common `--timeout` flag (default `10s`, maximum `5m`). That CLI timeout
also bounds URL validation, dependency preflight, and connection setup, so expiry before polling starts remains
an operation error. Both FFmpeg and Tesseract are checked before the command opens a device connection. As with
`wait-stable`, the CLI command does not require or accept `--allow-control`.

`jetkvm_drag` requires start coordinates `x1`, `y1` and destination coordinates `x2`, `y2`. Its optional `button`
bitmask defaults to 1 (left), and `steps` selects from 0 through 256 intermediate moves made with that button
state. The default `steps=0` sends a direct move from the start to the destination. Every drag ends with a
zero-button report at the destination. An explicit `button=0` is accepted as a stepped pointer move without a
held button. The CLI `drag` command uses the same bounds, defaults, and zero-button behavior.

`jetkvm_mouse_button` changes one named button as a discrete action without moving the cursor. Names and actions
are exact lowercase enums: `left`, `right`, or `middle`, and `press` or `release`. MCP presses are combined with
other buttons held through this tool, so an agent can press, move, and release in separate calls to compose a
custom gesture such as a right-button drag. `jetkvm_release_all`, control-lease watchdog expiry, device-session
teardown, and MCP server shutdown attempt canonical neutralization; each tracked interface is cleared locally only
after the peer SCTP transport acknowledges its neutral report. The matching `jetkvmctl mouse-button` command sends
the same zero-delta report, but its one-shot device session neutralizes before exit; use the long-lived MCP server
for cross-call holds.

`jetkvm_type` requires `text` and accepts an optional `delay_ms` from 0 through 500 (default 0) between keys. It
supports every printable ASCII character on a US keyboard, plus newline (Enter) and tab, with a maximum of 4096
runes per call. The complete string is mapped and validated before typing starts: an unsupported rune is reported
with its position and nothing from that call is sent. Each character follows the same control-lease,
generation-token, and neutralization path as `jetkvm_keypress`, so each key's neutral reports are
SCTP-acknowledged before the next is sent; a failed confirmation stops the call.
The CLI `type` command uses the same layout, limits, and per-key neutralization behavior.

`jetkvm_key_sequence` requires an ordered `combos` array containing from 1 through 64 named chords and accepts
optional `delay_ms` from 0 through 500 (default 0) between chords. The entire array is resolved and validated
before the first HID call; an invalid entry is reported by array index without echoing its raw value, and nothing
from that call is sent. Every chord uses the existing `jetkvm_key_combo` path, including transport-confirmed
neutral reports before the delay and next chord. The CLI `key-sequence` command provides the same contract through
repeatable `--combo` flags and `--delay-ms`; both surfaces are available only with `--allow-control`.

`jetkvm_screenshot` accepts a strict object with these optional fields:

- `format`: `"png"` (the default) or `"jpeg"`.
- `quality`: a JPEG quality integer from 1 through 100 (default 80). It is invalid for PNG output.
- `scale`: any positive finite number. Its effective value is capped at 1, so values above 1 never enlarge the
  image. The cropped dimensions are multiplied by the effective scale, rounded to the nearest whole pixel, and
  clamped to a minimum of one pixel per axis.
- `region`: a strict source-pixel rectangle `{x, y, width, height}`. All four integer fields are required; `x`
  and `y` must be at least 0, `width` and `height` must be at least 1, and the complete rectangle must lie inside
  the captured frame. Each integer is capped at 2,147,483,647 so oversized JSON numbers are rejected before
  typed decoding. Cropping happens before scaling.

Example MCP argument objects:

| Result | Arguments |
|---|---|
| Default request-fresh PNG | `{}` |
| JPEG at explicit quality | `{"format":"jpeg","quality":80}` |
| Half-size full frame | `{"scale":0.5}` |
| Crop source pixels, then halve the crop | `{"region":{"x":100,"y":50,"width":800,"height":600},"scale":0.5}` |

The returned `image/png` or `image/jpeg` MIME type always matches the encoded bytes. `width` and `height` report
the final delivered image, while `sourceWidth` and `sourceHeight` report the original capture (and differ after
cropping or effective down-scaling). `capturedAt` and `fresh` continue to describe that request-fresh source
frame. `{}` therefore retains the previous fresh PNG behavior exactly. All MCP-side crop, resize, and encoding
work happens in memory: the tool accepts no output path and never writes the result to the filesystem. The
earlier output-path form was removed because it gave an MCP caller an arbitrary-file-overwrite primitive on the
server host.

`jetkvm_read_text` accepts the same optional `scale` and `region` fields, with the same validation,
crop-before-scale ordering, and no-upscale rule. It captures one strictly request-fresh frame, transforms the
PNG entirely in memory, and passes those bytes to the configured `OCREngine`. The default engine detects the
external `tesseract` executable at runtime and invokes it with fixed stdin/stdout arguments; caller input never
becomes a command-line argument or filename. The result content is only the recognized plain text, not an image.
A successful empty string means the engine found no text. A missing executable produces a typed, actionable
error before the device is contacted; an engine that later fails to recognize the frame produces a tool error,
never an empty success.

The matching CLI command uses `--scale` and an optional comma-separated `--region X,Y,WIDTH,HEIGHT`, then writes
the recognized text directly to stdout. Neither interface accepts `--allow-control`, acquires a control lease,
or writes the captured frame to disk.

`jetkvm_scroll` requires `dy` and accepts optional `dx` (default 0). Both are semantic wheel deltas in
`[-127,127]`, the signed range in the device's HID descriptors; positive `dy` scrolls up and positive `dx` scrolls
right, and at least one axis must be non-zero. The CLI `scroll` command uses the same bounds and directions. The
handler rejects invalid values before they can be narrowed to the firmware's signed-byte inputs.

All tool schemas are strict: unknown fields (including unknown `region` fields), wrong types, missing required
fields, and schema-declared numeric bounds are rejected as `InvalidParams` rather than silently ignored. Semantic
checks that depend on argument combinations or captured-frame dimensions return a redacted tool error instead:
examples include PNG plus `quality`, zero/zero scroll, an unknown combo, an unsupported typing rune, or a crop
outside the fresh frame. Screenshot and OCR `scale` values above 1 are clamped; other out-of-range values are
rejected.

### MCP call reliability

The MCP process starts without requiring the device to be online; its first tool call opens the device session.
For a transient `unreachable` failure, one read-only call may make at most **three total connection/operation
attempts**, with jittered exponential backoff starting at 75ms and capped at 300ms. The caller's deadline (or the
server's `--timeout`, default 10s) is the outer bound: a retry whose backoff cannot fit is not started. There are
no background reconnect loops, process respawns, or unbounded waits.

Authentication failures, timeouts, and malformed/oversized protocol frames are not retried. Keyboard, pointer,
scroll, and release operations may retry connection establishment before sending anything, but are never repeated
after an operation starts because delivery could be ambiguous. Device transport, authentication, timeout, and
protocol errors begin with one stable category: `auth-failed`, `unreachable`, `timeout`, or `bad-frame`.

Wire input is bounded before parsing: an RPC data-channel frame larger than 64 KiB is rejected as `bad-frame`,
and an HTTP response body is never read past 1 MiB — an oversized success body is rejected as `bad-frame`,
while an oversized error body is truncated so the HTTP status taxonomy (such as a 401 auth failure) still
surfaces. Video is bounded independently of its compressed size: H.264 reassembly stays below 4 MiB, FFmpeg
rejects frames above 16,777,216 decoded pixels, and the Go image path independently rejects dimensions above that
total or 8,192 pixels per axis. Encoded screenshot buffers stop at 66 MiB. A misbehaving device or interposed peer
cannot make the client allocate without limit.

Outbound application data is bounded before Pion accepts it. HID uses a 4 KiB cap with 22 bytes reserved for the
complete neutral set: an 8-byte keyboard report, a 4-byte relative-mouse report, and, when needed, a 10-byte
absolute-pointer report. RPC uses a 64 KiB cap over the current buffered amount plus the next frame. A frame that
would exceed either limit is rejected as not sent instead of entering Pion's otherwise-unbounded SCTP pending
queue. The existing 16-slot HID application queue remains unchanged.

## Verifying a release

The v0.4.0 release — and future releases produced by the [release workflow](.github/workflows/release.yml) —
builds each platform binary **twice** and requires the builds to be bit-identical. The workflow checks the
archives against `SHA256SUMS`, secret-scans the payloads, and gives the validated assets a GitHub **build
provenance attestation** before attaching them to a draft release. You can independently verify both properties
on a downloaded asset.

Check the checksums (`SHA256SUMS` covers `BUILDINFO` and all four archives):

```sh
shasum -a 256 --check --ignore-missing SHA256SUMS   # macOS
sha256sum --check --ignore-missing SHA256SUMS       # Linux
```

Verify the provenance attestation with the GitHub CLI - this proves the exact artifact was built by this
repository's release workflow on GitHub-hosted runners, not assembled elsewhere:

```sh
gh attestation verify jetkvmctl_0.4.0_darwin_arm64.tar.gz --repo LeeroyDing/jetkvm-mcp
```

Then confirm the embedded provenance matches after extracting: `./jetkvmctl --version` reports the version,
source commit, and build date that `BUILDINFO` records for that release.

## Security model

See [SECURITY.md](SECURITY.md) for the full trust boundary, the plaintext-LAN warning, and the rationale behind
each gate. Summary: this tool can see and (if opted in) control whatever is plugged into the JetKVM. Successful
screenshot results are captured after that request begins. The compatibility `fresh` field is always
`true` on success; if a strictly newer frame does not arrive before the deadline, the call fails rather than
returning a cached image. Stable-screen waits likewise compare only successive fresh frames and send no HID
input.

Every keyboard, pointer/button, and scroll operation flows through one process-local exclusive control lease. A
holder has a fixed 30-second watchdog from acquisition — it is not renewed by activity. Ordinary operations also bind the holder to
the caller or MCP operation deadline (default 10 seconds). A successful MCP `jetkvm_mouse_button` press is the
explicit exception: its retained holder outlives that completed call so later calls can compose a gesture, but the
same 30-second watchdog, a matching release, `jetkvm_release_all`, or session shutdown still ends it.
Neutralization gets its own two-second cleanup budget. What the lease actually proves, and what the tests pin down:

- Each holder gets a fresh, never-reused generation token, re-validated at the last moment before any frame is
  written. Input authorized by a lease that has since ended is **dropped**, and the caller is told - never
  delivered late.
- However a lease ends (release, cancellation, watchdog expiry, disconnect, shutdown), the generation is
  revoked first. Neutral reports then jump ahead of input still in the application queue. Bytes already accepted
  by Pion cannot be pre-empted, but the ordered channel places neutralization after them, making it the last HID
  data sent for that generation.
- Pion's outbound HID application-byte count is capped at 4 KiB, with 22 bytes reserved for the complete
  three-report neutral set. A report that would exceed its limit fails before `Send` instead of growing SCTP's
  pending queue.
- The canonical neutralization plan always includes an all-zero keyboard report and a zero-delta relative-mouse
  report. When the absolute interface may hold a button, the plan also includes a zero-button absolute-pointer
  report at the last recorded coordinates rather than at an arbitrary position.
- Release success requires every report in the applicable neutralization plan to be accepted and Pion's outbound
  amount to reach zero within the cleanup deadline. With the pinned Pion/SCTP versions, zero means the peer SCTP
  transport acknowledged all queued bytes, including the canonical neutral reports. If that cannot be confirmed,
  the call reports an error and retains the prior held-state uncertainty.

The lease does not coordinate another `jetkvmctl` process, MCP server, or the browser UI. In the MCP adapter,
one additional non-blocking admission token covers each complete dangerous tool call, including every phase and
inter-key delay. A concurrent dangerous call receives a busy error instead of waiting or interleaving; read-only
tools remain callable. Below that boundary, `jetkvm_drag` keeps one lease for its complete multi-report gesture;
`jetkvm_hold_key` keeps one lease for its complete press-hold-release interval; `jetkvm_type` acquires and
neutralizes per character; click and double-click phases are individually leased and neutralized; and
`jetkvm_mouse_button` retains one bounded lease only while its tracked aggregate button mask is non-zero. If
hold-key runs while that retained mouse generation is live, it reuses the same holder and releases only keyboard
state on success, preserving the explicitly held buttons. A hold failure or cancellation terminally neutralizes
the whole generation with an independent cleanup context.

Scroll is the one transport exception. The firmware defines `TypeWheelReport`, but its `hidrpc` handler drops that
message type, so `jetkvm_scroll` intentionally uses the legacy JSON-RPC `wheelReport` method instead. It remains
control-gated at every public boundary and now acquires the same lease non-blockingly before the RPC, which makes
HID readiness and process-local exclusivity structural and blocks scroll during a retained button gesture. The
lease is neutralized on exit. Calls require a matching RPC acknowledgement and are never retried after the
operation starts. The RPC frame itself cannot carry the lease generation token, so device-side send-boundary
validation remains unavailable for this compatibility path.

For HID release, this client can prove that the peer SCTP transport acknowledged the canonical neutral reports for
every input interface the session may have left holding state; for scroll, it can prove that the firmware
acknowledged the RPC.
Neither proves that the firmware applied HID state to USB or that the attached computer acted on the input.

## Architecture

```
cmd/jetkvmctl/          CLI adapter (thin: flag parsing -> internal/jetkvm -> print result)
internal/buildinfo/     Single version/provenance source for CLI, MCP, and release builds
internal/mcpserver/     MCP stdio adapter (thin: tool registration -> internal/jetkvm -> tool result)
internal/jetkvm/        session-owning core plus replaceable decoder/OCR subprocess adapters
internal/hidproto/      HID-RPC wire format (encode/decode only, no transport)
test/integration/       read-only live integration test (build-tag gated, off by default)
```

Both adapters delegate device protocol operations to one `jetkvm.Client`. There is exactly one session owner, so
CLI and MCP share connection, authentication, validation, gating, and neutralization behavior while retaining
their own argument and result adapters.

## How it talks to the device

No browser, no Playwright/Chromium, no `/dev/hidg*` writes. Just HTTP + a raw WebSocket + [Pion
WebRTC](https://github.com/pion/webrtc):

1. **Auth**: `GET /device/status` (public) to check API reachability, then `POST /auth/login-local` with a password
   (or reuse a supplied session cookie) to get an `authToken` cookie, followed by authenticated `GET /device` —
   the same local flow the web UI uses. A device in `noPassword` mode needs no credential.
2. **Signaling**: `GET /webrtc/signaling/client` upgrades to a WebSocket. The device immediately sends a
   `device-metadata` message with its firmware version - this client checks that message's shape before doing
   anything else (see [Firmware compatibility](#firmware-compatibility)). We then send an SDP offer as
   `{"type":"offer","data":{"sd":base64(json(offer))}}` and get back `{"type":"answer","data":"<b64 sdp>"}`, with
   ICE candidates trickled both directions as `new-ice-candidate` messages.
3. **WebRTC**: a `recvonly` video transceiver (this client structurally cannot send video/audio - it never offers
   to), plus a `"rpc"` data channel (JSON-RPC 2.0, including `ping` and the explicitly control-gated legacy
   `wheelReport`) and, only if `--allow-control` was passed, a `"hidrpc"` data channel (binary keyboard/pointer
   framing).
4. **Video**: H.264 over RTP, depacketized with Pion's codec support, reassembled into access units, then held
   until a self-contained SPS+PPS+IDR frame arrives; its raw Annex-B bytes go to a replaceable `Decoder`
   interface (currently backed by shelling out to `ffmpeg`, in a subprocess with an allowlisted environment) to
   get a decoded image, which is then PNG-encoded in memory. Keyframes are the device's to give: its encoder
   sets the GOP to half the source framerate, so an IDR arrives roughly every half second unprompted, and there
   is no keyframe-request API in the firmware's video component. This client does send a PLI (Picture Loss
   Indication) when the track starts, and periodically after, because that is correct receiver behavior - but on
   this firmware it changes nothing: `drainRTCP` reads inbound RTCP into a scratch buffer and discards it
   unparsed. The reassembly window is sized from the encoder's own output-buffer bound rather than a default, so
   the documented encoder output bound plus explicit headroom is covered; see `internal/jetkvm/h264.go`.
5. **OCR (on demand)**: both text tools send in-memory PNG data through one `OCREngine` interface. `read-text`
   performs one recognition pass and returns its UTF-8 text; `wait-for-text` repeats recognition on fresh frames
   and returns match metadata. The default engine runtime-detects Tesseract and uses fixed arguments with bounded
   subprocess I/O. Tesseract is optional for every non-OCR operation.

## Firmware compatibility

This client's protocol assumptions were read directly from the
[jetkvm/kvm](https://github.com/jetkvm/kvm) source, `dev` branch, commit `b3c29a4`, cloned and read 2026-08-02.
**None of this is a published or versioned protocol** - `jetkvm/kvm` ships a server and a reference browser
client, not a spec, and the wire format can change in any future commit without notice.

To keep that risk contained:

- The very first signaling message is checked to actually be `{"type":"device-metadata",...}` with a non-empty
  `deviceVersion` field before anything else happens. If it isn't, you get an actionable
  `CompatibilityError` naming the exact commit this client was built against, not a generic timeout or a panic
  three steps later.
- Protocol-parsing code (`internal/hidproto`, the signaling/session/RPC layers in `internal/jetkvm`) is isolated
  behind narrow interfaces - notably `Decoder` for video decode and the HID wire-format package - specifically so
  a firmware change can be absorbed by replacing one piece rather than the whole client.
- The video offer advertises **only** H.264 (via explicit codec preferences on the transceiver), so the device
  cannot select H.265 - its `resolveCodec` in `webrtc.go` prefers H.265 whenever the offer allows it, and Pion's
  default codec set includes it. If the negotiated codec somehow isn't H.264 anyway, video fails with a named
  error rather than feeding an undecodable stream to `ffmpeg`.
- A protocol gap requires one narrow compatibility path: the firmware's `hidrpc` data-channel handler
  (`hidrpc.go`'s `handleHidRPCMessage`) has no case for `TypeWheelReport`, so sending that binary report would be
  dropped. Scroll therefore uses the legacy `wheelReport` JSON-RPC method with its required `wheelY` and `wheelX`
  parameters. The exception is isolated in the client and documented by `internal/hidproto/hidproto.go`'s
  `WheelReportUnsupported` comment; all other keyboard/pointer input remains on `hidrpc`.

If you're on a materially different firmware version, expect the compatibility check to fail loudly rather than
silently doing the wrong thing - and please treat that as a signal to re-verify against current source, not to
bypass the check.

## Validation status

Be aware of what is and isn't proven before depending on this.

**Verified by the test suite** (`go test ./...`, also under `-race`): the full connect → auth → signal → WebRTC →
video → screenshot pipeline against an in-process fake device that speaks the real protocol with real Pion
negotiation; fresh-frame stable-screen and OCR-text polling; H.264 depacketization and frame assembly against an
FFmpeg-generated fixture; an actual FFmpeg decode of that fixture; one-shot OCR text adaptation; and the
control-plane safety behavior listed under [Security model](#security-model).

**Verified against real hardware (firmware 0.5.8): live read-only screenshot capture.** On 2026-08-05 two
separately established sessions each authenticated, negotiated ICE and the H.264 track, and captured a fresh
1920x1080 screenshot. The first completed in 7.23 seconds with a 143-packet keyframe; the second completed in
2.60 seconds with a 125-packet keyframe. Both reported `failureBoundary=none`, zero packet loss/reordering/
duplicates, and zero reassembly drops. That independently exercises the repaired receiver beyond the old
50-packet limit. Each capture lived only in the integration test's private temporary directory and was removed
automatically. The test now rejects a run that does not exercise a keyframe larger than 50 packets or reports
any reassembly drop.

To repeat the same read-only proof against another supported device, first load `JETKVM_PASSWORD` into the
environment through a secret manager (do not type a literal password into command history), then run:

```sh
JETKVM_LIVE_TEST=1 JETKVM_URL=http://your-device \
  go test -tags integration ./test/integration/... -run TestLiveSessionAndScreenshot -v
```

Keyboard, pointer, and scroll control has never been exercised against real hardware by this project. It is
unit-tested against fakes only.

## Limitations

- No virtual media, ATX/power, firmware update, network, or device-administration RPC method is implemented.
  This is intentional, not an oversight - see [SECURITY.md](SECURITY.md).
- Audio is not received or exposed.
- OCR quality and language coverage depend on the locally installed Tesseract engine and its trained data;
  recognition can be incomplete or wrong, so text readiness is a convenience signal rather than proof of exact
  pixels and OCR output is not a pixel-perfect transcription.
- Scroll-wheel input uses the firmware's legacy JSON-RPC compatibility path. The call is admitted under the HID
  control lease and neutralized on exit, but the RPC frame cannot carry the lease generation token and its
  acknowledgement cannot prove host-side delivery.
- Only one device connection per `jetkvmctl`/MCP server process; no multi-device fan-out.
- The device's transport is plaintext HTTP and this client cannot change that - see the warning at the top.
- The browser-based web UI remains the more maintainable choice if you need the full feature set (virtual media,
  ATX control, terminal, settings) - this tool exists specifically for agent-driven, non-interactive inspection
  and narrow control, not as a UI replacement.

## Troubleshooting

Start with `jetkvmctl doctor`. Its default report stays local; add `--probe-device` only when you also want one
bounded connect-and-status check.

- **`unreachable`** - check `--url`/`JETKVM_URL`, and that the device's web UI loads in a browser from the same
  network. One MCP call has already exhausted its bounded retry allowance; the server process has not respawned.
- **`auth-failed`** - the device rejected the supplied credentials; configure the
  Keychain service/account variables, set `JETKVM_PASSWORD` or `JETKVM_AUTH_TOKEN`, or pass `--password-stdin`.
- **`timeout`** - the operation or retry budget reached the caller/server deadline. Increase `--timeout` only
  after checking whether the device or video source is actually responding.
- **`bad-frame`** - the device returned malformed or oversized HTTP, signaling, or RPC data. Treat this as a
  firmware/protocol mismatch or a corrupt peer response; it is deliberately not retried.
- **`CompatibilityError: ... signaling-metadata ...`** - the device's signaling handshake didn't match this
  client's assumptions; you're likely on firmware materially different from the commit pinned above. Re-check
  `jetkvm/kvm`'s current `web.go`/`webrtc.go` before assuming it's safe to ignore.
- **FFmpeg is unavailable** - screenshots, read-text, stable-screen waits, and text waits fail during preflight, before a
  device session is opened. Install `ffmpeg` through Homebrew or your Linux package manager; `status` remains
  usable without it.
- **OCR engine is unavailable** - `read-text` or `wait-for-text` failed its local preflight before opening a device session.
  Install Tesseract with `brew install tesseract` on macOS or your distribution's Tesseract package. Non-OCR
  commands remain usable without it.
- **Screenshot times out waiting for a frame** - FFmpeg preflight has already passed. Rerun the screenshot command
  with `--diagnostics`, then read the block printed to stderr. `failureBoundary` names the single stage that stopped, and
  `wireNalUnitsByType` versus `nalUnitsByType` separates what the device sent from what reassembly produced.
- **`ffmpeg decode failed`** - the captured Annex-B frame didn't decode; this usually means the SPS/PPS/IDR
  assembly logic in `internal/jetkvm/video.go` needs to be re-checked against a firmware change.

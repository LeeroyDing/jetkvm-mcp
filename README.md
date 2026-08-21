# jetkvmctl

A native, browser-free controller for a [JetKVM](https://jetkvm.com) device: one Go binary with a CLI and an MCP
stdio server, so an agent (or a human without a browser handy) can check a JetKVM's status, inspect its display,
and wait for the screen to settle without opening the web UI.

This is a v1 compatibility spike, not a full remote-control client. It deliberately implements a narrow slice of
JetKVM's protocol - enough for read-only inspection, plus opt-in, heavily gated keyboard/mouse paths - and says
so explicitly where it's relying on undocumented behavior. See [Firmware compatibility](#firmware-compatibility)
and [Limitations](#limitations) before depending on it.

## What it does

- `status` - authenticates, opens a WebRTC session, and confirms the RPC data channel responds.
- `screenshot` - receives the live H.264 video track and decodes one fresh frame via FFmpeg. The CLI saves a PNG;
  MCP returns an in-memory PNG or JPEG with optional crop and down-scale controls.
- `wait-stable` - polls successive fresh decoded frames until the changed-pixel fraction stays at or below a threshold,
  providing read-only readiness gating before an agent acts.
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
complete thirteen-tool surface described here.

### Install / build

Requires `ffmpeg` on `PATH` (the H.264 decode backend for screenshots and stable-screen waits; `status` works
without it). Install it first — for example `brew install ffmpeg` on macOS or your distribution's `ffmpeg`
package on Linux.

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
jetkvmctl wait-stable
```

## CLI

```
jetkvmctl --version
jetkvmctl doctor       [--probe-device [--url URL] [--timeout DURATION]]
jetkvmctl status       [--url URL]
jetkvmctl screenshot   [--url URL] --output PATH [--diagnostics]
jetkvmctl wait-stable  [--url URL] [--threshold F] [--stable-frames N] [--poll-interval DURATION]
jetkvmctl serve        [--url URL] [--allow-control]
jetkvmctl keypress     [--url URL] --allow-control --key CODE [--modifier N]
jetkvmctl type         [--url URL] --allow-control --text TEXT [--delay-ms N]
jetkvmctl key-combo    [--url URL] --allow-control --combo NAME
jetkvmctl key-sequence [--url URL] --allow-control --combo NAME [--combo NAME ...] [--delay-ms N]
jetkvmctl mouse-move   [--url URL] --allow-control --x N --y N [--buttons N]
jetkvmctl click        [--url URL] --allow-control --x N --y N [--button N]
jetkvmctl scroll       [--url URL] --allow-control --dy N [--dx N]
jetkvmctl drag         [--url URL] --allow-control --x1 N --y1 N --x2 N --y2 N [--button N] [--steps N]
jetkvmctl release-all  [--url URL] --allow-control
```

The synopsis omits common flags for readability. Every device-facing command accepts `--timeout` (default
`10s`). `status`, `screenshot`, `wait-stable`, and every CLI control command also accept `--password-stdin`;
`doctor` does not, and `serve` rejects it because MCP owns stdin. `jetkvm_double_click` is currently MCP-only:
there is no `jetkvmctl double-click` command.

`jetkvmctl key-combo` and the `jetkvm_key_combo` MCP tool send a named keyboard chord as one HID report, then
release it. Built-in names are `ctrl+alt+del`, `cmd+space` (meta+space), `alt+tab`, `ctrl+c`, `ctrl+v`, `ctrl+z`,
`ctrl+shift+t`, `win`, `cmd`, `esc`, `enter`, and the bare keys `e`, `m`, `r`, and `t`. Names are
case-insensitive, and plus signs, hyphens, and whitespace are interchangeable separators. Both interfaces
require `--allow-control`.

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

`--password-stdin` is accepted by `status`, `screenshot`, `wait-stable`, `keypress`, `type`, `key-combo`,
`key-sequence`, `mouse-move`, `click`, `scroll`, `drag`, and `release-all`. Because it is an explicit per-command
choice, it takes precedence over `JETKVM_AUTH_TOKEN`, Keychain configuration, and `JETKVM_PASSWORD`; those sources
are not consulted. It is not a `doctor` option, and is **rejected by `serve`**: the MCP protocol owns stdin, and
reading a password line from it would consume the client's first JSON-RPC message. Use the environment variables
for MCP and device probes.

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
`jetkvm_wait_stable` tool plus dangerous keyboard/mouse input tools. See [Security model](#security-model) first.

### MCP tools

Every input is a strict JSON object. “Required” and “optional” below refer to object properties; `{}` is the
complete argument object for a no-argument tool.

Read-only catalog:

| Tool | Exact arguments | Result |
|---|---|---|
| `jetkvm_status` | `{}` | Device ID, firmware version, and RPC reachability |
| `jetkvm_screenshot` | Optional `format`: `"png"` (default) or `"jpeg"`; `quality`: integer 1–100 (JPEG only, default 80); `scale`: positive finite number (default 1, values above 1 clamp to 1); `region`: `{x,y,width,height}` source-pixel rectangle | One request-fresh PNG or JPEG in the response, optionally cropped/down-scaled, with truthful MIME and final/source dimensions; no filesystem write |

Opt-in catalog — all eleven additional tools are registered only with `--allow-control`:

| Tool | Exact arguments | Result or action |
|---|---|---|
| `jetkvm_wait_stable` | Optional `threshold`: finite number 0–1 (default 0.01); `stable_frames`: integer ≥1 (default 2); `poll_interval_ms`: integer 0–9,223,372,036,854 (default 250) | Read-only; compares successive fresh frames and returns settling state, frames sampled, final changed-pixel fraction, and elapsed time |
| `jetkvm_release_all` | `{}` | **Dangerous** — releases all held keys/buttons without moving the cursor |
| `jetkvm_keypress` | Required `key`: integer 0–255; optional `modifier`: integer 0–255 (default 0) | **Dangerous** — sends one live USB HID key usage |
| `jetkvm_type` | Required `text`: string of at most 4,096 runes; optional `delay_ms`: integer 0–500 (default 0) | **Dangerous** — types printable ASCII, newline, and tab using a US layout |
| `jetkvm_key_combo` | Required `combo`: one supported named chord | **Dangerous** — sends the chord in one keyboard report, then releases it |
| `jetkvm_key_sequence` | Required `combos`: array of 1–64 supported named chords; optional `delay_ms`: integer 0–500 (default 0) | **Dangerous** — sends an ordered, fully prevalidated sequence, releasing each chord before the delay and next chord |
| `jetkvm_mouse_move` | Required `x`, `y`: integers 0–32,767; optional `buttons`: integer 0–255 (default 0) | **Dangerous** — sends an absolute pointer/button state |
| `jetkvm_click` | Required `x`, `y`: integers 0–32,767; optional `button`: integer 0–255 (default 1 = left) | **Dangerous** — moves, presses, and releases at that position |
| `jetkvm_double_click` | Required `x`, `y`: integers 0–32,767; optional `button`: integer 0–255 (default 1 = left) | **Dangerous** — moves, then performs two immediate press/release cycles at that position; there is no delay parameter |
| `jetkvm_scroll` | Required `dy`: integer −127–127; optional `dx`: integer −127–127 (default 0); the two axes cannot both be zero | **Dangerous** — positive `dy` scrolls up; positive `dx` scrolls right |
| `jetkvm_drag` | Required `x1`, `y1`, `x2`, `y2`: integers 0–32,767; optional `button`: integer 0–255 (default 1); optional `steps`: integer 0–256 (default 0) | **Dangerous** — presses, moves while held directly or through optional intermediate steps, then releases; there is no duration/delay parameter |

When the server is started without `--allow-control`, it registers **exactly two tools**: `jetkvm_status` and
`jetkvm_screenshot`. Every opt-in tool, including the read-only `jetkvm_wait_stable` readiness gate and
`jetkvm_release_all`, is not merely refused - it is never registered, so it doesn't appear in `tools/list` at
all. With control enabled, the catalog contains exactly thirteen tools.

`jetkvm_wait_stable` is read-only but is advertised by MCP only with `--allow-control`. It accepts an optional
changed-pixel `threshold` from 0.0 through 1.0
(default 0.01), `stable_frames` of at least 1 consecutive stable comparisons (default 2), and a non-negative
`poll_interval_ms` between fresh-frame polls (default 250). A resolution change always counts as unstable, even
when the threshold is 1.0. The call compares only successive request-fresh decoded frames and remains bounded by
the caller deadline or the server's `--timeout` (default 10s). The matching CLI command uses `--threshold`,
`--stable-frames`, and a Go duration such as `250ms` for `--poll-interval`; unlike the MCP catalog entry, the
CLI command does not require or accept `--allow-control`.

`jetkvm_drag` requires start coordinates `x1`, `y1` and destination coordinates `x2`, `y2`. Its optional `button`
bitmask defaults to 1 (left), and `steps` selects from 0 through 256 intermediate moves made while the button
remains held. The default `steps=0` sends a direct held-button move from the start to the destination. Every drag
ends with a button-release report at the destination. The CLI `drag` command uses the same bounds and defaults.

`jetkvm_type` requires `text` and accepts an optional `delay_ms` from 0 through 500 (default 0) between keys. It
supports every printable ASCII character on a US keyboard, plus newline (Enter) and tab, with a maximum of 4096
runes per call. The complete string is mapped and validated before typing starts: an unsupported rune is reported
with its position and nothing from that call is sent. Each character follows the same control-lease,
generation-token, and neutralization path as `jetkvm_keypress`, so every key is released before the next is sent.
The CLI `type` command uses the same layout, limits, and per-key neutralization behavior.

`jetkvm_key_sequence` requires an ordered `combos` array containing from 1 through 64 named chords and accepts
optional `delay_ms` from 0 through 500 (default 0) between chords. The entire array is resolved and validated
before the first HID call; an invalid entry is reported by array index without echoing its raw value, and nothing
from that call is sent. Every chord uses the existing `jetkvm_key_combo` path, including release before the delay
and next chord. The CLI `key-sequence` command provides the same contract through repeatable `--combo` flags and
`--delay-ms`; both surfaces are available only with `--allow-control`.

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

`jetkvm_scroll` requires `dy` and accepts optional `dx` (default 0). Both are semantic wheel deltas in
`[-127,127]`, the signed range in the device's HID descriptors; positive `dy` scrolls up and positive `dx` scrolls
right, and at least one axis must be non-zero. The CLI `scroll` command uses the same bounds and directions. The
handler rejects invalid values before they can be narrowed to the firmware's signed-byte inputs.

All tool schemas are strict: unknown fields (including unknown `region` fields), wrong types, missing required
fields, and schema-declared numeric bounds are rejected as `InvalidParams` rather than silently ignored. Semantic
checks that depend on argument combinations or captured-frame dimensions return a redacted tool error instead:
examples include PNG plus `quality`, zero/zero scroll, an unknown combo, an unsupported typing rune, or a crop
outside the fresh frame. Only screenshot `scale` is clamped; other out-of-range values are rejected.

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

Keyboard and pointer/button input flows through one process-local exclusive control lease. A holder has a fixed
30-second watchdog from acquisition — it is not renewed by activity — while the caller or MCP operation deadline
(default 10 seconds) can end it sooner. Neutralization gets its own two-second cleanup budget. What the lease
actually proves, and what the tests pin down:

- Each holder gets a fresh, never-reused generation token, re-validated at the last moment before any frame is
  written. Input authorized by a lease that has since ended is **dropped**, and the caller is told - never
  delivered late.
- However a lease ends (release, cancellation, watchdog expiry, disconnect, shutdown), the generation is
  revoked first and neutralization frames are then written from a priority queue that pre-empts queued input, so
  neutralization is the last thing written for that generation.
- Neutralization clears buttons with a zero-delta *relative* mouse report, so releasing state can never move the
  attached computer's cursor.
- If neutralization can't be confirmed on the wire, that is reported as an error rather than a clean release.

The lease does not coordinate another `jetkvmctl` process, MCP server, or the browser UI. In the MCP adapter,
`jetkvm_drag` keeps one lease for its complete multi-report gesture; `jetkvm_type` acquires and neutralizes per
character, while click and double-click phases are individually leased and neutralized.

Scroll is the one transport exception. The firmware defines `TypeWheelReport`, but its `hidrpc` handler drops that
message type, so `jetkvm_scroll` intentionally uses the legacy JSON-RPC `wheelReport` method instead. It remains
control-gated at every public boundary: without `--allow-control` it is absent from the MCP catalog and refused by
the CLI, and the retrying device and `Client` layers independently re-check the gate. Calls are serialized, require
a matching RPC acknowledgement, and are never retried after the operation starts. A wheel event is stateless, so
this path cannot leave a key or button held and does not use the HID lease's generation token or neutralization.

This client can only prove what it wrote to a channel and, for scroll, that the firmware acknowledged the RPC. It
does not claim that the attached computer acted on the input.

## Architecture

```
cmd/jetkvmctl/          CLI adapter (thin: flag parsing -> internal/jetkvm -> print result)
internal/buildinfo/     Single version/provenance source for CLI, MCP, and release builds
internal/mcpserver/     MCP stdio adapter (thin: tool registration -> internal/jetkvm -> tool result)
internal/jetkvm/        session-owning core: auth, signaling, WebRTC, video, RPC, HID, control lease
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
negotiation; fresh-frame stable-screen polling and pixel comparison; H.264 depacketization and frame assembly
against an FFmpeg-generated fixture; an actual FFmpeg decode of that fixture; and the control-plane safety
behavior listed under [Security model](#security-model).

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
- Scroll-wheel input uses the firmware's legacy JSON-RPC compatibility path, so it does not receive the HID
  control lease's generation/neutralization guarantees; an RPC acknowledgement cannot prove host-side delivery.
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
- **FFmpeg is unavailable** - screenshots and stable-screen waits fail during preflight, before a device session
  is opened. Install `ffmpeg` through Homebrew or your Linux package manager; `status` remains usable without it.
- **Screenshot times out waiting for a frame** - FFmpeg preflight has already passed. Rerun the screenshot command
  with `--diagnostics`, then read the block printed to stderr. `failureBoundary` names the single stage that stopped, and
  `wireNalUnitsByType` versus `nalUnitsByType` separates what the device sent from what reassembly produced.
- **`ffmpeg decode failed`** - the captured Annex-B frame didn't decode; this usually means the SPS/PPS/IDR
  assembly logic in `internal/jetkvm/video.go` needs to be re-checked against a firmware change.

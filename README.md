# jetkvmctl

A native, browser-free controller for a [JetKVM](https://jetkvm.com) device: one Go binary with a CLI and an MCP
stdio server, so an agent (or a human without a browser handy) can check a JetKVM's status and grab a screenshot
of the attached computer's display without opening the web UI.

This is a v1 compatibility spike, not a full remote-control client. It deliberately implements a narrow slice of
JetKVM's protocol - enough for read-only inspection, plus an opt-in, heavily gated keyboard/mouse path - and says
so explicitly where it's relying on undocumented behavior. See [Firmware compatibility](#firmware-compatibility)
and [Limitations](#limitations) before depending on it.

## What it does

- `status` - authenticates, opens a WebRTC session, and confirms the RPC data channel responds.
- `screenshot` - receives the live H.264 video track, decodes one fresh frame via FFmpeg, and saves it as a PNG
  with its capture timestamp and dimensions.
- `serve` - the same functionality as an MCP server over stdio, for an agent to call as tools.
- `--version` - prints JSON build provenance from the same version source used by MCP `serverInfo`.
- `doctor` - reports local build, bundle, signing, configuration, FFmpeg, and Keychain-presence diagnostics;
  a device probe is opt-in.
- Keyboard/mouse control - implemented and unit-tested, but only reachable behind `--allow-control`, and never
  exercised against a live device by this project's own test suite (see [Security model](#security-model)).

> [!WARNING]
> **Affected JetKVM firmware serves its local web API and signaling WebSocket over plaintext HTTP.** Your device
> password and session cookie travel the LAN unencrypted, and anyone able to observe that network can read them
> and authenticate as you. This client cannot upgrade the device's transport - only the firmware can. Use a
> trusted, isolated network segment or a VPN, and pick a device password that protects nothing else. See
> [SECURITY.md](SECURITY.md#the-single-most-important-warning-plaintext-credentials-on-the-lan).

## Install / build

Requires `ffmpeg` on `PATH` (the H.264 decode backend for screenshots; `status` works without it). Install it
first — for example `brew install ffmpeg` on macOS or your distribution's `ffmpeg` package on Linux.

`go.mod` requires Go 1.26 or newer. `.go-version` records the canonical CI compiler (Go 1.26.6), and CI runs
native tests on `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64` runners. Locally built and
tested with Go 1.26.6 on darwin/arm64.

```sh
go build -o jetkvmctl ./cmd/jetkvmctl
```

## CLI

```
jetkvmctl --version
jetkvmctl doctor       [--probe-device [--url URL] [--timeout DURATION]]
jetkvmctl status       [--url URL]
jetkvmctl screenshot   [--url URL] --output PATH [--diagnostics]
jetkvmctl serve        [--url URL] [--allow-control]
jetkvmctl keypress     [--url URL] --allow-control --key CODE [--modifier N]
jetkvmctl type         [--url URL] --allow-control --text TEXT [--delay-ms N]
jetkvmctl key-combo    [--url URL] --allow-control --combo NAME
jetkvmctl mouse-move   [--url URL] --allow-control --x N --y N [--buttons N]
jetkvmctl click        [--url URL] --allow-control --x N --y N [--button N]
jetkvmctl release-all  [--url URL] --allow-control
```

`jetkvmctl key-combo` and the `jetkvm_key_combo` MCP tool send a named keyboard chord as one HID report, then
release it. Built-in names are `ctrl+alt+del`, `cmd+space` (meta+space), `alt+tab`, `ctrl+c`, `ctrl+v`, `ctrl+z`,
`ctrl+shift+t`, `win`, `cmd`, `esc`, and `enter`. Names are case-insensitive; `+` and `-` separators and
surrounding whitespace are accepted. Both interfaces require `--allow-control`.

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
deliberately no built-in default: a device address belongs to your network, not to this tool.

Examples:

```sh
export JETKVM_URL=http://jetkvm.local     # or http://<your-device-ip>

jetkvmctl status
jetkvmctl screenshot --output /tmp/shot.png
```

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

`--password-stdin` is accepted by `status`, `screenshot`, `keypress`, `type`, `key-combo`, `mouse-move`, `click`,
and `release-all`. Because it is an explicit per-command choice, it takes precedence over `JETKVM_AUTH_TOKEN`,
Keychain configuration, and `JETKVM_PASSWORD`; those sources are not consulted. It is not a `doctor` option,
and is **rejected by `serve`**: the MCP protocol owns stdin, and reading a password line from it would consume
the client's first JSON-RPC message. Use the environment variables for MCP and device probes.

## MCP configuration

Add to your MCP client's server config (adjust the path to the built binary):

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

Add `"--allow-control"` to `args` only if you want the agent to be able to send keyboard/mouse input - see
[Security model](#security-model) first.

### MCP tools

| Tool | Always available? | Description |
|---|---|---|
| `jetkvm_status` | yes | Device ID, firmware version, RPC reachability |
| `jetkvm_screenshot` | yes | One request-fresh screenshot, returned **as an image in the response**, plus dimensions and capture timestamp |
| `jetkvm_release_all` | only with `--allow-control` | Releases all held keys/buttons without moving the cursor |
| `jetkvm_keypress` | only with `--allow-control` | **Dangerous** - sends a live key press |
| `jetkvm_type` | only with `--allow-control` | **Dangerous** - types a whole string as live US-layout keypresses |
| `jetkvm_key_combo` | only with `--allow-control` | **Dangerous** - sends a named keyboard chord as one HID report, then releases it |
| `jetkvm_mouse_move` | only with `--allow-control` | **Dangerous** - moves the mouse / sets buttons |
| `jetkvm_click` | only with `--allow-control` | **Dangerous** - moves to an absolute position, presses a button bitmask (default 1 = left), then releases it there |

When the server is started without `--allow-control`, it registers **exactly two tools**: `jetkvm_status` and
`jetkvm_screenshot`. Every HID-capable tool, including `jetkvm_release_all`, is not merely refused - it is never
registered, so it doesn't appear in `tools/list` at all. With control enabled, the catalog contains exactly eight
tools.

`jetkvm_type` requires `text` and accepts an optional `delay_ms` from 0 through 500 (default 0) between keys. It
supports every printable ASCII character on a US keyboard, plus newline (Enter) and tab, with a maximum of 4096
runes per call. The complete string is mapped and validated before typing starts: an unsupported rune is reported
with its position and nothing from that call is sent. Each character follows the same control-lease,
generation-token, and neutralization path as `jetkvm_keypress`, so every key is released before the next is sent.
The CLI `type` command uses the same layout, limits, and per-key neutralization behavior.

All tool schemas are strict: unknown fields and out-of-range values are rejected as `InvalidParams` rather than
silently ignored. `jetkvm_screenshot` takes **no arguments** and writes nothing to disk - an earlier version
accepted a caller-supplied output path, which handed any MCP caller arbitrary file overwrite on the host running
the server.

### MCP call reliability

The MCP process starts without requiring the device to be online; its first tool call opens the device session.
For a transient `unreachable` failure, one read-only call may make at most **three total connection/operation
attempts**, with jittered exponential backoff starting at 75ms and capped at 300ms. The caller's deadline (or the
server's `--timeout`, default 10s) is the outer bound: a retry whose backoff cannot fit is not started. There are
no background reconnect loops, process respawns, or unbounded waits.

Authentication failures, timeouts, and malformed/oversized protocol frames are not retried. Keyboard, mouse,
and release operations may retry connection establishment before sending anything, but are never repeated after
an operation starts because delivery could be ambiguous. MCP error text begins with one stable category:
`auth-failed`, `unreachable`, `timeout`, or `bad-frame`.

Wire input is bounded before parsing: an RPC data-channel frame larger than 64 KiB is rejected as `bad-frame`,
and an HTTP response body is never read past 1 MiB — an oversized success body is rejected as `bad-frame`,
while an oversized error body is truncated so the HTTP status taxonomy (such as a 401 auth failure) still
surfaces. A misbehaving device or interposed peer cannot make the client allocate without limit.

## Security model

See [SECURITY.md](SECURITY.md) for the full trust boundary, the plaintext-LAN warning, and the rationale behind
each gate. Summary: this tool can see and (if opted in) control whatever is plugged into the JetKVM. Successful
screenshot results are captured after that request begins. The compatibility `fresh` field is always
`true` on success; if a strictly newer frame does not arrive before the deadline, the call fails rather than
returning a cached image.

Keyboard and mouse input flows through one exclusive control lease. What it actually proves, and what the tests
pin down:

- Each holder gets a fresh, never-reused generation token, re-validated at the last moment before any frame is
  written. Input authorized by a lease that has since ended is **dropped**, and the caller is told - never
  delivered late.
- However a lease ends (release, cancellation, inactivity timeout, disconnect, shutdown), the generation is
  revoked first and neutralization frames are then written from a priority queue that pre-empts queued input, so
  neutralization is the last thing written for that generation.
- Neutralization clears buttons with a zero-delta *relative* mouse report, so releasing state can never move the
  attached computer's cursor.
- If neutralization can't be confirmed on the wire, that is reported as an error rather than a clean release.

This client can only prove what it wrote to the channel. It does not claim to know that the attached computer
acted on it.

## Architecture

```
cmd/jetkvmctl/          CLI adapter (thin: flag parsing -> internal/jetkvm -> print result)
internal/buildinfo/     Single version/provenance source for CLI, MCP, and release builds
internal/mcpserver/     MCP stdio adapter (thin: tool registration -> internal/jetkvm -> tool result)
internal/jetkvm/        session-owning core: auth, signaling, WebRTC, video, RPC, HID, control lease
internal/hidproto/      HID-RPC wire format (encode/decode only, no transport)
test/integration/       read-only live integration test (build-tag gated, off by default)
```

Both adapters are thin wrappers around one `jetkvm.Client` - there is exactly one session owner, so CLI and MCP
share identical connection/auth/control-lease behavior rather than reimplementing it twice.

## How it talks to the device

No browser, no Playwright/Chromium, no `/dev/hidg*` writes. Just HTTP + a raw WebSocket + [Pion
WebRTC](https://github.com/pion/webrtc):

1. **Auth**: `GET /device/status` (public) to confirm the device is set up, then `POST /auth/login-local` with a
   password (or reuse a supplied session cookie) to get an `authToken` cookie - the same flow the web UI uses.
2. **Signaling**: `GET /webrtc/signaling/client` upgrades to a WebSocket. The device immediately sends a
   `device-metadata` message with its firmware version - this client checks that message's shape before doing
   anything else (see [Firmware compatibility](#firmware-compatibility)). We then send an SDP offer as
   `{"type":"offer","data":{"sd":base64(json(offer))}}` and get back `{"type":"answer","data":"<b64 sdp>"}`, with
   ICE candidates trickled both directions as `new-ice-candidate` messages.
3. **WebRTC**: a `recvonly` video transceiver (this client structurally cannot send video/audio - it never offers
   to), plus a `"rpc"` data channel (JSON-RPC 2.0, e.g. `ping`) and, only if `--allow-control` was passed, a
   `"hidrpc"` data channel (binary keyboard/mouse framing).
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
- A protocol gap was found and is deliberately *not* worked around: the firmware's `hidrpc` data-channel handler
  (`hidrpc.go`'s `handleHidRPCMessage`) has no case for `TypeWheelReport` - only the legacy `wheelReport`
  JSON-RPC method is wired up. This client's HID library does not implement scroll-wheel input as a result;
  see `internal/hidproto/hidproto.go`'s `WheelReportUnsupported` doc comment.

If you're on a materially different firmware version, expect the compatibility check to fail loudly rather than
silently doing the wrong thing - and please treat that as a signal to re-verify against current source, not to
bypass the check.

## Validation status

Be aware of what is and isn't proven before depending on this.

**Verified by the test suite** (`go test ./...`, also under `-race`): the full connect → auth → signal → WebRTC →
video → screenshot pipeline against an in-process fake device that speaks the real protocol with real Pion
negotiation; H.264 depacketization and frame assembly against an FFmpeg-generated fixture; an actual FFmpeg decode
of that fixture; and the control-plane concurrency guarantees listed under [Security model](#security-model).

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

Keyboard/mouse control has never been exercised against real hardware by this project. It is unit-tested against
fakes only.

## Limitations

- No virtual media, ATX/power, firmware update, network, or any other state-changing RPC method is implemented.
  This is intentional, not an oversight - see [SECURITY.md](SECURITY.md).
- Audio is not received or exposed.
- Scroll-wheel input is not implemented (see above).
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
- **FFmpeg is unavailable** - screenshots fail during preflight, before a device session is opened. Install
  `ffmpeg` through Homebrew or your Linux package manager; `status` remains usable without it.
- **Screenshot times out waiting for a frame** - FFmpeg preflight has already passed. Rerun the screenshot command
  with `--diagnostics`, then read the block printed to stderr. `failureBoundary` names the single stage that stopped, and
  `wireNalUnitsByType` versus `nalUnitsByType` separates what the device sent from what reassembly produced.
- **`ffmpeg decode failed`** - the captured Annex-B frame didn't decode; this usually means the SPS/PPS/IDR
  assembly logic in `internal/jetkvm/video.go` needs to be re-checked against a firmware change.

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
- Keyboard/mouse control - implemented and unit-tested, but only reachable behind `--allow-control`, and never
  exercised against a live device by this project's own test suite (see [Security model](#security-model)).

> [!WARNING]
> **Affected JetKVM firmware serves its local web API and signaling WebSocket over plaintext HTTP.** Your device
> password and session cookie travel the LAN unencrypted, and anyone able to observe that network can read them
> and authenticate as you. This client cannot upgrade the device's transport - only the firmware can. Use a
> trusted, isolated network segment or a VPN, and pick a device password that protects nothing else. See
> [SECURITY.md](SECURITY.md#the-single-most-important-warning-plaintext-credentials-on-the-lan).

## Install / build

### Runtime dependency

Screenshots require the `ffmpeg` executable on `PATH`; status does not. Install it first (for example,
`brew install ffmpeg` on macOS or your distribution's `ffmpeg` package on Linux), then verify the local runtime:

```sh
jetkvmctl doctor
```

`doctor` performs no device or network access. A server may start without FFmpeg and `jetkvm_status` remains
usable; `jetkvm_screenshot` fails its actionable FFmpeg preflight before opening a device session.

### Release archive

Release candidates contain checksummed archives for `darwin/amd64`, `darwin/arm64`, `linux/amd64`, and
`linux/arm64`, plus `BUILDINFO` provenance. Verify an archive before extracting it:

```sh
sha256sum --check SHA256SUMS     # Linux
shasum -a 256 --check SHA256SUMS # macOS
gh attestation verify jetkvmctl_0.2.0_darwin_arm64.tar.gz \
  --repo LeeroyDing/jetkvm-mcp
```

The tag workflow accepts only a stable-version tag whose commit is on `main` and has a successful protected
aggregate `test` check. It creates checksummed, reproducibility-compared binaries with GitHub build-provenance
attestations and can only create or update a **draft** release; publishing remains a separate maintainer action.

### Go toolchain

`go.mod` requires Go 1.26 or newer; CI and release builds pin Go 1.26.6 via `.go-version` and run natively on all
four supported OS/architecture pairs.

```sh
go install github.com/leeroyding/jetkvm-mcp/cmd/jetkvmctl@v0.2.0
# or, from a source checkout:
go build -o jetkvmctl ./cmd/jetkvmctl
```

`go install` installs the Go binary only; FFmpeg remains an explicit runtime dependency for screenshots.

## CLI

```
jetkvmctl --version
jetkvmctl doctor
jetkvmctl status       [--url URL]
jetkvmctl screenshot   [--url URL] --output PATH [--diagnostics]
jetkvmctl serve        [--url URL] [--allow-control]
jetkvmctl keypress     [--url URL] --allow-control --key CODE [--modifier N]
jetkvmctl mouse-move   [--url URL] --allow-control --x N --y N [--buttons N]
jetkvmctl release-all  [--url URL] --allow-control
```

`--url` is required, or set `$JETKVM_URL`. There is deliberately no built-in default: a device address belongs to
your network, not to this tool.

Examples:

```sh
export JETKVM_URL=http://jetkvm.local     # or http://<your-device-ip>

jetkvmctl status
jetkvmctl screenshot --output /tmp/shot.png
```

### Credentials

Credentials are never accepted as flags, so they do not appear in the process arguments shown by `ps`. A literal
environment assignment typed at a shell can still enter shell history: load it through your secret manager, or
use `--password-stdin` for CLI commands. Treat the process environment itself as sensitive.

| Mechanism | Effect |
|---|---|
| `JETKVM_PASSWORD` env var | Logs in via the device's real `/auth/login-local` flow |
| `JETKVM_AUTH_TOKEN` env var | Uses an already-valid session cookie directly, skipping login |
| `--password-stdin` | Reads a password from the first line of stdin (pipe it from a keychain/secret manager) |

If the device is in "noPassword" mode, none of these are needed.

`--password-stdin` is **rejected by `serve`**: the MCP protocol owns stdin, and reading a password line from it
would consume the client's first JSON-RPC message. Use the environment variables for MCP.

## MCP configuration

Add to your MCP client's server config (adjust the path to the built binary):

```json
{
  "mcpServers": {
    "jetkvm": {
      "command": "/path/to/jetkvmctl",
      "args": ["serve", "--url", "http://jetkvm.local"],
      "env": { "JETKVM_PASSWORD": "..." }
    }
  }
}
```

The literal `"..."` above is a placeholder, not a recommendation to store a password in a checked-in MCP
configuration. Use your MCP host's secret/environment injection facility and restrict access to its config.

Add `"--allow-control"` to `args` only if you want the agent to be able to send keyboard/mouse input - see
[Security model](#security-model) first.

### MCP tools

| Tool | Always available? | Description |
|---|---|---|
| `jetkvm_status` | yes | Device ID, firmware version, RPC reachability |
| `jetkvm_screenshot` | yes | One request-fresh screenshot, returned **as an image in the response**, plus dimensions and capture timestamp |
| `jetkvm_release_all` | only with `--allow-control` | Releases all held keys/buttons without moving the cursor |
| `jetkvm_keypress` | only with `--allow-control` | **Dangerous** - sends a live key press |
| `jetkvm_mouse_move` | only with `--allow-control` | **Dangerous** - moves the mouse / sets buttons |

When the server is started without `--allow-control`, it registers **exactly two tools**: `jetkvm_status` and
`jetkvm_screenshot`. Every HID-capable tool, including `jetkvm_release_all`, is absent from `tools/list`.

All tool schemas are strict: unknown fields and out-of-range values are rejected as `InvalidParams` rather than
silently ignored. `jetkvm_screenshot` takes **no arguments** and writes nothing to disk - an earlier version
accepted a caller-supplied output path, which handed any MCP caller arbitrary file overwrite on the host running
the server.

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
cmd/jetkvmctl/          CLI adapter (flags, doctor/version, redacted result rendering)
internal/mcpserver/     MCP tools + fresh-session manager and Darwin/Linux coordination
internal/jetkvm/        session-owning core: auth, signaling, WebRTC, video, RPC, HID, control lease
internal/buildinfo/     authoritative semantic version and injected build provenance
internal/hidproto/      HID-RPC wire format (encode/decode only, no transport)
test/integration/       read-only live integration test (build-tag gated, off by default)
```

Each `jetkvm.Client` owns exactly one WebRTC session. One-shot CLI commands create one client. The MCP server
keeps **no eager or idle client**: every tool call acquires exclusive coordination, connects a fresh client,
executes once, closes deterministically, and only then returns a result. Control calls use a fresh
control-enabled client and are never replayed. Status/screenshot alone may receive one new-session retry, and
only for an explicitly classified terminal transport/handoff failure.

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

- `web.go` has one global `currentSession`; accepting a new local or cloud session replaces it and closes the
  previous peer after one second, while native video frames go only to `currentSession.VideoTrack`. A persistent
  MCP WebRTC session would therefore monopolise or lose the device, and persistent reconnects could steal it in
  a loop. MCP uses bounded fresh sessions per call instead. Calls are serialized in-process and cooperative
  MCP processes on the same Darwin/Linux user account coordinate with a private advisory file lock. One-shot
  CLI commands deliberately do not acquire the MCP lock; like browser, cloud, old-client, different-user, and
  URL-alias sessions, they remain external competitors. The single bounded
  read-only retry covers only a terminal handoff, not arbitrary failures.

- The very first signaling message is checked to actually be `{"type":"device-metadata",...}` with a bounded,
  safely printable `deviceVersion` field before anything else happens. If it isn't, you get an actionable
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

The version string itself is informational, not an allowlist: upstream publishes no stable protocol/version
compatibility contract from which to derive a sound range. Handshake-shape drift fails at the metadata check;
later protocol drift fails at the affected signaling, RPC, media, or HID boundary. If your firmware differs from
the pinned source, re-verify against current upstream rather than treating a successful metadata check as proof
of compatibility.

## Validation status

Be aware of what is and isn't proven before depending on this.

**Verified by the test suite** (`go test ./...`, also under `-race`): the full connect → auth → signal → WebRTC →
video → screenshot pipeline against an in-process fake device that speaks the real protocol with real Pion
negotiation; H.264 depacketization and frame assembly against an FFmpeg-generated fixture; an actual FFmpeg decode
of that fixture; request-bound frame generations; firmware-faithful single-session handoff; bounded fresh-session
retry/cleanup/cancellation; same-user cross-process locking; and the control-plane concurrency guarantees listed
under [Security model](#security-model).

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
- No multi-device fan-out. One MCP server configuration targets one canonical device URL.
- Same-user MCP processes coordinate only when configured with the same canonical URL. One-shot CLI commands,
  DNS/IP aliases, browsers, cloud clients, different OS users, and older clients remain external session
  competitors. Run CLI inspection only when no MCP control operation is in flight.
- The device's transport is plaintext HTTP and this client cannot change that - see the warning at the top.
- The browser-based web UI remains the more maintainable choice if you need the full feature set (virtual media,
  ATX control, terminal, settings) - this tool exists specifically for agent-driven, non-interactive inspection
  and narrow control, not as a UI replacement.

## Troubleshooting

- **"device unreachable"** - check `--url`/`JETKVM_URL`, and that the device's web UI loads in a browser from the
  same network.
- **"not authenticated"** - the device is in password mode and no credentials were supplied; set
  `JETKVM_PASSWORD` or `JETKVM_AUTH_TOKEN`, or pass `--password-stdin`.
- **`CompatibilityError: ... signaling-metadata ...`** - the device's signaling handshake didn't match this
  client's assumptions; you're likely on firmware materially different from the commit pinned above. Re-check
  `jetkvm/kvm`'s current `web.go`/`webrtc.go` before assuming it's safe to ignore.
- **Screenshot times out waiting for a frame** - confirm the local prerequisite with `jetkvmctl doctor`, rerun
  the screenshot command with `--diagnostics`, then read the block printed to stderr. `failureBoundary` names
  the single stage that stopped, and
  `wireNalUnitsByType` versus `nalUnitsByType` separates what the device sent from what reassembly produced.
- **`ffmpeg decode failed`** - the captured Annex-B frame didn't decode; this usually means the SPS/PPS/IDR
  assembly logic in `internal/jetkvm/video.go` needs to be re-checked against a firmware change.

# Walkthrough

What this is, how it was built, what was actually proven against the live device, and what's left.

## Goal

An agent-native, browser-free controller for a JetKVM device: one Go binary (`jetkvmctl`) exposing a
CLI and an MCP stdio server, built as a compatibility spike first (read-only auth/signaling/video) and a
controller second (opt-in, gated input), following a staged architecture.

## Evidence gathered before writing any protocol code

- Cloned `https://github.com/jetkvm/kvm` (`dev` branch, commit `b3c29a4`, read 2026-08-02) and read, in full or in
  relevant part: `web.go` (HTTP routes, auth, local WebSocket signaling), `webrtc.go` (`Session`, `ExchangeOffer`,
  data channel wiring, codec negotiation), `cloud.go`'s `handleSessionRequest` (the shared offer/answer/ICE
  handshake logic), `jsonrpc.go` (the JSON-RPC method table on the `"rpc"` data channel), `hidrpc.go` and
  `internal/hidrpc/{hidrpc,message}.go` (the binary HID wire format on the `"hidrpc"` data channel), `version.go`/
  `ota.go` (firmware version string handling).
- Confirmed the read-only HTTP surface behaves as the source says, against a real device on the author's own
  network: the web UI responds, `GET /device/status` reports `isSetup: true`, and `GET /device` without a
  session cookie returns 401 (i.e. password mode, not `noPassword`). Nothing was modified on the device.
- Reproduced a mismatch the firmware itself has: the `hidrpc` data channel's server-side handler
  (`hidrpc.go`'s `handleHidRPCMessage`) has no case for `hidrpc.TypeWheelReport` - only the legacy
  `"wheelReport"` JSON-RPC method is wired. Scroll therefore uses that narrow compatibility path with both required
  parameters, `wheelY` and `wheelX`, while the unsupported HID message remains disabled and documented in
  `internal/hidproto/hidproto.go` and `README.md`.

## Architecture

```
cmd/jetkvmctl/          CLI adapter - flag parsing only, delegates to internal/jetkvm
internal/mcpserver/     MCP stdio adapter - tool registration only, delegates to internal/jetkvm
internal/jetkvm/        session-owning core (see below)
internal/hidproto/      HID-RPC wire format: pure encode/decode, no transport, no device access
test/integration/       build-tag-gated live test against the real device (off by default)
```

`internal/jetkvm` files, in the order the data flows:

- `redact.go` - `Secret` type; credentials can't leak through `%v`/JSON/logging.
- `http.go` - `/device/status`, `/auth/login-local`, `/device`; cookie-jar-based session.
- `compat.go` - pins the evidence commit; validates the signaling handshake's shape before anything else runs.
- `signaling.go` - the local WebSocket: offer/answer/ICE-candidate envelopes, matching `web.go`'s wire shapes
  exactly (including the legacy comment in the firmware about backward compatibility, which this client doesn't
  need since it only ever speaks the current WebSocket-based flow).
- `session.go` - Pion `PeerConnection` setup: `recvonly` video transceiver, `"rpc"` and (opt-in) `"hidrpc"` data
  channels, ICE trickling, connection-state handling. Deliberately runs its own long-lived background context
  independent of the caller's request-scoped `ctx` (a real bug caught by testing - see below).
- `video.go` - RTP→Annex-B depacketization via Pion's H.264 codec, SPS/PPS caching, "wait for one self-contained
  IDR" frame capture.
- `h264.go` - the RTP receive path's H.264 knowledge: the reassembly window, derived from the firmware's encoder
  output-buffer bound rather than defaulted; per-packet packetization classification, which is what lets a
  keyframe the device *sent* be distinguished from one this client managed to *rebuild*; sequence and
  frame-boundary trackers; and RFC 6184 `sprop-parameter-sets` parsing.
- `decoder.go` - `Decoder` interface + `FFmpegDecoder` (shells out to `ffmpeg`, in-memory pipes, no temp files).
- `rpc.go` - JSON-RPC 2.0 request/response correlation and event delivery on the `"rpc"` channel.
- `hid.go` - the HID control state machine: readiness handshake, a single bounded writer goroutine, lease
  generation validation at the final send boundary, and pre-emptive neutralization that never moves the cursor.
- `owner.go` - the control lease: `Acquire`/`TryAcquire`/`Release`, generation tokens, and a watchdog that
  force-releases on context cancellation or inactivity timeout.
- `redact.go` - `Secret`, plus the central error/URL/response-body redaction used everywhere output is produced.
- `client.go` - `Client`, the single session owner: `Connect`, `Status`, `CaptureScreenshot`/`SaveScreenshot`,
  `Scroll` (control-gated through the legacy RPC path), `Control`, and `Close`, with a command lock serializing
  operations through one `Client`.

## Tests (`go test ./...`, also clean under `-race`)

- `internal/hidproto`: wire-format encode/decode round trips, range validation, the canonical release-all
  frames (keyboard clear plus the zero-delta relative mouse report), keydown-state decode/`IsReleaseAll`.
- `internal/jetkvm`: HTTP auth including redaction canaries (no password, token, cookie, `Authorization` header
  or URL query string survives an error path, and auth response bodies are dropped outright); the HID control
  state machine against a deterministic transport double that can park the writer mid-send, which is what makes
  the ordering guarantees assertions rather than timing races - queued stale sends dropped, release-all
  pre-empting queued input, generation reuse, disconnect/reconnect, backpressure bounds, handshake failure, and
  release retry versus unverified reporting; the FFmpeg subprocess environment boundary (a real child process is
  inspected for inherited credentials); scroll bounds and exact `wheelY`/`wheelX` JSON-RPC encoding; NAL splitting
  and frame assembly against a **real FFmpeg-generated H.264
  fixture** (`internal/jetkvm/testdata/synthetic_red_32x32.h264` - a synthetic red 32×32 test pattern, not live
  screen content); an actual FFmpeg decode of that fixture verifying pixel colour and dimensions; and full
  end-to-end `Connect`→`Status`→`Screenshot` runs against an in-process fake device that speaks the real Pion
  WebRTC protocol (not a mock - a second, independent implementation of the device's offer/answer/data-channel/
  video-streaming behaviour, so these tests catch real wire-level bugs).
- `internal/mcpserver`: tool registration gating (control tools structurally absent from `tools/list` without
  `--allow-control`), real `CallTool` round trips, strict-schema enforcement (unknown fields and out-of-range
  values rejected), one-shot scroll forwarding through the retry wrapper, the screenshot tool returning image
  content while writing nothing and rejecting any caller-supplied path, and a source-level check that the package
  never writes to stdout.
- `cmd/jetkvmctl`: the `serve` versus `--password-stdin` incompatibility, the absence of any baked-in device
  address, and the `--allow-control` gate on every control subcommand.

**A real bug caught by this test suite**: the first version of `session.go` passed the caller's `ctx` (the one
used only to bound `Connect()`'s handshake) directly into the session's long-lived background goroutines (video
depacketization, signaling pump, keyframe requests). A test that used a short-lived context for `Connect()` and
let it expire afterward - a completely reasonable calling pattern - caused the *entire session* to silently die
a few hundred milliseconds later, because canceling that context force-closed the underlying WebSocket read the
signaling pump was blocked on. Fixed by giving `session` its own `context.WithCancel(context.Background())`,
independent of the caller's context, canceled only by `session.close()`. This is exactly the kind of
failure mode a stateful MCP session must guard against, and it would have caused any MCP client that passes a
bounded context into a single tool call to silently break every subsequent call.

## What was proven live

Ran via `go test -tags integration ./test/integration/... -v` (this test is excluded from the default `go test
./...` run; it requires the `integration` build tag and `JETKVM_LIVE_TEST=1`):

- **`TestLiveReachability`: PASS.** TCP dial to port 80 succeeds; `GET /device/status` returns HTTP 200 with
  `isSetup: true`.
- **`TestLiveSessionAndScreenshot`: PASS twice on separate connections.** Against firmware 0.5.8, both fresh
  sessions authenticated, negotiated ICE and the H.264 track, and captured a fresh 1920x1080 screenshot. The
  runs completed in 7.23 seconds and 2.60 seconds respectively. Their largest on-wire keyframes used 143 and
  125 RTP packets - both well beyond the old broken 50-packet receiver window - with zero packet loss,
  reordering, duplicates or reassembly drops and `failureBoundary=none`.

No keyboard, pointer, scroll, ATX, power, virtual-media, firmware, or reboot RPC was sent to the device at any point
in this project's development, live testing, or automated test suite. Live traffic was limited to the read-only
reachability/status, authentication, signaling, ICE and receive-only video paths described above. Both images
were held only in private test temporary directories and removed automatically.

## Known gaps

1. **Live control-plane behaviour is unverified.** No keyboard, pointer, or scroll input has ever been sent to real
   hardware by this project. The HID state machine, lease generations and neutralization ordering are proven
   against fakes and a deterministic transport double only.
2. **Scroll uses a legacy transport exception.** A successful `wheelReport` RPC acknowledgement cannot prove the
   attached host received the stateless wheel event, and this path does not have the HID lease's generation or
   neutralization guarantees. It remains control-gated, serialized, and non-retried after an operation starts.
3. **H.265 is not supported.** The offer advertises only H.264 via explicit transceiver codec preferences, so
   the device cannot select H.265 (its `resolveCodec` prefers H.265 whenever the offer permits it).

## How to repeat the read-only live proof

Use a fresh connection for each run. First load `JETKVM_PASSWORD` into the environment through a secret manager
(do not type a literal password into command history), then run:

```sh
JETKVM_LIVE_TEST=1 JETKVM_URL=http://your-device \
  go test -tags integration ./test/integration/... -run TestLiveSessionAndScreenshot -v
```

This remains **read-only**: it logs in with the real login flow (`POST /auth/login-local`), opens a `recvonly`
video session (`AllowControl: false` is hardcoded in the test), and captures exactly one screenshot in a private
test temporary directory that is removed automatically. Logs contain bounded diagnostics plus dimensions,
timestamp and freshness only - never pixels, a content-derived hash, an address, a device ID or a filesystem
path. No HID channel is opened. A run fails if the receiver drops reassembly packets or the observed keyframe
does not exceed the old 50-packet window, so a pass proves the repaired boundary was exercised.

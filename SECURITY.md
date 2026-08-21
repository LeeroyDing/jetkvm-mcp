# Security model

## Reporting a vulnerability

Please report security issues privately by opening a
[GitHub security advisory](https://docs.github.com/en/code-security/security-advisories/guiding-contributors-through-security-vulnerabilities/privately-reporting-a-security-vulnerability)
on this repository, rather than filing a public issue.

This is a personal project maintained on a best-effort basis. There is no
guaranteed response time and no bug bounty. Expect an acknowledgement when the
maintainer next picks up the project; if a fix is warranted it will land in a
normal commit with the issue described in the message. If you need a guaranteed
response, please assume there isn't one and act accordingly.

## The single most important warning: plaintext credentials on the LAN

**Affected JetKVM firmware serves its local web API and signaling WebSocket over
unencrypted HTTP on the LAN.** That means:

- Your device password is sent in the clear in the body of `POST /auth/login-local`.
- The resulting `authToken` session cookie is sent in the clear on every
  subsequent request, including the `GET /webrtc/signaling/client` WebSocket
  upgrade that carries the WebRTC session negotiation.
- Anyone who can observe that network segment - another host on the same Wi-Fi,
  a compromised switch or router, an attacker in a position to ARP-spoof - can
  read both, and can then authenticate to the device themselves.

**This client cannot fix that.** It is a property of the device's own transport.
`jetkvmctl` can only speak the protocol the firmware exposes; it has no way to
negotiate TLS the device does not offer, and it does not pretend otherwise by,
say, wrapping the connection in something that looks encrypted but isn't.

What you can actually do:

- Put the JetKVM on a **trusted, isolated network segment** (a management VLAN,
  a dedicated subnet) and treat everything on that segment as able to reach it.
- Reach it **over a VPN or an SSH tunnel** rather than across an untrusted LAN.
- Do not use it over shared, public, or guest Wi-Fi.
- Assume the device password is exposed to that network and use one that
  protects nothing else.

If a future firmware offers HTTPS for the local API, this section should be
revisited - it is written about the firmware this client was built against, not
about JetKVM forever.

## Trust boundary

`jetkvmctl` sits between whoever/whatever invokes it (a human, a script, or an
MCP-calling agent) and a JetKVM device that has direct keyboard/mouse/video
access to another computer. That means:

- **The device's video feed can show anything on the attached computer's
  screen**, including secrets that happen to be visible at capture time (open
  password managers, terminal scrollback, etc.). This tool has no way to redact
  screen content - it only adds metadata (timestamp, dimensions, freshness)
  around the pixels. Treat every screenshot as being as sensitive as whatever
  might be on that screen. The MCP screenshot tool returns the image *in the
  response*, which means it flows to whatever MCP client and model you have
  connected. `jetkvm_read_text` does not make that content less sensitive: the
  captured pixels are provided to a local OCR subprocess, and the recognized
  text flows to the CLI caller or connected MCP client/model. A password or
  token visible on-screen may therefore appear verbatim in OCR output.
- **Keyboard/mouse control, if enabled, is equivalent to physical access** to the
  attached computer. There is no OS-level distinction between "a person at the
  keyboard" and an input report arriving through the JetKVM - the attached computer
  cannot tell the difference. Anything a person could do by typing, clicking, and
  scrolling, this tool (and anyone/anything driving it) can do.
- **The MCP server, once running with `--allow-control`, will act on any tool
  call it receives** from whatever MCP client is connected to it. It does not
  attempt to authenticate or authorize the *caller* - that's the MCP client or
  host's job. Don't expose a `--allow-control` server to an agent or client you
  wouldn't hand a keyboard to.

## Why dangerous actions are opt-in

Layered gates exist before a single keystroke, pointer action, or scroll reaches
the device, on purpose:

1. **`--allow-control` at the public surface** (CLI flag or MCP server flag).
   Without it, each CLI control subcommand (`keypress`, `type`, `key-combo`,
   `key-sequence`, `mouse-button`, `mouse-move`, `scroll`, `click`,
   `double-click`, `drag`, and `release-all`) refuses to run, and the MCP server
   omits `jetkvm_release_all`, `jetkvm_keypress`, `jetkvm_type`,
   `jetkvm_key_combo`, `jetkvm_key_sequence`, `jetkvm_mouse_button`,
   `jetkvm_mouse_move`, `jetkvm_click`, `jetkvm_double_click`, `jetkvm_scroll`,
   and `jetkvm_drag`
   from `tools/list`.
2. **Independent device and client checks.** The retrying MCP device carries the
   control setting and rejects scroll when it is disabled; `Client.Scroll`
   checks it again before using the otherwise-always-present RPC channel. For
   keyboard and pointer input, a disabled connection never constructs the
   `hidrpc` channel or control lease.
3. **Transport acknowledgement.** Stateful HID input is not permitted until the
   device echoes the HID-RPC readiness handshake. Scroll uses the firmware's
   legacy JSON-RPC exception described below, and a call is successful only
   after the matching RPC response arrives; a missing path, device error, or
   timeout is not reported as success.
4. **One-shot execution policy.** Keyboard and pointer input use the exclusive
   control lease below. Scroll calls are serialized and, like all control
   operations, are never retried after the operation starts because delivery
   could be ambiguous.

## What the control lease actually guarantees

These properties apply to keyboard and pointer/button reports sent over
`hidrpc`. The lease is process-local, not a device-wide lock. Its watchdog is a
fixed 30 seconds from acquisition and is not renewed by activity; a shorter
caller or MCP operation deadline can end it first. Neutralization runs with a
fresh two-second cleanup bound. These guarantees are stated narrowly on purpose
- the previous version of this document claimed more than the code delivered.

- **Exclusivity.** At most one holder at a time. A second acquirer either waits
  (bounded by its context) or is told the lease is held.
- **Generation validation at the send boundary.** Each holder receives a fresh,
  never-reused token. Every frame is re-validated against the currently-active
  token at the last moment before it is written to the channel - not at the call
  site. A frame authorized by a lease that has since ended is *dropped*, and the
  caller is told so; it is never delivered late.
- **Terminal, pre-emptive neutralization.** However a lease ends - explicit
  release, context cancellation, watchdog expiry, disconnect, or process
  shutdown - the generation is revoked *first*. Neutral reports then jump ahead
  of input still in the application queue. Bytes already accepted by Pion cannot
  be pre-empted, but the ordered channel places neutralization after them, so it
  is the last HID data sent for that generation.
- **Bounded lower-layer buffering.** Before every Pion `Send`, the client checks
  the outbound HID application-byte count against a 4 KiB cap. Twelve bytes are
  reserved for the neutral pair. A report that would exceed its limit fails as
  not sent instead of growing SCTP's otherwise-unbounded pending queue.
- **No cursor movement.** Neutralization clears buttons with a zero-delta
  *relative* mouse report, never an absolute pointer report. Clearing state
  cannot move the attached computer's cursor as a side effect.
- **Transport-confirmed success and truthful failure.** Release success requires
  both neutral reports to be accepted and Pion's outbound amount to reach zero
  within the cleanup deadline. In the pinned Pion/SCTP versions, zero means the
  peer SCTP transport acknowledged every queued application byte. If that does
  not happen, the error says the neutral state is not confirmed and the client
  keeps believing input is held.

Ordered key sequences are bounded to 1 through 64 named chords, with an
optional delay from 0 through 500 milliseconds (default 0). The complete list
is resolved and wire-validated before the first HID call, so an invalid later
entry cannot produce a partially sent sequence. Once execution begins, each
chord uses the existing key-combo path and its neutral reports must be
transport-confirmed before the delay and next chord. A later transport failure
can still leave an already-completed prefix; state-changing operations are not
retried after sending because their delivery would be ambiguous.

What this does **not** guarantee: that input is definitely no longer held on the
attached computer. A zero outbound amount proves peer SCTP acknowledgement, not
that the firmware applied the report to USB or that the host acted on it. A
device that acknowledges a frame and fails to act on it is outside what this
client can verify.

## Scroll's legacy RPC exception

The firmware defines binary `TypeWheelReport`, but its `hidrpc` input handler
has no case for it and drops it. The only wired scroll path is the legacy
JSON-RPC `wheelReport` method, with signed `wheelY` and `wheelX` values in
`[-127,127]` and at least one non-zero axis. This client therefore uses that
method deliberately rather than claiming an unsupported HID send succeeded.

A wheel event is stateless: it cannot leave a key or mouse button held, so
terminal neutralization is neither needed nor meaningful. The tradeoff is that
scroll does not receive the HID lease's generation-token or neutralization
guarantees. Its RPC acknowledgement proves only that the firmware handled the
request, not that the attached host received it; this firmware may acknowledge
while its USB HID path is temporarily unavailable. The independent
`--allow-control`, retrying-device, and `Client` checks, serialization, and
one-shot retry policy still apply.

## What is deliberately not implemented

Virtual media mounting, ATX/power control, firmware update/OTA, and
network/Tailscale/MQTT configuration are simply not wired up in
`internal/jetkvm`. This isn't a missing feature so much as a scope boundary:
this tool's job is inspection and (opt-in) input, not device administration.
Adding any of those would need its own explicit gate and its own review, the
same way `--allow-control` did.

## Secrets handling

- Credentials are never accepted as command-line arguments, which would expose
  them in process listings via `ps`. Accepted mechanisms are a macOS Keychain
  item (named by the non-secret `JETKVM_PASSWORD_KEYCHAIN_SERVICE` and
  `JETKVM_PASSWORD_KEYCHAIN_ACCOUNT` variables), the `JETKVM_PASSWORD` and
  `JETKVM_AUTH_TOKEN` environment variables, or stdin (`--password-stdin`). A
  literal environment assignment typed at a shell can still enter shell
  history, so prefer the Keychain item or inject the value through a secret
  manager; for CLI commands, `--password-stdin` avoids putting the value in
  either arguments or the shell command. Treat the parent process environment
  as sensitive too.
- **Keychain lookup runs a fixed absolute path**, `/usr/bin/security
  find-generic-password … -w`, so credential resolution cannot be redirected
  by a modified `PATH`, and it contacts no external secret provider. Its
  output is accepted only as a single non-empty line; anything else is treated
  as a failed lookup. The subprocess's stderr is never surfaced across the
  redaction boundary, and a failed (but not canceled) or malformed lookup
  falls back to `JETKVM_PASSWORD` only when that variable is present —
  otherwise the error reports the failure without quoting command output. An explicit
  `JETKVM_AUTH_TOKEN` skips password resolution entirely. For one-shot
  commands the Keychain subprocess shares the command context, so cancellation
  or the command deadline also terminates a stuck lookup.
- An explicit `--password-stdin` is authoritative for that device command: it
  is read before, and instead of, token, Keychain, or password environment
  sources. This prevents a stale `JETKVM_AUTH_TOKEN` from silently overriding
  the password the operator deliberately piped in.
- `--password-stdin` is **rejected by `serve`**. The MCP protocol owns
  stdin/stdout; consuming a line of stdin for a password would eat the client's
  first JSON-RPC message. The two modes are genuinely incompatible, so this
  fails fast with an explanation instead of corrupting the transport.
- Internally, credentials are wrapped in a `Secret` type
  (`internal/jetkvm/redact.go`) whose `String()`/`GoString()`/`MarshalJSON()` all
  return `<redacted>`. Production calls `Secret.Expose()` only at the two
  outbound credential boundaries: building the password login body and setting
  a supplied `authToken` cookie.
- **Credential-reflecting response bodies are never quoted back.** A body from
  an `/auth/*` endpoint is always dropped, because a reflected credential need
  not look like one. A non-auth error body is also dropped in full when it
  contains an exact configured password or auth token, including short values
  that generic token heuristics cannot recognize. Remaining error bodies are
  scrubbed for credential-shaped key/value pairs and long opaque tokens, then
  truncated.
- **Errors are redacted centrally.** URLs lose their userinfo, query string, and
  fragment before appearing in any error (Go's `*url.Error` embeds the full
  URL). Transport errors are flattened rather than wrapped, so nothing can
  unwrap back to an unredacted original.
- **The FFmpeg and Tesseract subprocesses get allowlisted environments**, not
  this process's.
  `exec.Cmd` inherits everything when `Env` is nil, which would hand the decoder
  or OCR engine `JETKVM_PASSWORD` and any secrets belonging to the agent or
  secret manager that launched this process. Both receive `PATH`, the temp-dir
  variables, and (on Windows) `SystemRoot`/`WINDIR`; Tesseract additionally
  receives `TESSDATA_PREFIX` for custom trained-data installations.
  `LD_LIBRARY_PATH`, `DYLD_*` and `HOME` are excluded deliberately.
- **Video decode and screenshot output have explicit allocation ceilings.**
  Compressed H.264 reassembly stays below 4 MiB. FFmpeg receives a 16,777,216
  pixel limit and a 256 MiB single-allocation limit; its PNG stdout is capped at
  66 MiB. Go checks the PNG configuration independently before allocating its
  pixel image, then checks the decoded bounds again before any transform:
  neither axis may exceed 8,192, and crop/scale/re-encode output uses the same
  pixel and encoded-byte ceilings.
- **Tesseract receives no caller-controlled command-line arguments or paths.**
  The transformed PNG is piped to stdin and recognized text is read from
  stdout, with a context deadline, bounded/drained output, and fixed arguments.
  No shell is involved, and this client creates no temporary screenshot or
  OCR-output file. This narrows command-injection and persistence risk, but
  Tesseract still sees the complete selected frame and must be treated as part
  of the trusted local computing base.
- **MCP stdout stays protocol-clean.** Diagnostics go to stderr, and a test
  enforces at source level that the MCP package never writes to stdout.
- Session credentials (the device's `authToken` cookie) live only in an
  in-memory cookie jar for the process's lifetime; nothing is written to disk.
- **The MCP screenshot and OCR tools write no files at all.** Screenshot
  returns the image in the response. An earlier version accepted a
  caller-supplied `output_path`,
  which handed any MCP caller arbitrary file overwrite (plus traversal and
  symlink following) on the host running the server; that parameter is now
  rejected rather than ignored. OCR passes image and text through subprocess
  pipes and returns text only. The screenshot CLI still takes a path - it comes
  from the local user's own command line - and writes atomically.

## Firmware/protocol trust

This client's understanding of JetKVM's local auth/signaling/WebRTC/HID protocol
comes entirely from reading `jetkvm/kvm`'s own source (see README.md's firmware
compatibility section for the exact commit). That source is not a published API
contract, so this client verifies the signaling handshake's shape before
proceeding, produces a named `CompatibilityError` on mismatch instead of
misbehaving silently, and keeps protocol-parsing code behind narrow interfaces
so a firmware change can be absorbed locally.

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
  screen content - it only adds metadata (timestamp, dimensions, request freshness)
  around the pixels. Treat every screenshot as being as sensitive as whatever
  might be on that screen. The MCP screenshot tool returns the image *in the
  response*, which means it flows to whatever MCP client and model you have
  connected.
- **Keyboard/mouse control, if enabled, is equivalent to physical access** to the
  attached computer. There is no OS-level distinction between "a person at the
  keyboard" and "a HID report arriving over `hidrpc`" - the attached computer
  cannot tell the difference. Anything a person could do by typing and clicking,
  this tool (and anyone/anything driving it) can do.
- **The MCP server, once running with `--allow-control`, will act on any tool
  call it receives** from whatever MCP client is connected to it. It does not
  attempt to authenticate or authorize the *caller* - that's the MCP client or
  host's job. Don't expose a `--allow-control` server to an agent or client you
  wouldn't hand a keyboard to.

## Why dangerous actions are opt-in

Three separate gates exist before a single keystroke or click reaches the
device, on purpose:

1. **`--allow-control` at process startup** (CLI flag or MCP server flag).
   Without it, this client never even opens the `hidrpc` WebRTC data channel -
   it is *structurally* incapable of sending input, not merely refusing to at
   the call site. A read-only MCP server registers exactly `jetkvm_status` and
   `jetkvm_screenshot`; it does not advertise `jetkvm_keypress`,
   `jetkvm_mouse_move`, or even `jetkvm_release_all` without the control gate.
2. **The readiness handshake.** Even with the channel open, no input is
   permitted until the device echoes the HID-RPC handshake back - which is what
   makes the firmware honor HID frames at all. A session where that handshake
   does not complete fails to connect, rather than silently dropping every
   keystroke while reporting success.
3. **The control lease** (`internal/jetkvm/owner.go`). Every keyboard/mouse
   command goes through a single exclusive lease whose guarantees are spelled
   out below.

## What the control lease actually guarantees

These are the properties the tests pin down. They are stated narrowly on
purpose - the previous version of this document claimed more than the code
delivered.

- **Exclusivity.** At most one holder at a time. A second acquirer either waits
  (bounded by its context) or is told the lease is held.
- **Generation validation at the send boundary.** Each holder receives a fresh,
  never-reused token. Every frame is re-validated against the currently-active
  token at the last moment before it is written to the channel - not at the call
  site. A frame authorized by a lease that has since ended is *dropped*, and the
  caller is told so; it is never delivered late.
- **Terminal, pre-emptive neutralization.** However a lease ends - explicit
  release, context cancellation, the inactivity timeout, disconnect, or process
  shutdown - the generation is revoked *first*, then neutralization frames are
  written from a priority queue that jumps ahead of any queued input. A
  neutralization frame is therefore the last frame written for that generation.
- **No cursor movement.** Neutralization clears buttons with a zero-delta
  *relative* mouse report, never an absolute pointer report. Clearing state
  cannot move the attached computer's cursor as a side effect.
- **Truthful failure.** If neutralization cannot be confirmed on the wire, the
  error says so and the client keeps believing input is held, rather than
  reporting a clean release it cannot back up.

What this does **not** guarantee: that input is definitely no longer held on the
attached computer. This client can only prove what it wrote to the channel. A
device that accepts a frame and fails to act on it is outside what any client
can verify.

## Single-session firmware and operation lifetime

The pinned firmware has one global `currentSession`. A new local or cloud
WebRTC session replaces it, closes the previous peer after one second, and
receives all subsequent native video frames. Keeping an eager MCP connection
open would therefore monopolise the device while idle; reconnecting a
persistent client could repeatedly steal it from a browser or another process.

The MCP process holds no idle device connection. Every tool call:

1. acquires an in-process gate and a cooperative, same-user Darwin/Linux file
   lock;
2. connects one fresh session with HID disabled unless that specific server was
   started with `--allow-control`;
3. performs one operation;
4. confirms control release where applicable and closes the session; and
5. returns success only after cleanup succeeds.

The lock lives under the current user's cache directory in a private `0700`
directory. The stable hashed lock file is opened without following symlinks,
must be a singly-linked regular file owned by the user with private
permissions, and is never unlinked (unlinking an active advisory lock permits
two processes to lock two different inodes). It contains no device URL,
credential, PID, or other data.

This coordination is deliberately cooperative and limited to MCP processes:
one-shot CLI commands, browsers, cloud clients, old binaries, different OS
users, and DNS/IP aliases can still cause a firmware handoff. Do not run a CLI
command while an MCP control operation is in flight. Status and screenshot have
an explicit maximum of two total attempts, and only a classified terminal
transport/handoff failure qualifies for the second fresh session. Cancellation, timeout, authentication,
compatibility, RPC, decode, validation, and filesystem errors are never
retried. **Control operations are never replayed.** The HID layer's two
attempts to send canonical all-zero neutralisation frames are a separate safety
mechanism; they can never replay the original key or pointer input.

Screenshots record a monotonic frame-generation boundary after the request
begins and require a strictly newer generation. A stopped stream or deadline
therefore produces an error instead of returning the last cached pixels.

## What is deliberately not implemented

Virtual media mounting, ATX/power control, firmware update/OTA, and
network/Tailscale/MQTT configuration are simply not wired up in
`internal/jetkvm`. This isn't a missing feature so much as a scope boundary:
this tool's job is inspection and (opt-in) input, not device administration.
Adding any of those would need its own explicit gate and its own review, the
same way `--allow-control` did.

## Secrets handling

- Credentials are never accepted as command-line arguments, which would expose
  them in process listings via `ps`. Only environment variables
  (`JETKVM_PASSWORD`, `JETKVM_AUTH_TOKEN`) or stdin (`--password-stdin`) are
  accepted. A literal environment assignment typed at a shell can still enter
  shell history, so inject it through a secret manager; for CLI commands,
  `--password-stdin` avoids putting the value in either arguments or the shell
  command. Treat the parent process environment as sensitive too.
- `--password-stdin` is **rejected by `serve`**. The MCP protocol owns
  stdin/stdout; consuming a line of stdin for a password would eat the client's
  first JSON-RPC message. The two modes are genuinely incompatible, so this
  fails fast with an explanation instead of corrupting the transport.
- Internally, credentials are wrapped in a `Secret` type
  (`internal/jetkvm/redact.go`) whose `String()`/`GoString()`/`MarshalJSON()` all
  return `<redacted>`. `Secret.Expose()` is confined to the HTTP auth adapter's
  two outbound boundaries: the login request body and pre-supplied session
  cookie installation.
- **Authentication response bodies are never quoted back.** A body from an
  `/auth/*` endpoint is dropped entirely rather than redacted, because a
  reflected credential need not look like one. Other error bodies are scrubbed
  for credential-shaped key/value pairs and long opaque tokens, then truncated.
- **Errors are redacted centrally at both public adapters.** CLI parse/runtime
  failures cross one top-level renderer and never reflect raw flag/command
  values. MCP execution failures cross one result renderer, while a receiving
  middleware replaces SDK-generated `tools/call` schema/unknown-tool errors
  with a fixed diagnostic. URLs lose their userinfo, query string, and fragment before
  appearing in output (Go's `*url.Error` embeds the full URL). Transport errors
  and device-controlled RPC payloads are reduced to safe text rather than
  retaining an unwrap path to an unredacted original. Joined send,
  neutralisation, and cleanup failures preserve each useful safe reason.
- **The FFmpeg subprocess gets an allowlisted environment**, not this process's.
  `exec.Cmd` inherits everything when `Env` is nil, which would hand the decoder
  `JETKVM_PASSWORD` and any secrets belonging to the agent or secret manager
  that launched this process. It receives only `PATH`, the temp-dir variables,
  and (on Windows) `SystemRoot`/`WINDIR`. `LD_LIBRARY_PATH`, `DYLD_*` and `HOME`
  are excluded deliberately. Its PNG stdout and diagnostic stderr are
  independently memory-bounded; PNG dimensions/pixel count are checked before
  pixel allocation, and in-process decoding remains context-cancelable.
- **MCP stdout stays protocol-clean.** Diagnostics go to stderr, and a test
  enforces at source level that the MCP package never writes to stdout.
- Session credentials (the device's `authToken` cookie) live only in the fresh
  connection's in-memory cookie jar; nothing credential-bearing is written to
  the coordination lock or any other file.
- **The MCP screenshot tool writes no files at all.** It returns the image in
  the response. An earlier version accepted a caller-supplied `output_path`,
  which handed any MCP caller arbitrary file overwrite (plus traversal and
  symlink following) on the host running the server; that parameter is now
  rejected rather than ignored. The CLI still takes a path - it comes from the
  local user's own command line - and writes atomically.

## Release workflow and provenance

CI runs format, vet and explicit integration-tag compilation, pinned
`actionlint`, `govulncheck`, and `gitleaks`, unit/race/soak tests, and a native Darwin/Linux amd64/arm64
matrix. Branch protection can depend on the stable aggregate `test` job rather
than unstable matrix-generated context names. Actions are referenced by
immutable commit SHA, checkout credentials are not persisted, permissions are
read-only by default, and no `pull_request_target` workflow is used.
The manifest-available setup-go version is only a bootstrap: both CI and release
force and assert the exact `.go-version` toolchain before any project Go command,
and include `.go-version` in the module/build cache key.

A strict stable-semver `v*` tag may build only after its commit is proved to be
on `main` with a successful aggregate `test` check. It builds four
`CGO_ENABLED=0`, trimmed binaries twice and compares the bytes, preserving Go's
clean exact VCS revision/time metadata while also injecting the exact tag
version, resolved commit, and UTC commit date. Deterministic archives are also
built twice and compared, then receive sorted SHA-256 checksums, `BUILDINFO`,
and a GitHub/Sigstore build-provenance attestation. The workflow can create or
update a **draft** GitHub release only and fails closed if an existing release
is already published; it has no publish step. FFmpeg is not bundled and remains
an explicit runtime dependency for screenshots.

## Firmware/protocol trust

This client's understanding of JetKVM's local auth/signaling/WebRTC/HID protocol
comes entirely from reading `jetkvm/kvm`'s own source (see README.md's firmware
compatibility section for the exact commit). That source is not a published API
contract, so this client verifies the signaling handshake's shape before
proceeding, produces a named `CompatibilityError` on mismatch instead of
misbehaving silently, and keeps protocol-parsing code behind narrow interfaces
so a firmware change can be absorbed locally.

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
   the call site. An agent talking to a server started without this flag cannot
   discover `jetkvm_release_all`, `jetkvm_keypress`, `jetkvm_type`, or
   `jetkvm_mouse_move` in `tools/list`, let alone call them.
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
  return `<redacted>`. `Secret.Expose()` is called at exactly one place:
  building the outbound HTTP request.
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
- **The FFmpeg subprocess gets an allowlisted environment**, not this process's.
  `exec.Cmd` inherits everything when `Env` is nil, which would hand the decoder
  `JETKVM_PASSWORD` and any secrets belonging to the agent or secret manager
  that launched this process. It receives only `PATH`, the temp-dir variables,
  and (on Windows) `SystemRoot`/`WINDIR`. `LD_LIBRARY_PATH`, `DYLD_*` and `HOME`
  are excluded deliberately.
- **MCP stdout stays protocol-clean.** Diagnostics go to stderr, and a test
  enforces at source level that the MCP package never writes to stdout.
- Session credentials (the device's `authToken` cookie) live only in an
  in-memory cookie jar for the process's lifetime; nothing is written to disk.
- **The MCP screenshot tool writes no files at all.** It returns the image in
  the response. An earlier version accepted a caller-supplied `output_path`,
  which handed any MCP caller arbitrary file overwrite (plus traversal and
  symlink following) on the host running the server; that parameter is now
  rejected rather than ignored. The CLI still takes a path - it comes from the
  local user's own command line - and writes atomically.

## Firmware/protocol trust

This client's understanding of JetKVM's local auth/signaling/WebRTC/HID protocol
comes entirely from reading `jetkvm/kvm`'s own source (see README.md's firmware
compatibility section for the exact commit). That source is not a published API
contract, so this client verifies the signaling handshake's shape before
proceeding, produces a named `CompatibilityError` on mismatch instead of
misbehaving silently, and keeps protocol-parsing code behind narrow interfaces
so a firmware change can be absorbed locally.

# Changelog

## Unreleased — planned v0.4.0

The source version remains `0.3.0` until the v0.4.0 release candidate is
staged; no v0.4 tag or release exists yet. These are the product, test, and
release-engineering changes merged after the v0.3.0 tag. They close every
porting gap identified in the v0.3.0 release-time note below; that historical
record is retained, and the host-local coordinator remains intentionally
superseded rather than ported.

### Security and correctness

- [`#14`](https://github.com/LeeroyDing/jetkvm-mcp/pull/14) restored
  `CanonicalBaseURL` validation at the library boundary and hardened CLI
  parsing/error rendering so hostile URL or flag input is rejected before
  credential resolution or network I/O, without reflecting the input.
- [`#15`](https://github.com/LeeroyDing/jetkvm-mcp/pull/15) restored shared
  keypress and pointer validation across CLI and MCP adapters, including the
  modifier/button ranges that previously could narrow silently to a byte.
- [`#21`](https://github.com/LeeroyDing/jetkvm-mcp/pull/21) upgraded the MCP Go
  SDK to v1.7.0 and retained the strict-input contract by mapping SDK argument
  validation failures back to JSON-RPC `InvalidParams`.
- [`#23`](https://github.com/LeeroyDing/jetkvm-mcp/pull/23) drops non-auth HTTP
  error bodies that exactly reflect a configured password or token, keeps
  FFmpeg availability failures actionable without echoing an executable path,
  and checks decoder availability before a screenshot opens a device session
  or waits for video.
- MCP schema-validation failures now use a fixed `InvalidParams` message rather
  than returning SDK text that could quote a caller-supplied value or property
  name. Explicit `--password-stdin` now overrides all configured credential
  sources, and Keychain lookup observes command cancellation/deadlines.
- CLI keypress and mouse-move commands now make neutralization part of their
  success boundary: a failed release returns an error before success JSON is
  printed, even when the input report itself was sent successfully.

### Reliability

- [`#13`](https://github.com/LeeroyDing/jetkvm-mcp/pull/13) made only
  transport-establishment deadlines in the loopback WebRTC tests
  extendable by CI; timeout-classification and fast-failure assertions retain
  their fixed budgets.
- [`#16`](https://github.com/LeeroyDing/jetkvm-mcp/pull/16) fixed both sides of
  trickle-ICE ordering by bounding and flushing candidates that arrive before
  the offer or answer. Configured loopback device URLs and test peers now use
  loopback-only ICE, and race lanes are serialized to avoid cross-package
  runner starvation.
- Exhausting the bounded MCP retry loop now returns a stable classified
  `unreachable` error instead of relying on a process panic. Behavior-focused
  tests pin connection-only retries, one-shot control operations, gate/backoff
  cancellation, bad-frame session discard, screenshot preflight ordering, and
  ordered mouse/release HID forwarding.

### Versioning, release automation, and dependencies

- [`#17`](https://github.com/LeeroyDing/jetkvm-mcp/pull/17) added one
  `internal/buildinfo` version source, JSON `--version` output, and a
  prompt-proof `doctor` command whose device probe is explicitly opt-in.
- [`#18`](https://github.com/LeeroyDing/jetkvm-mcp/pull/18) added the
  reproducible four-target release workflow. Tag builds require the tag to
  match the source version and checked-out commit, that commit to be on `main`,
  and its aggregate `test` check to have succeeded. `workflow_dispatch` cannot
  create a tag, attestation, or GitHub release; it uploads only a workflow
  artifact.
- [`#22`](https://github.com/LeeroyDing/jetkvm-mcp/pull/22) updated the release
  dry-run hygiene allowlist for the reviewed OAuth module checksum and carried
  the current immutable checkout/setup-go action pins into that workflow.
- GitHub Actions and Go dependencies were refreshed: checkout v7.0.1
  ([`#5`](https://github.com/LeeroyDing/jetkvm-mcp/pull/5)), setup-go v7.0.0
  ([`#6`](https://github.com/LeeroyDing/jetkvm-mcp/pull/6)), jsonschema-go
  v0.4.3 ([`#8`](https://github.com/LeeroyDing/jetkvm-mcp/pull/8)), Pion RTCP
  v1.2.17 ([`#9`](https://github.com/LeeroyDing/jetkvm-mcp/pull/9)), Pion RTP
  v1.10.5 ([`#19`](https://github.com/LeeroyDing/jetkvm-mcp/pull/19)), and
  Pion WebRTC v4.2.18
  ([`#20`](https://github.com/LeeroyDing/jetkvm-mcp/pull/20)).

## v0.3.0 (2026-08-15)

Reliability release driven by the 2026-08-14 production outage, in which the
1Password service-account quota blocked JetKVM MCP cold start for a day. The
goal of this release: **cold start performs zero external secret-provider
calls**, device failures are bounded and classified, and the reliability test
harness covers the failure modes that previously went untested.

The guarded cutover session (Beads `oc-byj.4`) completed and was accepted on
2026-08-15: production now runs the 0.3.0/build 5 bundle with Keychain-only
credential resolution.

### Added — Keychain-native credential resolution (`oc-byj.1`)

- `jetkvmctl` now reads the device password directly from the macOS login
  Keychain when `JETKVM_PASSWORD_KEYCHAIN_SERVICE` and
  `JETKVM_PASSWORD_KEYCHAIN_ACCOUNT` are both set, invoking the absolute
  `/usr/bin/security find-generic-password … -w` path so credential
  resolution cannot be redirected by a modified `PATH`.
- Keychain output is accepted only as a single non-empty line; malformed or
  failed lookups fall back to `JETKVM_PASSWORD` when present, so an existing
  deployment keeps working during migration. `JETKVM_AUTH_TOKEN` still skips
  password resolution entirely. Subprocess stderr never crosses the
  redaction boundary.
- Tests run against a hermetic fake `security` binary placed first on
  `PATH`; the suite never touches a real Keychain.

### Changed — bounded retries and a stable error taxonomy (`oc-byj.2`)

- MCP server startup is now device-independent: the first tool call connects
  lazily, so a server process starts (and lists tools) with the device
  unreachable, powered off, or mid-reboot.
- Read-only calls make at most three total attempts with 75 ms exponential
  jittered backoff capped at 300 ms; the smaller of the caller deadline or
  the server `--timeout` is always the outer budget.
- Only classified `unreachable` failures are retried. Authentication
  failures, timeouts, and bad protocol frames return immediately with stable
  `auth-failed`, `timeout`, and `bad-frame` markers. State-changing
  operations may retry connection setup but never the operation itself once
  started.
- Reconnection replaces the owned session in-process. There is no background
  reconnect loop, no self-respawn, and no unbounded attempt path — a
  deliberate guard against the single-session firmware's eviction semantics.
  RPC frames are capped at 64 KiB and HTTP responses at 1 MiB.
- MCP `serverInfo` version is now `0.3.0`.

### Added — expanded mock-device reliability harness (`oc-byj.3`)

- The from-scratch loopback fake JetKVM (real HTTP + signaling + Pion WebRTC
  negotiation, no live hardware) now also exercises: auth rejection, delayed
  HTTP/RPC responses, truncated and oversized RPC responses, transient HTTP
  503 recovery, dropped data-channel reconnection, and persistent-failure
  attempt counting with exact thread-safe attempt/status assertions.
- `go test -race` and repeated-run (`-count=5`) checks pass; no test uses a
  device address, external service, or platform credential store.

### Deployment (live since the `oc-byj.4` cutover, 2026-08-15)

- `deploy/openclaw-jetkvm-mcp-wrapper.sh`: the production OpenClaw wrapper
  reduced to a plain `exec` with a cleared environment. No 1Password
  resolver, no `jq`, no external secret-provider subprocess at cold start.
  The Keychain item add + ACL grant was performed in that guarded cutover
  session.

### Ported to v0.2.0 production parity (`oc-byj.4` staging follow-up)

The first staging pass (2026-08-15) recorded three deltas where this
v0.1.1-lineage tree diverged from the accepted v0.2.0 production contract.
All three are now ported, so the staged v0.3.0 catalog and screenshot
semantics match accepted production behaviour and the dependency graph
matches the public security baseline. The cutover artifact is the 0.3.0
**build 5** bundle produced at this release commit (superseding the
pre-port build 4):

- **Two-tool read-only MCP catalog (`oc-q3w.5` parity).**
  `jetkvm_release_all` now requires `--allow-control` and is registered
  alongside the other HID-capable tools. Without control the catalog is
  exactly `jetkvm_status` + `jetkvm_screenshot`, as v0.2.0 production
  acceptance required; with control it is exactly five tools. A release
  that did not actually release input is a tool error, never a quiet
  success.
- **Post-request screenshot freshness (`oc-q3w.3` parity).**
  `CaptureScreenshot` records the completed frame generation as its request
  boundary and waits for a strictly newer decodable frame. A successful
  screenshot can never be a cached frame from before the call (or before a
  preceding control action); if no newer frame arrives within the deadline
  the call fails truthfully. The old `fresh = "younger than 5 s"` window is
  gone — the compatibility `fresh` field is always `true` on success, and
  stopped-stream errors surface even when a stale cached frame exists.
- **Pion dependency security parity.** `pion/stun/v3` v3.0.2 → v3.1.5 and
  `pion/dtls/v3` v3.0.9 → v3.1.4 (adding `pion/transport/v4` v4.0.2), the
  exact versions accepted v0.2.0 production ships after the public
  repository's Dependabot security bumps. `govulncheck` was already clean
  before the bump (nothing reachable); this closes the Dependabot-parity
  gap rather than a known reachable vulnerability.

### Lineage and known gaps

> **Historical release-time record:** the text below describes the v0.3.0
> cutover tree, not the current tree. The porting gaps it names are now closed
> by the **Unreleased** changes above; the host-local coordinator remains
> intentionally superseded.

- This tree is the **private v0.1.1-lineage development repository** plus
  the three changes above. The public repository's v0.2.0 release
  (`78eb2a8`, built from the separate `jetkvm-mcp-v020` worktree) contains
  improvements that are **not in this tree**, notably:
  - `CanonicalBaseURL` URL validation/userinfo rejection and related CLI
    error-rendering hardening;
  - control-plane input validation (`control_validation.go`) — production
    does not enable control tools, so not cutover-relevant;
  - the host-local multi-process session coordinator (superseded here by
    lazy sessions + bounded in-call reconnect, by design);
  - `internal/buildinfo` and the `--version`/`doctor` commands (this tree
    reports its version only via MCP `serverInfo` and the app bundle's
    `Info.plist`);
  - the reproducible public release CI pipeline.
- The guarded cutover proceeded without porting the remaining public
  v0.2.0 behaviours above — an explicit decision recorded in that session;
  none of them changes the accepted production tool contract.

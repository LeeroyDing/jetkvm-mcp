# Changelog

## Unreleased

## v0.5.0 (2026-08-22)

This release raises the authoritative source version to `0.5.0`. The annotated
`v0.5.0` tag points at this release commit; publication of the separately staged
GitHub release remains a human-approved step. These are the product, test, and
release-engineering changes after the v0.4.0 tag.

### Changed

- The post-roadmap tool-surface audit now rejects all-zero padded key chords,
  keeps shared validation errors free of MCP/CLI-specific parameter spelling,
  restores committed fuzz-smoke coverage for every committed target, and
  corrects stale scroll-lease and opt-in catalog documentation. Click, double-click,
  and drag now reject zero button masks across MCP and CLI instead of reporting
  success for a movement-only sequence. Decoder-dependent MCP calls now run one
  gate-owned FFmpeg availability check per logical operation instead of
  repeating the check inside the core client.
- Hardened the shipped MCP/CLI control surface: every control call now uses the
  exclusive lease, MCP dangerous handlers have whole-call admission, canceled
  HID/RPC work is checked again at the send boundary, and HID readiness
  requires an exact handshake/version echo. RPC now joins HID in enforcing a
  bounded outbound buffer. Schema limits/defaults, timeout and structured-result
  contracts, nil decoder handling, and control-disabled error identity are now
  consistent and covered by regression tests.
- Restored the accepted three-tool production MCP catalog when
  `--allow-control` is absent: `jetkvm_status`, `jetkvm_screenshot`, and
  `jetkvm_read_text` remain available, while `jetkvm_wait_stable` is now
  registered only in the opt-in catalog (`oc-lfk`).
- Marked `jetkvm_release_all` with the same `DANGEROUS:` description prefix
  and non-read-only, destructive, non-idempotent mutator annotations as every
  other control-gated input tool (`oc-xf2`).

### CI and internal

- Added the `v0.4.0` staging-cutover runbook and reproducibility receipt for
  the artifact-only `workflow_dispatch` release path, including deterministic
  local build evidence and explicit no-tag, no-attestation, and
  no-release-mutation guardrails (`oc-2gf`, child of `oc-6jb`).
- Added a loopback HTTP/WebRTC MCP harness that exercises every registered tool
  through the production JSON-RPC boundary, including valid, invalid,
  authorization, payload, and no-device-I/O cases
  ([`#45`](https://github.com/LeeroyDing/jetkvm-mcp/pull/45)).
- Expanded fuzz and regression coverage for URL, HID, schema, tool arguments,
  and CLI parser/help behavior, including drag, scroll, and key-sequence cases;
  made the CI fuzz-smoke shards run their discovered targets sequentially
  ([`#37`](https://github.com/LeeroyDing/jetkvm-mcp/pull/37),
  [`#54`](https://github.com/LeeroyDing/jetkvm-mcp/pull/54),
  [`#71`](https://github.com/LeeroyDing/jetkvm-mcp/pull/71),
  [`#73`](https://github.com/LeeroyDing/jetkvm-mcp/pull/73),
  [`#74`](https://github.com/LeeroyDing/jetkvm-mcp/pull/74),
  [`#75`](https://github.com/LeeroyDing/jetkvm-mcp/pull/75)).
- Added grouped weekly Go-module Dependabot updates and a pinned Gitleaks scan,
  refreshed release-workflow Actions and Pion transport, and synchronized the
  shipped CLI/MCP documentation with the 17-tool catalog
  ([`#28`](https://github.com/LeeroyDing/jetkvm-mcp/pull/28),
  [`#29`](https://github.com/LeeroyDing/jetkvm-mcp/pull/29),
  [`#30`](https://github.com/LeeroyDing/jetkvm-mcp/pull/30),
  [`#38`](https://github.com/LeeroyDing/jetkvm-mcp/pull/38),
  [`#54`](https://github.com/LeeroyDing/jetkvm-mcp/pull/54),
  [`#56`](https://github.com/LeeroyDing/jetkvm-mcp/pull/56), and
  [`#72`](https://github.com/LeeroyDing/jetkvm-mcp/pull/72)).

### Added

- `jetkvm_read_text` (`oc-5he.6`) and `jetkvmctl read-text` capture one
  request-fresh frame and return OCR text without returning or persisting the
  image. Both surfaces share screenshot cropping and down-scaling, remain
  read-only, and use a runtime-detected Tesseract subprocess behind a narrow
  OCR interface. Missing Tesseract produces a typed, actionable unavailable
  error before any device session is opened.
- `jetkvm_wait_for_text` (`oc-5he.7`) and `jetkvmctl wait-for-text` poll
  request-fresh screenshots through a replaceable Tesseract OCR engine until
  a bounded literal substring or regular expression appears. Both operations
  are read-only; matching the accepted `jetkvm_wait_stable` boundary, the MCP
  tool is registered only with `--allow-control` while the CLI remains
  ungated. They share strict interval/timeout validation, preflight FFmpeg and
  OCR availability before connecting, report the recognized match, elapsed
  time, and frame count, and return polling deadline exhaustion as a
  structured timeout result rather than a tool error.
- `jetkvm_mouse_button` (`oc-5he.8`) and `jetkvmctl mouse-button` add
  dangerous, `--allow-control`-gated discrete press and release actions for
  the named left, right, and middle mouse buttons. Zero-delta relative reports
  preserve the cursor position, while MCP tracks the combined held-button
  state so agents can compose custom gestures across calls. Release-all and
  every session-ending neutralization path clear the tracked buttons.
- `jetkvm_hold_key` (`oc-5he.5`) and `jetkvmctl hold-key` add a dangerous,
  `--allow-control`-gated press-hold-release operation for named keyboard
  chords. Required durations are validated in the inclusive 1 through 5000
  millisecond range before any HID call, the wait is
  context-interruptible, and lease neutralization guarantees a release attempt
  even after cancellation or timeout.
- `jetkvm_wait_stable` (`oc-4kc`) polls successive request-fresh video frames
  until the changed-pixel fraction remains at or below a configurable
  threshold for the required consecutive comparisons. The read-only MCP tool
  and matching `jetkvmctl wait-stable` command share validated threshold,
  stable-frame, and poll-interval defaults and bounds; unit and fuzz coverage
  pins settling, timeout, resolution changes, and option validation.
- `jetkvm_key_sequence` (`oc-5he.4`) and `jetkvmctl key-sequence` add a
  dangerous, `--allow-control`-gated sequence of 1 through 64 named keyboard
  chords. The complete ordered list is resolved and validated before the first
  send, each chord is released before the next, and the optional inter-chord
  delay shares `type`'s 0 through 500 millisecond range and default of zero.
- `jetkvm_scroll` (`oc-5he.1`) and `jetkvmctl scroll` add control-gated
  vertical and horizontal wheel input. Both axes are validated against the HID
  descriptor's signed `[-127,127]` range; positive `dy` is up and positive
  `dx` is right, and a zero/zero no-op is rejected. Because this firmware drops
  binary `TypeWheelReport`, the implementation uses its legacy `wheelReport`
  JSON-RPC method, with the gate re-checked at the catalog/CLI,
  retrying-device, and `Client` layers. The operation acquires and neutralizes
  the process-local HID lease for exclusivity/readiness, requires an
  acknowledgement, and is never retried after it starts. The legacy RPC frame
  cannot carry the lease generation token, and its acknowledgement cannot prove
  host-side delivery.
- `jetkvm_drag` (`oc-5he.2`) adds a control-gated press-hold-move-release
  gesture between two validated absolute coordinates. Callers can request up
  to 256 intermediate pointer moves for smoother drag-and-drop or text
  selection. The matching `jetkvmctl drag` command uses the same bounds,
  defaults, control lease, and `--allow-control` gate. Its button mask must be
  in the inclusive range 1 through 31.
- `jetkvm_double_click` (`oc-5he.3`) adds one control-gated convenience call
  that moves to an absolute position, then presses and releases a validated
  nonzero button bitmask twice at the same coordinates.
- `jetkvm_click` (`oc-0vr`) adds one control-gated call that moves to an
  absolute position, presses a validated nonzero button bitmask, and releases
  it at the same coordinates. The matching `jetkvmctl click` command uses the
  same pointer validation and requires
  `--allow-control`.
- `jetkvm_screenshot` ([`#34`](https://github.com/LeeroyDing/jetkvm-mcp/issues/34), `oc-hqw`) now supports
  in-memory PNG/JPEG encoding, JPEG quality, down-scaling without up-scaling, and bounded source-pixel crops.
  The default remains a request-fresh PNG, result MIME/dimensions reflect the delivered image, and the MCP tool
  still accepts no output path or filesystem-write option.
- `jetkvm_type` (`oc-24u`) types a bounded UTF-8 string through the existing
  control-gated keypress path. A pure US-layout mapper covers all printable
  ASCII plus Enter and Tab, rejects unsupported runes with their position
  before any input is sent, and is covered exhaustively plus by
  `FuzzTypeStringMapping`. The matching `jetkvmctl type` command uses the same
  mapping, validation, delay bounds, and per-key neutralization behavior.
- Added the dangerous, `--allow-control`-gated `jetkvm_key_combo` MCP tool and
  matching `jetkvmctl key-combo` command. Both resolve a small canonical
  registry through the shared control validator, send the complete chord in
  one HID keyboard report under the existing control lease, and neutralize
  keyboard state before reporting success. Unit, fuzz, MCP, and CLI coverage
  pin name normalization, invalid input, report forwarding, and control gates.

## v0.4.0 (2026-08-15)

This release candidate raises the authoritative source version to `0.4.0`.
Tagging and publication remain separate human-approved steps; staging this
source change creates neither. These are the product, test, and
release-engineering changes after the v0.3.0 tag. They close every porting gap
identified in the v0.3.0 release-time note below; that historical record is
retained, and the host-local coordinator remains intentionally superseded
rather than ported.

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
- [`#25`](https://github.com/LeeroyDing/jetkvm-mcp/pull/25) makes MCP
  schema-validation failures use a fixed `InvalidParams` message rather than
  returning SDK text that could quote a caller-supplied value or property
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
- [`#25`](https://github.com/LeeroyDing/jetkvm-mcp/pull/25) makes exhaustion of
  the bounded MCP retry loop return a stable classified `unreachable` error
  instead of relying on a process panic. Behavior-focused tests pin
  connection-only retries, one-shot control operations, gate/backoff
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
- [`#26`](https://github.com/LeeroyDing/jetkvm-mcp/pull/26) makes CI run
  Staticcheck 2026.1 from a Go module tool pin, perform least-privilege Go
  CodeQL analysis on pushes and pull requests without a recurring schedule,
  and fail closed if aggregate atomic statement coverage falls below 80.0% (a
  2.7-point margin below the measured 82.7% baseline).
- Release workflow artifacts now use a path-safe source-version/run identity
  shared by upload and tag-only consumers, so release branches containing `/`
  can complete artifact-only dry runs without weakening tag release gates.
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
> by the **v0.4.0** changes above; the host-local coordinator remains
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

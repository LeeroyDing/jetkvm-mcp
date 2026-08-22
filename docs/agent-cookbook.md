# Agent cookbook for the v0.5 MCP surface

This cookbook shows an MCP agent how to observe and control one attached computer with the 17 tools shipped by
jetkvm-mcp v0.5. The argument examples below are the exact JSON `arguments` objects for the named tool; they do
not include an MCP client's surrounding call envelope. Treat every coordinate and piece of visible text as an
observation to refresh, not as durable UI state.

The source of truth is the registration code in
[`internal/mcpserver/tools.go`](../internal/mcpserver/tools.go). Core limits and semantic validation live in
[`internal/jetkvm`](../internal/jetkvm). All tool argument roots are strict objects: `null`, unknown properties,
wrong types, missing required properties, and schema-declared out-of-range values are rejected as invalid
parameters. The nested `region` object is strict too.

## Before you act

Start the server without `--allow-control` when observation is enough. That catalog contains exactly
`jetkvm_status`, `jetkvm_screenshot`, and `jetkvm_read_text`. Starting with `--allow-control` exposes all 17
tools, including the two read-only wait tools and the 12 tools that send or neutralize input. FFmpeg is required
for image capture; local Tesseract availability is additionally required for OCR and text waits.

Use this loop for control work:

1. Capture a fresh full-frame screenshot.
2. Read text when labels help, remembering that OCR returns text, not bounding boxes.
3. Perform one bounded action.
4. Wait for a known condition or capture another fresh screenshot before acting again.

Pointer tools use the HID logical plane `0..32767` on each axis, while screenshot regions use source-image
pixels. The server does not convert between them. As agent-side proportional guidance, a point `(px, py)` in an
uncropped full source frame of size `(sourceWidth, sourceHeight)`, when both dimensions exceed one, can be
estimated as:

```text
x = round(px * 32767 / (sourceWidth - 1))
y = round(py * 32767 / (sourceHeight - 1))
```

This mapping is an inference, not a server guarantee; use coordinate 0 on a hypothetical one-pixel axis. Use the
screenshot result's full `sourceWidth` and `sourceHeight`, map cropped or scaled coordinates back to the full
source frame first, aim at the center of a large target, and verify with a fresh frame. Named button mask values
are 1 (left), 2 (right), and 4 (middle); click-like tools accept any nonzero five-bit mask through 31.

## Shipped tool surface

“Always” means the tool is registered without `--allow-control`. “Opt-in read-only” means it sends no input and
takes no control lease, but is registered only with that flag. “Opt-in control” means it is registered only with
the flag and uses the internal control lease.

### Always-registered read-only tools

| Tool | Exact input schema, defaults, and clamps |
|---|---|
| `jetkvm_status` | `{}` only. |
| `jetkvm_screenshot` | All optional: `format` is `"png"` (default) or `"jpeg"`; `quality` is integer 1–100, valid only for JPEG and effectively defaults to 80 for JPEG; `scale` is a positive finite number, defaults to 1, and values above 1 clamp to 1; `region` is `{x,y,width,height}` with all four fields required, `x`/`y` integers 0–2,147,483,647 and `width`/`height` integers 1–2,147,483,647. The region must also fit the fresh source frame. Crop precedes scale. |
| `jetkvm_read_text` | All optional: `scale` and `region` have exactly the screenshot bounds and behavior above. It has no `format` or `quality` property. |

### Opt-in read-only tools

| Tool | Exact input schema, defaults, and clamps |
|---|---|
| `jetkvm_wait_stable` | All optional: `threshold` is a finite number 0–1 (default 0.01); `stable_frames` is an integer 1–2,147,483,647 (default 2); `poll_interval_ms` is an integer 0–9,223,372,036,854 (default 250). A deadline returns a tool error with partial observations. |
| `jetkvm_wait_for_text` | Required `text` is a non-empty UTF-8 string of at most 4,096 runes. Optional `regex` is boolean (default false), `interval_ms` is integer 100–10,000 (default 500), and `timeout_ms` is integer 100–300,000 (default 10,000); interval must not exceed timeout. Matching is case-sensitive literal matching unless `regex` selects valid Go/RE2 syntax. A configured timeout returns `timedOut: true` as a successful structured result. The caller deadline or server `--timeout` (default 10s) can end it sooner. |

### Opt-in control tools

| Tool | Exact input schema, defaults, and clamps |
|---|---|
| `jetkvm_keypress` | Required `key`: integer 0–255 USB HID usage code. Optional `modifier`: integer 0–255 bitmask (default 0). |
| `jetkvm_type` | Required `text`: string of at most 4,096 runes; printable ASCII, newline, and tab only on a US layout. Optional `delay_ms`: integer 0–500 (default 0). The entire string is validated before input starts. |
| `jetkvm_key_combo` | Required `combo`: supported named chord of at most 64 runes. Names are case-insensitive; plus, hyphen, and whitespace separators normalize equivalently. |
| `jetkvm_hold_key` | Required `combo`: supported named chord of at most 64 runes. Required `hold_ms`: integer 1–5,000. The call returns only after release or an earlier cancellation/failure cleanup attempt. |
| `jetkvm_key_sequence` | Required `combos`: array of 1–64 supported named chords, each at most 64 runes. Optional `delay_ms`: integer 0–500 (default 0). The complete sequence is resolved before the first send. |
| `jetkvm_mouse_move` | Required `x`, `y`: integers 0–32,767. Optional operation-local `buttons`: integer 0–31 (default 0). |
| `jetkvm_mouse_button` | Required `button`: exactly `"left"`, `"right"`, or `"middle"`. Required `action`: exactly `"press"` or `"release"`. Names are case-sensitive. A nonzero aggregate retains a bounded lease across calls. |
| `jetkvm_scroll` | Required `dy`: integer −127–127. Optional `dx`: integer −127–127 (default 0). Both cannot be zero; positive `dy` scrolls up and positive `dx` scrolls right. |
| `jetkvm_click` | Required `x`, `y`: integers 0–32,767. Optional operation-local `button`: integer 1–31 (default 1, left). |
| `jetkvm_drag` | Required `x1`, `y1`, `x2`, `y2`: integers 0–32,767. Optional operation-local `button`: integer 1–31 (default 1); optional `steps`: integer 0–256 (default 0). |
| `jetkvm_double_click` | Required `x`, `y`: integers 0–32,767. Optional operation-local `button`: integer 1–31 (default 1, left). There is no delay parameter. |
| `jetkvm_release_all` | `{}` only. Sends the canonical neutral reports for input interfaces this session may have left holding state. |

The fixed chord registry used by `jetkvm_key_combo`, `jetkvm_hold_key`, and every element of
`jetkvm_key_sequence` is: `alt+tab`, `cmd`, `cmd+space`, `ctrl+alt+del`, `ctrl+c`, `ctrl+shift+t`, `ctrl+v`,
`ctrl+z`, `e`, `enter`, `esc`, `m`, `r`, `t`, and `win`. In particular, bare `tab` is not a registered chord;
type `"\t"` with `jetkvm_type` or click the next field instead.

## Safety model

**Capability and lease gating.** `--allow-control` changes which tools exist; it is not a lease an agent acquires.
Each control tool takes a non-blocking whole-call admission token and acquires or reuses the process-local session
lease internally, so concurrent control calls fail busy instead of interleaving. Ordinary calls neutralize on
exit; named mouse-button state is the sole cross-call retained-holder case and remains retained while its tracked
aggregate mask is nonzero. That holder is bounded by a 30-second watchdog. The lease does not coordinate another
process or the browser UI. See
[why dangerous actions are opt-in](../SECURITY.md#why-dangerous-actions-are-opt-in) and
[what the control lease actually guarantees](../SECURITY.md#what-the-control-lease-actually-guarantees).

**Wire-byte bounds and clamping.** Control integers are checked at full width and rejected before conversion to
wire bytes; they are never wrapped, truncated, or clamped. The only intentional numeric input clamp is `scale`
for `jetkvm_screenshot` and `jetkvm_read_text`: a positive finite value above 1 becomes 1, preventing upscaling.
For the strict-schema versus semantic-error boundary and the complete validation contract, see the existing
[tool reference](../README.md#tool-reference).

## Recipes

The coordinates below are illustrative valid HID coordinates. Recompute them from the current full-frame
screenshot rather than copying them blindly.

### 1. Use the automatic lease and finish neutral

There is no explicit lease-acquisition call. First confirm the session is reachable:

`jetkvm_status`

```json
{}
```

Issue control calls serially. When no named mouse button is retained, a successful one-shot call that sends input
has already acquired, used, and ended its lease before it returns. Calls made during a retained-button gesture
reuse that holder and preserve its named button state. If a call reports busy, do not spin or launch a competing
call; wait for the in-flight operation to finish, then observe the screen again.

Use `jetkvm_release_all` after a cross-call held-button gesture, after cancellation, or whenever the agent cannot
account for input state:

```json
{}
```

Success means the peer SCTP transport acknowledged the applicable neutral reports. It does not prove the
firmware applied them to USB or undo an action the host already performed. If neutralization is unverified, stop
sending control input and continue with read-only observation only.

### 2. Log in from a fresh frame

1. Capture the whole screen with `jetkvm_screenshot` so the image and its source dimensions are current:

   ```json
   {"format":"png","scale":1}
   ```

2. Call `jetkvm_read_text` to confirm the expected sign-in labels. This captures a separate fresh frame and
   returns no bounding boxes:

   ```json
   {}
   ```

3. Because OCR observed a later frame but returned no image, capture another full `jetkvm_screenshot` and use
   this final image for coordinates:

   ```json
   {}
   ```

4. Visually locate the username field in that screenshot, convert its center into HID coordinates, and call
   `jetkvm_click`. Suppose the computed center is `(11200, 12800)`:

   ```json
   {"x":11200,"y":12800,"button":1}
   ```

5. Enter the username with `jetkvm_type`:

   ```json
   {"text":"operator","delay_ms":20}
   ```

6. Re-observe if the form moved, then click the password field and type the actual secret. The secret necessarily
   appears in the MCP arguments and may be retained by the MCP client or transcript; use a trusted client and its
   secret-handling controls, and do not copy the value into notes or OCR expectations:

   ```json
   {"x":11200,"y":15100,"button":1}
   ```

   ```json
   {"text":"replace-with-the-real-password","delay_ms":20}
   ```

   `jetkvm_type` supports printable ASCII, newline, and tab only. An unsupported rune rejects the complete call
   before any character is sent.

7. Submit with the real one-element `jetkvm_key_sequence` schema. `combos` contains named chords, not arbitrary
   text:

   ```json
   {"combos":["enter"]}
   ```

8. Verify a post-login condition with `jetkvm_wait_for_text`, then capture another screenshot. Replace `Welcome`
   with a stable, non-secret string from the target UI:

   ```json
   {"text":"Welcome","interval_ms":500,"timeout_ms":10000}
   ```

Treat `matched: true` as readiness. `timedOut: true` is a normal non-match outcome, not evidence that submission
failed or succeeded.

### 3. Wait for a machine to boot

When the target exposes a stable boot marker, use `jetkvm_wait_for_text` instead of sleeping and guessing. This
example requests the maximum five-minute inner wait. Start the server with a longer outer timeout, such as
`--timeout 5m10s`, and give the MCP call a deadline longer than five minutes; the earliest outer bound still wins:

```json
{"text":"login:","regex":false,"interval_ms":1000,"timeout_ms":300000}
```

On `matched: true`, immediately take a full `jetkvm_screenshot` because the UI may continue changing:

```json
{}
```

For a graphical boot with no dependable text, use `jetkvm_wait_stable` as a weaker readiness signal:

```json
{"threshold":0.01,"stable_frames":3,"poll_interval_ms":500}
```

A stable display might still be a firmware prompt, crash screen, or modal dialog, so inspect it before sending
input. Unlike the structured text-wait timeout, a stable-wait deadline is a tool error carrying partial
observations.

### 4. Drag-select visible text

Capture a full frame, identify the centers of the first and last character to select, and map both points into
the HID plane. Optionally move to the start without pressing and verify the hover/caret location:

`jetkvm_mouse_move`

```json
{"x":8200,"y":11800,"buttons":0}
```

Then select with `jetkvm_drag`; moderate interpolation helps applications that sample motion:

```json
{"x1":8200,"y1":11800,"x2":20500,"y2":11800,"button":1,"steps":16}
```

The operation presses at the start, moves, and clears its operation-local button state at the destination. Verify
the highlight with a fresh screenshot. If the call fails ambiguously, use `jetkvm_release_all` and observe; do
not repeat the drag blindly. Copying afterward with `jetkvm_key_combo` and `{"combo":"ctrl+c"}` affects only the
attached host's clipboard—this MCP surface cannot read that clipboard.

### 5. Scroll a long page without losing position

One `jetkvm_scroll` call sends one bounded wheel event; it has no repeat or count parameter. Scroll down with a
small negative vertical delta:

```json
{"dy":-6}
```

Then let the page settle with `jetkvm_wait_stable` and inspect it:

```json
{"threshold":0.02,"stable_frames":2,"poll_interval_ms":250}
```

Repeat one scroll call at a time until the wanted marker appears, taking screenshots or using
`jetkvm_wait_for_text` between bounded batches. Positive `dy` scrolls up; horizontal scrolling uses `dx`, where
positive is right. Release any button retained by `jetkvm_mouse_button` before scrolling, because scroll cannot
run while that cross-call lease is held.

### 6. Hold a mouse button across calls

Prefer `jetkvm_drag` for an ordinary drag. Use this lower-level recipe only when the application requires
separate press, movement, and release events.

1. Move to the start with `jetkvm_mouse_move` and no operation-local button state:

   ```json
   {"x":9000,"y":10000,"buttons":0}
   ```

2. Press with `jetkvm_mouse_button`:

   ```json
   {"button":"left","action":"press"}
   ```

3. Make one or more `jetkvm_mouse_move` calls. The retained button is combined automatically:

   ```json
   {"x":18000,"y":14000,"buttons":0}
   ```

4. Release the same named button in a guaranteed cleanup path:

   ```json
   {"button":"left","action":"release"}
   ```

On any failure or cancellation, call `jetkvm_release_all` with `{}`. Finish promptly: the retained holder's fixed
30-second watchdog starts at the original press and is not renewed by later gesture calls. Do not call
`jetkvm_click` with the same retained button and expect a click; combining an already-down bit does not create a
button-up transition.

### 7. Recognize that modifier-click is unsupported

A keyboard modifier cannot be held across a mouse click with the v0.5 tools. `jetkvm_hold_key` keeps the
whole-call control admission token until it releases the chord, so a parallel `jetkvm_click` fails busy. A
sequential click occurs after the modifier is released. `jetkvm_keypress` and `jetkvm_key_combo` also neutralize
keyboard state before returning.

Do not approximate modifier-click with overlapping calls or a timed race. If the application has a semantically
equivalent supported named keyboard chord, use that only after confirming the equivalence; otherwise stop and
hand this interaction to a human or a control surface that supports atomic keyboard-plus-pointer state.

### 8. Recover when a click lands wrong

Do not immediately retry. A successful wrong click may have opened a menu, changed focus, or triggered an
irreversible action; a transport error may still mean the click reached the host.

1. If held input state is uncertain, call `jetkvm_release_all` with `{}`. This neutralizes input state but does
   not undo the click.
2. Capture a fresh `jetkvm_screenshot` and optionally call `jetkvm_read_text` with `{}` to identify the new state.
3. If a clearly dismissible modal is visible and Escape is appropriate, use `jetkvm_key_combo`:

   ```json
   {"combo":"esc"}
   ```

4. Wait for settling, capture another screenshot, and move the pointer to the newly computed target before
   clicking:

   ```json
   {"x":14600,"y":17400,"buttons":0}
   ```

5. Verify the pointer/hover state visually, then issue one `jetkvm_click` with the same coordinates:

   ```json
   {"x":14600,"y":17400,"button":1}
   ```

There is no generic undo recipe. If the new screen is ambiguous or the next action could submit, delete, power
off, purchase, or overwrite, stop and ask for human confirmation.

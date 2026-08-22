// Package jetkvm is a browser-free client and session core for a JetKVM
// device: local (non-cloud) authentication, WebRTC signaling, video
// capture, JSON-RPC status/wheel calls and (opt-in, gated) HID control.
//
// It deliberately does not implement or depend on anything from the
// jetkvm/kvm cloud relay, OIDC/Google login, virtual media, ATX/power
// control, firmware update, or any other device-administration RPC method -
// only what's needed for read-only inspection and (when explicitly enabled)
// keyboard/mouse input.
package jetkvm

import (
	"context"
	"errors"
	"fmt"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// Options configures a Connect call.
type Options struct {
	// BaseURL is the device's HTTP base URL, e.g. "http://jetkvm.local"
	// or "http://<device-ip>".
	BaseURL string

	// Credentials authenticates against the device's local web API. Zero
	// value is fine for a device in noPassword mode.
	Credentials Credentials

	// AllowControl gates every keyboard/mouse operation. It controls whether
	// this client negotiates the "hidrpc" data channel and constructs a
	// control lease; Scroll also checks it explicitly before using the legacy
	// JSON-RPC wheel path required by current firmware. This matches the
	// CLI/MCP --allow-control gate at the core client boundary.
	AllowControl bool

	// Decoder overrides the video decode backend. Defaults to
	// &FFmpegDecoder{} when nil.
	Decoder Decoder

	// HTTPTimeout bounds individual HTTP requests. Defaults to 10s.
	HTTPTimeout time.Duration
}

// Client is the single session-owning core for one connected device. Status,
// screenshot, stable-screen waits, and legacy RPC control calls use its
// command lock; binary HID control uses the exclusive lease in owner.go and
// its release-all guarantees.
type Client struct {
	baseURL string
	http    *httpClient
	sig     *signaler
	sess    *session
	decoder Decoder

	deviceID     string
	firmwareVer  string
	allowControl bool

	cmdMu chan struct{} // 1-buffered channel used as a non-reentrant command lock

	control *controlLease // nil unless AllowControl was set
}

// Connect performs the full browser-free handshake described in
// README.md: unauthenticated status probe, login (if credentials were
// given), signaling websocket dial with a firmware-compatibility check,
// then WebRTC offer/answer/ICE and data channel setup. On any failure the
// underlying connection is torn down before returning.
func Connect(ctx context.Context, opts Options) (*Client, error) {
	timeout := opts.HTTPTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	// Validate and canonicalize the device URL before anything else touches
	// it: userinfo, queries, fragments, and non-root paths are rejected here
	// so a credential-bearing or aliased URL can never reach the network,
	// an error message, or a log line. newHTTPClient re-checks as
	// defense-in-depth for any future direct caller.
	baseURL, err := CanonicalBaseURL(opts.BaseURL)
	if err != nil {
		return nil, err
	}
	hc, err := newHTTPClient(baseURL, timeout)
	if err != nil {
		return nil, err
	}
	hc.knownCredentials = []Secret{opts.Credentials.Password, opts.Credentials.AuthToken}

	if _, err := hc.deviceStatus(ctx); err != nil {
		return nil, classifyConnectError("checking device status", err, ErrorKindUnreachable)
	}

	switch {
	case !opts.Credentials.AuthToken.Empty():
		hc.setSessionCookie(opts.Credentials.AuthToken)
	case !opts.Credentials.Password.Empty():
		if err := hc.login(ctx, opts.Credentials.Password); err != nil {
			return nil, classifyConnectError("logging in", err, ErrorKindAuthFailed)
		}
	}

	dev, err := hc.device(ctx)
	if err != nil {
		return nil, classifyConnectError("checking authenticated device session", err, ErrorKindAuthFailed)
	}

	baseURLParsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("jetkvm: invalid base URL: %w", err)
	}
	cookies := hc.hc.Jar.Cookies(baseURLParsed)

	sig, meta, err := dialSignaling(ctx, baseURL, cookies)
	if err != nil {
		return nil, err
	}

	sess, err := establishSession(ctx, sig, dialOptions{
		allowControl: opts.AllowControl,
		// A loopback-hosted device is reachable only over loopback;
		// restricting ICE to loopback candidates removes non-viable
		// interface noise (see dialOptions.loopbackOnlyICE).
		loopbackOnlyICE: isLoopbackHost(baseURLParsed.Hostname()),
	})
	if err != nil {
		_ = sig.close()
		if ErrorKindOf(err) != "" {
			return nil, err
		}
		var compatibilityErr *CompatibilityError
		if errors.As(err, &compatibilityErr) {
			return nil, err
		}
		return nil, newDeviceError(ErrorKindUnreachable, "establishing WebRTC session", err)
	}

	decoder := opts.Decoder
	if decoder == nil {
		decoder = &FFmpegDecoder{}
	}

	c := &Client{
		baseURL:      opts.BaseURL,
		http:         hc,
		sig:          sig,
		sess:         sess,
		decoder:      decoder,
		deviceID:     dev.DeviceID,
		firmwareVer:  meta.DeviceVersion,
		allowControl: opts.AllowControl,
		cmdMu:        make(chan struct{}, 1),
	}
	if opts.AllowControl {
		c.control = newControlLease(sess.hid)
	}
	return c, nil
}

func classifyConnectError(operation string, err error, fallback ErrorKind) error {
	if ErrorKindOf(err) != "" {
		return err
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == 401 || apiErr.StatusCode == 403:
			return newDeviceError(ErrorKindAuthFailed, operation, err)
		case apiErr.StatusCode >= 500:
			return newDeviceError(ErrorKindUnreachable, operation, err)
		}
	}
	return newDeviceError(fallback, operation, err)
}

// lock acquires the command lock, serializing Status, Screenshot, WaitStable,
// and legacy RPC control calls through this Client, and returns an unlock
// func. It respects ctx cancellation so a canceled caller doesn't wait
// forever behind a stuck command.
func (c *Client) lock(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case c.cmdMu <- struct{}{}:
		// If cancellation raced the ready lock case, fail closed and return the
		// slot instead of beginning a command under an already-dead context.
		if err := ctx.Err(); err != nil {
			<-c.cmdMu
			return nil, err
		}
		return func() { <-c.cmdMu }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// DeviceID returns the device ID captured from GET /device at connect time.
func (c *Client) DeviceID() string { return c.deviceID }

// FirmwareVersion returns the deviceVersion string captured from the
// signaling handshake's device-metadata message.
func (c *Client) FirmwareVersion() string { return c.firmwareVer }

// StatusResult is the result of a Status call.
type StatusResult struct {
	DeviceID        string
	FirmwareVersion string
	RPCReachable    bool
}

// Status verifies the RPC data channel is alive by calling the device's
// "ping" method (jsonrpc.go's rpcPing), and reports what was learned about
// the device at connect time.
func (c *Client) Status(ctx context.Context) (StatusResult, error) {
	unlock, err := c.lock(ctx)
	if err != nil {
		return StatusResult{}, err
	}
	defer unlock()

	result := StatusResult{DeviceID: c.deviceID, FirmwareVersion: c.firmwareVer}

	var pong string
	if err := c.sess.rpc.call(ctx, "ping", nil, &pong); err != nil {
		return result, fmt.Errorf("jetkvm: status check failed: %w", err)
	}
	result.RPCReachable = true
	return result, nil
}

// wheelReportRPCParams is the single encoding point for the firmware's legacy
// JSON-RPC wheel shape. At the pinned firmware revision jsonrpc.go requires
// both names, in Y/X order at the handler boundary, even when wheelX is zero.
// Keeping this mapping isolated makes a future firmware correction local.
func wheelReportRPCParams(dx, dy int8) map[string]any {
	return map[string]any{"wheelY": dy, "wheelX": dx}
}

// Scroll sends one stateless wheel event. Positive dy scrolls up and positive
// dx scrolls right, matching the JetKVM HID descriptor and reference UI.
//
// Firmware caveat: TypeWheelReport exists on the binary hidrpc channel, but
// the pinned firmware's hidRPC input switch has no wheel case and drops it.
// The only working path is the legacy wheelReport JSON-RPC method, so this
// operation cannot carry the control lease's generation token. It still takes
// the same process-local lease non-blockingly and holds it through the RPC, so
// a wheel event cannot bypass a competing keyboard/pointer holder. The
// AllowControl check remains defense in depth, and success requires both the
// device's RPC acknowledgement and lease neutralization. It is intentionally
// not retried after send by the MCP adapter because delivery would be
// ambiguous.
func (c *Client) Scroll(ctx context.Context, dx, dy int8) (err error) {
	if err := ValidateScroll(int(dx), int(dy)); err != nil {
		return fmt.Errorf("jetkvm: invalid scroll: %w", err)
	}
	if !c.allowControl || c.control == nil {
		return fmt.Errorf("jetkvm: scroll requires AllowControl/--allow-control: %w", ErrControlDisabled)
	}

	unlock, err := c.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()

	held, err := c.control.TryAcquirePersistent(ctx, DefaultControlLeaseTimeout)
	if err != nil {
		return fmt.Errorf("jetkvm: acquiring scroll control lease: %w", err)
	}
	defer func() { err = errors.Join(err, held.Release()) }()

	if c.sess == nil || c.sess.rpc == nil {
		return newDeviceError(ErrorKindUnreachable, "sending wheelReport RPC", fmt.Errorf("RPC session is unavailable"))
	}
	if err := c.sess.rpc.call(ctx, "wheelReport", wheelReportRPCParams(dx, dy), nil); err != nil {
		return fmt.Errorf("jetkvm: scroll failed: %w", err)
	}
	return nil
}

// ScreenshotResult describes one captured screenshot, always including
// enough metadata for a caller to judge whether to trust it.
//
// Path is set only by SaveScreenshot; CaptureScreenshot leaves it empty
// because it never touches the filesystem.
type ScreenshotResult struct {
	Path       string
	Width      int
	Height     int
	CapturedAt time.Time
	Fresh      bool
}

// Screenshot is one captured frame as PNG bytes plus its metadata. Keeping
// the image in memory is what lets the MCP adapter return an image to the
// caller without ever accepting - or writing to - a caller-chosen path.
type Screenshot struct {
	ScreenshotResult
	PNG []byte
}

// CaptureScreenshot records the current frame generation after the call has
// started, waits for a strictly newer decodable video frame, decodes it via
// the configured Decoder, and returns it as PNG bytes. Nothing is written to
// disk. A successful result is therefore always request-fresh: it can never
// be a cached frame from before this call (or a preceding control action).
//
// The frame wait and decode subprocess observe ctx directly. The synchronous
// image steps check it before and immediately after their bounded work, so an
// expired request cannot return a successful screenshot.
func (c *Client) CaptureScreenshot(ctx context.Context) (Screenshot, error) {
	if checker, ok := c.decoder.(interface{ CheckAvailable(context.Context) error }); ok {
		if err := checker.CheckAvailable(ctx); err != nil {
			return Screenshot{}, err
		}
	}
	unlock, err := c.lock(ctx)
	if err != nil {
		return Screenshot{}, err
	}
	defer unlock()

	requestBoundary := c.sess.video.generationBoundary()
	fr, err := c.sess.video.waitForFrameAfter(ctx, requestBoundary)
	if err != nil {
		// A frame that never arrives produces the same error whatever the
		// cause, so attach the localized boundary. Summary() is a bounded,
		// privacy-safe line: counts, states and codec parameters only.
		if ctx.Err() != nil {
			return Screenshot{}, timeoutError("waiting for a video frame", fmt.Errorf(
				"no video frame available: %w (%s)", err, c.VideoDiagnostics().Summary()))
		}
		return Screenshot{}, fmt.Errorf("jetkvm: no video frame available: %w (%s)", err, c.VideoDiagnostics().Summary())
	}

	c.sess.diag.decodeAttempted(len(fr.annexB))
	img, err := c.decoder.DecodeFrame(ctx, fr.annexB)
	if err != nil {
		c.sess.diag.decodeFailed(classifyDecodeError(err))
		return Screenshot{}, err
	}
	if img == nil {
		err := fmt.Errorf("jetkvm: video decoder returned a nil image")
		c.sess.diag.decodeFailed(classifyDecodeError(err))
		return Screenshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return Screenshot{}, fmt.Errorf("jetkvm: screenshot canceled after decode: %w", err)
	}

	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if err := ValidateScreenshotDimensions(width, height); err != nil {
		return Screenshot{}, fmt.Errorf("jetkvm: refusing to encode decoded screenshot: %w", err)
	}

	buf := newCappedBuffer(MaxScreenshotEncodedBytes)
	encodeErr := png.Encode(buf, img)
	if err := ctx.Err(); err != nil {
		return Screenshot{}, fmt.Errorf("jetkvm: screenshot canceled after PNG encode: %w", err)
	}
	if encodeErr != nil {
		return Screenshot{}, fmt.Errorf("jetkvm: encoding PNG: %w", encodeErr)
	}
	if buf.Overflowed() {
		return Screenshot{}, fmt.Errorf("jetkvm: encoded screenshot exceeds the %d-byte limit", MaxScreenshotEncodedBytes)
	}

	return Screenshot{
		ScreenshotResult: ScreenshotResult{
			Width:      width,
			Height:     height,
			CapturedAt: fr.capturedAt,
			Fresh:      true,
		},
		PNG: buf.Bytes(),
	}, nil
}

// SaveScreenshot captures one screenshot and writes it to outputPath as a
// PNG. It exists for the CLI, where the path comes from the local user's
// own command line. The MCP adapter deliberately does not use it - see
// CaptureScreenshot.
//
// The write is atomic: the PNG lands in a temporary file in the same
// directory and is renamed into place, so an interrupted capture cannot
// leave a truncated image at outputPath.
func (c *Client) SaveScreenshot(ctx context.Context, outputPath string) (ScreenshotResult, error) {
	shot, err := c.CaptureScreenshot(ctx)
	if err != nil {
		return ScreenshotResult{}, err
	}

	dir := filepath.Dir(outputPath)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return ScreenshotResult{}, fmt.Errorf("jetkvm: creating output directory: %w", err)
		}
	}

	tmp, err := os.CreateTemp(dir, ".jetkvm-screenshot-*.png")
	if err != nil {
		return ScreenshotResult{}, fmt.Errorf("jetkvm: creating output file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if _, err := tmp.Write(shot.PNG); err != nil {
		tmp.Close()
		return ScreenshotResult{}, fmt.Errorf("jetkvm: writing screenshot: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return ScreenshotResult{}, fmt.Errorf("jetkvm: closing screenshot: %w", err)
	}
	if err := os.Rename(tmpName, outputPath); err != nil {
		return ScreenshotResult{}, fmt.Errorf("jetkvm: saving screenshot: %w", err)
	}

	result := shot.ScreenshotResult
	result.Path = outputPath
	return result, nil
}

// VideoDiagnostics returns a privacy-safe snapshot of the video pipeline:
// transport and negotiation state, RTP and NAL-unit counts, keyframe
// request counts, and bounded failure categories. It contains no
// credentials, addresses, SDP, ICE candidates, paths, or payload bytes, and
// is safe to log or hand to an agent.
//
// It is most useful after a screenshot fails: FailureBoundary names the
// single stage the pipeline stopped at.
func (c *Client) VideoDiagnostics() VideoDiagnostics {
	if c.sess == nil {
		return VideoDiagnostics{FailureBoundary: BoundaryUndetermined}
	}
	return c.sess.diag.snapshot(c.sess.pc)
}

// Control returns the control lease used by keyboard, pointer, and scroll
// commands, or an error if this Client was connected with AllowControl: false.
// Scroll acquires it internally because current firmware requires a separate
// legacy RPC wire path for wheel frames.
func (c *Client) Control() (*controlLease, error) {
	if !c.allowControl || c.control == nil {
		return nil, fmt.Errorf("jetkvm: control was not enabled for this connection (AllowControl/--allow-control): %w", ErrControlDisabled)
	}
	return c.control, nil
}

// Close neutralizes any held control input and tears down the session and
// signaling connection, in that order - neutralization has to happen while
// the transport is still up.
//
// It is safe to call more than once and safe to call with an
// already-canceled context: neutralization always runs on a short, fresh
// context of its own. A non-nil error means neutralization could not be
// confirmed (ErrNeutralizeUnverified); the teardown still completes.
func (c *Client) Close(ctx context.Context) error {
	var neutralizeErr error
	if c.control != nil {
		releaseCtx, cancel := context.WithTimeout(context.Background(), neutralizeTimeout)
		neutralizeErr = c.control.neutralize(releaseCtx)
		cancel()
	}
	if c.sess != nil {
		c.sess.close()
	}
	if c.sig != nil {
		_ = c.sig.close()
	}
	return neutralizeErr
}

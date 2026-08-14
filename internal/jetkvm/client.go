// Package jetkvm is a browser-free client and session core for a JetKVM
// device: local (non-cloud) authentication, WebRTC signaling, video
// capture, JSON-RPC status queries and (opt-in, gated) HID control.
//
// It deliberately does not implement or depend on anything from the
// jetkvm/kvm cloud relay, OIDC/Google login, virtual media, ATX/power
// control, firmware update, or any other state-changing RPC method - only
// what's needed for read-only inspection and (when explicitly enabled)
// keyboard/mouse input.
package jetkvm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sync"
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

	// AllowControl gates whether this client negotiates the "hidrpc" data
	// channel at all. When false, the resulting Client is structurally
	// incapable of keyboard/mouse/ReleaseAll control - not merely
	// refusing at the call site - matching the CLI/MCP --allow-control
	// gate.
	AllowControl bool

	// Decoder overrides the video decode backend. Defaults to
	// &FFmpegDecoder{} when nil.
	Decoder Decoder

	// HTTPTimeout bounds individual HTTP requests. Defaults to 10s.
	HTTPTimeout time.Duration
}

// Client is the single session-owning core for one connected device. All
// commands (status, screenshot, control) are serialized through it; see
// owner.go for the control lease and release-all guarantees.
type Client struct {
	baseURL string
	http    *httpClient
	sig     *signaler
	sess    *session
	decoder Decoder
	encode  func(io.Writer, image.Image) error

	deviceID     string
	firmwareVer  string
	allowControl bool

	cmdMu chan struct{} // 1-buffered channel used as a non-reentrant command lock

	control *controlLease // nil unless AllowControl was set

	closeOnce sync.Once
	closeErr  error
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
		return nil, fmt.Errorf("jetkvm: device unreachable at %s: %w", baseURL, err)
	}

	switch {
	case !opts.Credentials.AuthToken.Empty():
		hc.setSessionCookie(opts.Credentials.AuthToken)
	case !opts.Credentials.Password.Empty():
		if err := hc.login(ctx, opts.Credentials.Password); err != nil {
			return nil, err
		}
	}

	dev, err := hc.device(ctx)
	if err != nil {
		return nil, fmt.Errorf("jetkvm: not authenticated (supply a password or auth token): %w", err)
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

	sess, err := establishSession(ctx, sig, dialOptions{allowControl: opts.AllowControl})
	if err != nil {
		return nil, errors.Join(err, closeSignalerAfterFailedConnect(sig))
	}

	decoder := opts.Decoder
	if decoder == nil {
		decoder = &FFmpegDecoder{}
	}
	deviceID := safeDeviceIdentifier(dev.DeviceID)
	firmwareVersion := meta.DeviceVersion
	for _, credential := range []Secret{opts.Credentials.Password, opts.Credentials.AuthToken} {
		if credential.ContainedIn(deviceID) {
			deviceID = redactionPlaceholder
		}
		if credential.ContainedIn(firmwareVersion) {
			firmwareVersion = redactionPlaceholder
		}
	}

	c := &Client{
		baseURL:      baseURL,
		http:         hc,
		sig:          sig,
		sess:         sess,
		decoder:      decoder,
		encode:       png.Encode,
		deviceID:     deviceID,
		firmwareVer:  firmwareVersion,
		allowControl: opts.AllowControl,
		cmdMu:        make(chan struct{}, 1),
	}
	if opts.AllowControl {
		c.control = newControlLease(sess.hid)
	}
	return c, nil
}

// lock acquires the command lock, serializing all Status/Screenshot/control
// calls through this Client, and returns an unlock func. It respects ctx
// cancellation so a canceled caller doesn't wait forever behind a stuck
// command.
func (c *Client) lock(ctx context.Context) (func(), error) {
	select {
	case c.cmdMu <- struct{}{}:
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
	// Fresh is retained for API/result compatibility. A successful capture
	// always sets it true because cached pre-request frames are now errors.
	Fresh bool
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
// Every step is bounded by ctx: the frame wait, the decode subprocess, and
// the PNG encode all abort when it is done.
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
		return Screenshot{}, fmt.Errorf("jetkvm: no video frame available: %w (%s)",
			err, c.VideoDiagnostics().Summary())
	}

	c.sess.diag.decodeAttempted(len(fr.annexB))
	img, err := c.decoder.DecodeFrame(ctx, fr.annexB)
	if err != nil {
		c.sess.diag.decodeFailed(classifyDecodeError(err))
		return Screenshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return Screenshot{}, fmt.Errorf("jetkvm: screenshot canceled after decode: %w", err)
	}

	var buf bytes.Buffer
	encode := c.encode
	if encode == nil {
		encode = png.Encode
	}
	if err := encode(&contextWriter{ctx: ctx, next: &buf}, img); err != nil {
		return Screenshot{}, fmt.Errorf("jetkvm: encoding PNG: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Screenshot{}, fmt.Errorf("jetkvm: screenshot canceled during encode: %w", err)
	}

	bounds := img.Bounds()
	return Screenshot{
		ScreenshotResult: ScreenshotResult{
			Width:      bounds.Dx(),
			Height:     bounds.Dy(),
			CapturedAt: fr.capturedAt,
			Fresh:      true,
		},
		PNG: buf.Bytes(),
	}, nil
}

type contextWriter struct {
	ctx  context.Context
	next io.Writer
}

func (w *contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.next.Write(p)
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

// Control returns the control lease for keyboard/mouse commands, or an
// error if this Client was connected with AllowControl: false.
func (c *Client) Control() (*controlLease, error) {
	if c.control == nil {
		return nil, fmt.Errorf("jetkvm: control was not enabled for this connection (AllowControl/--allow-control)")
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
	c.closeOnce.Do(func() {
		if c.control != nil {
			// A live caller-supplied cleanup deadline is honored, capped by the
			// HID layer's own safety bound. If the operation context has already
			// expired, use a fresh bounded context: cancellation must not skip the
			// release-all attempt that makes control shutdown safe.
			parent := ctx
			if parent == nil || parent.Err() != nil {
				parent = context.Background()
			}
			releaseCtx, cancel := context.WithTimeout(parent, neutralizeTimeout)
			c.closeErr = c.control.neutralize(releaseCtx)
			cancel()
		}
		if c.sess != nil {
			c.closeErr = errors.Join(c.closeErr, c.sess.close(ctx))
		}
		if c.sig != nil {
			c.closeErr = errors.Join(c.closeErr, c.sig.close(ctx))
		}
	})
	return c.closeErr
}

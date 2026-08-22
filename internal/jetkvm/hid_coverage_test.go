package jetkvm

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/hidproto"
)

func TestHeldHIDReportWireBytesAndRejections(t *testing.T) {
	tests := []struct {
		name      string
		send      func(context.Context, *Held) error
		wantFrame []byte
		wantErr   bool
	}{
		{
			name: "keyboard maximum key buffer",
			send: func(ctx context.Context, held *Held) error {
				return held.SendKeyboardReport(ctx, 0x02, []byte{0x04, 0x05, 0x06, 0x07, 0x08, 0x09})
			},
			wantFrame: []byte{0x02, 0x02, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09},
		},
		{
			name: "keyboard key buffer one over maximum",
			send: func(ctx context.Context, held *Held) error {
				return held.SendKeyboardReport(ctx, 0, []byte{0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a})
			},
			wantErr: true,
		},
		{
			name: "absolute pointer inclusive boundaries",
			send: func(ctx context.Context, held *Held) error {
				return held.SendPointerReport(ctx, 0, hidproto.MaxAbsoluteCoordinate, hidproto.MaxAbsoluteButtonMask)
			},
			wantFrame: []byte{0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x7f, 0xff, 0x1f},
		},
		{
			name: "absolute pointer negative coordinate",
			send: func(ctx context.Context, held *Held) error {
				return held.SendPointerReport(ctx, -1, 0, 0)
			},
			wantErr: true,
		},
		{
			name: "absolute pointer coordinate one over maximum",
			send: func(ctx context.Context, held *Held) error {
				return held.SendPointerReport(ctx, 0, hidproto.MaxAbsoluteCoordinate+1, 0)
			},
			wantErr: true,
		},
		{
			name: "absolute pointer reserved button bit",
			send: func(ctx context.Context, held *Held) error {
				return held.SendPointerReport(ctx, 0, 0, hidproto.MaxAbsoluteButtonMask+1)
			},
			wantErr: true,
		},
		{
			name: "relative mouse inclusive boundaries",
			send: func(ctx context.Context, held *Held) error {
				return held.SendMouseReport(ctx, -hidproto.MaxRelativeMouseDelta, hidproto.MaxRelativeMouseDelta, 0x02)
			},
			wantFrame: []byte{0x06, 0x81, 0x7f, 0x02},
		},
		{
			name: "relative mouse dx below minimum",
			send: func(ctx context.Context, held *Held) error {
				return held.SendMouseReport(ctx, -128, 0, 0)
			},
			wantErr: true,
		},
		{
			name: "relative mouse dy below minimum",
			send: func(ctx context.Context, held *Held) error {
				return held.SendMouseReport(ctx, 0, -128, 0)
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc, transport := newFakeHIDClient(t)
			lease := newControlLease(hc)
			ctx := contextWithTimeout(t, 2*time.Second)
			held, err := lease.Acquire(ctx, 2*time.Second)
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			t.Cleanup(func() {
				if err := held.Release(); err != nil {
					t.Errorf("Release: %v", err)
				}
			})

			beforeFrames := transport.count()
			beforeHeld := hc.hasHeldState()
			err = tt.send(ctx, held)
			if tt.wantErr {
				if err == nil {
					t.Fatal("report unexpectedly succeeded")
				}
				if got := transport.count(); got != beforeFrames {
					t.Fatalf("rejected report reached the transport: frame count %d -> %d", beforeFrames, got)
				}
				if got := hc.hasHeldState(); got != beforeHeld {
					t.Fatalf("rejected report changed held state from %v to %v", beforeHeld, got)
				}
				return
			}

			if err != nil {
				t.Fatalf("report failed: %v", err)
			}
			frames := transport.snapshot()
			if len(frames) != beforeFrames+1 {
				t.Fatalf("successful report changed frame count %d -> %d, want one new frame", beforeFrames, len(frames))
			}
			if got := frames[len(frames)-1]; !bytes.Equal(got, tt.wantFrame) {
				t.Fatalf("wire frame = % x, want % x", got, tt.wantFrame)
			}
		})
	}
}

func TestHIDCheckWritableLockedMatrix(t *testing.T) {
	tests := []struct {
		name      string
		state     hidState
		activeGen uint64
		request   hidRequest
		wantErr   error
	}{
		{name: "closed rejects ordinary input", state: hidStateClosed, request: hidRequest{token: 7}, wantErr: ErrHIDClosed},
		{name: "negotiating rejects ordinary input", state: hidStateNegotiating, request: hidRequest{token: 7}, wantErr: ErrHIDNotReady},
		{name: "negotiating rejects privileged input", state: hidStateNegotiating, request: hidRequest{privileged: true}, wantErr: ErrHIDNotReady},
		{name: "handshaking rejects ordinary input", state: hidStateHandshaking, request: hidRequest{token: 7}, wantErr: ErrHIDNotReady},
		{name: "handshaking accepts privileged input", state: hidStateHandshaking, request: hidRequest{privileged: true}},
		{name: "ready rejects zero token", state: hidStateReady, activeGen: 7, request: hidRequest{}, wantErr: ErrStaleControlToken},
		{name: "ready rejects wrong token", state: hidStateReady, activeGen: 7, request: hidRequest{token: 8}, wantErr: ErrStaleControlToken},
		{name: "ready accepts current token", state: hidStateReady, activeGen: 7, request: hidRequest{token: 7}},
		{name: "ready accepts privileged input", state: hidStateReady, activeGen: 7, request: hidRequest{privileged: true}},
		{name: "unknown state rejects input", state: hidState(99), activeGen: 7, request: hidRequest{token: 7}, wantErr: ErrHIDNotReady},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc := &hidClient{state: tt.state, activeGen: tt.activeGen}
			hc.stateMu.Lock()
			err := hc.checkWritableLocked(tt.request)
			hc.stateMu.Unlock()

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("checkWritableLocked() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestHIDHandshakeStateAndSendFailures(t *testing.T) {
	tests := []struct {
		name        string
		arrange     func(*hidClient, *fakeHIDTransport)
		wantErr     error
		wantContain string
		wantKind    ErrorKind
		wantState   hidState
	}{
		{
			name: "closed channel",
			arrange: func(hc *hidClient, _ *fakeHIDTransport) {
				hc.closeWith(errors.New("synthetic disconnect"))
			},
			wantErr:   ErrHIDClosed,
			wantState: hidStateClosed,
		},
		{
			name: "handshake already in progress",
			arrange: func(hc *hidClient, _ *fakeHIDTransport) {
				hc.stateMu.Lock()
				hc.state = hidStateHandshaking
				hc.stateMu.Unlock()
			},
			wantContain: "already in progress",
			wantState:   hidStateHandshaking,
		},
		{
			name: "transport send failure",
			arrange: func(_ *hidClient, transport *fakeHIDTransport) {
				transport.setFailure(1, errors.New("synthetic send failure"))
			},
			wantKind:  ErrorKindUnreachable,
			wantState: hidStateClosed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc, transport := newUnreadyHIDClient(t)
			tt.arrange(hc, transport)

			err := hc.handshake(contextWithTimeout(t, time.Second))
			if err == nil {
				t.Fatal("handshake unexpectedly succeeded")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("handshake error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantContain != "" && !strings.Contains(err.Error(), tt.wantContain) {
				t.Fatalf("handshake error = %q, want substring %q", err, tt.wantContain)
			}
			if tt.wantKind != "" && ErrorKindOf(err) != tt.wantKind {
				t.Fatalf("handshake error kind = %q, want %q: %v", ErrorKindOf(err), tt.wantKind, err)
			}
			if got := hc.currentState(); got != tt.wantState {
				t.Fatalf("state after handshake = %s, want %s", got, tt.wantState)
			}
			if got := transport.count(); got != 0 {
				t.Fatalf("failed handshake recorded %d wire frames, want 0", got)
			}
		})
	}
}

func TestDisconnectWhileHeldReportsUnverifiedNeutralization(t *testing.T) {
	tests := []struct {
		name string
		send func(context.Context, *Held) error
	}{
		{
			name: "keyboard",
			send: func(ctx context.Context, held *Held) error {
				return held.SendKeyboardReport(ctx, 0x02, []byte{0x04})
			},
		},
		{
			name: "absolute pointer",
			send: func(ctx context.Context, held *Held) error {
				return held.SendPointerReport(ctx, 123, 456, 0x01)
			},
		},
		{
			name: "relative mouse",
			send: func(ctx context.Context, held *Held) error {
				return held.SendMouseReport(ctx, 0, 0, 0x02)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc, transport := newFakeHIDClient(t)
			lease := newControlLease(hc)
			ctx := contextWithTimeout(t, 2*time.Second)
			held, err := lease.Acquire(ctx, 2*time.Second)
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			t.Cleanup(func() { _ = held.Release() })

			if err := tt.send(ctx, held); err != nil {
				t.Fatalf("send held report: %v", err)
			}
			if !hc.hasHeldState() {
				t.Fatal("held report did not update conservative held state")
			}
			beforeDisconnect := transport.count()

			hc.closeWith(errors.New("synthetic disconnect"))
			err = held.Release()
			if !errors.Is(err, ErrNeutralizeUnverified) {
				t.Fatalf("Release after disconnect = %v, want ErrNeutralizeUnverified", err)
			}
			if got := transport.count(); got != beforeDisconnect {
				t.Fatalf("release wrote to a closed transport: frame count %d -> %d", beforeDisconnect, got)
			}
			if hc.activeGeneration() != 0 {
				t.Fatal("disconnect did not revoke the active generation")
			}
			if !hc.hasHeldState() {
				t.Fatal("unverified neutralization cleared conservative held state")
			}
		})
	}
}

func TestBuildNeutralizationPlanWireBytesAndErrors(t *testing.T) {
	keyboardClear := []byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	mouseClear := []byte{0x06, 0x00, 0x00, 0x00}
	tests := []struct {
		name         string
		held         heldInput
		wantFrames   [][]byte
		wantAbsolute bool
		wantErr      bool
	}{
		{
			name:       "canonical keyboard and relative clears",
			wantFrames: [][]byte{keyboardClear, mouseClear},
		},
		{
			name: "absolute clear at inclusive maximum coordinates",
			held: heldInput{
				absoluteButtons:       true,
				absolutePositionKnown: true,
				absoluteX:             hidproto.MaxAbsoluteCoordinate,
				absoluteY:             hidproto.MaxAbsoluteCoordinate,
			},
			wantFrames: [][]byte{
				keyboardClear,
				mouseClear,
				{0x03, 0x00, 0x00, 0x7f, 0xff, 0x00, 0x00, 0x7f, 0xff, 0x00},
			},
			wantAbsolute: true,
		},
		{
			name:    "absolute buttons without coordinates",
			held:    heldInput{absoluteButtons: true},
			wantErr: true,
		},
		{
			name: "corrupt negative absolute coordinate",
			held: heldInput{
				absoluteButtons:       true,
				absolutePositionKnown: true,
				absoluteX:             -1,
			},
			wantErr: true,
		},
		{
			name: "corrupt absolute coordinate above maximum",
			held: heldInput{
				absoluteButtons:       true,
				absolutePositionKnown: true,
				absoluteY:             hidproto.MaxAbsoluteCoordinate + 1,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := buildNeutralizationPlan(tt.held)
			if tt.wantErr {
				if err == nil {
					t.Fatal("buildNeutralizationPlan unexpectedly succeeded")
				}
				if len(plan.frames) != 0 || plan.keyboard || plan.relativeButtons || plan.absoluteButtons {
					t.Fatalf("failed plan returned partial neutralization state: %+v", plan)
				}
				return
			}

			if err != nil {
				t.Fatalf("buildNeutralizationPlan: %v", err)
			}
			if !plan.keyboard || !plan.relativeButtons || plan.absoluteButtons != tt.wantAbsolute {
				t.Fatalf("plan interface flags = keyboard:%v relative:%v absolute:%v, want true/true/%v",
					plan.keyboard, plan.relativeButtons, plan.absoluteButtons, tt.wantAbsolute)
			}
			if len(plan.frames) != len(tt.wantFrames) {
				t.Fatalf("plan has %d frames, want %d", len(plan.frames), len(tt.wantFrames))
			}
			for i := range tt.wantFrames {
				if !bytes.Equal(plan.frames[i], tt.wantFrames[i]) {
					t.Errorf("frame %d = % x, want % x", i, plan.frames[i], tt.wantFrames[i])
				}
			}
		})
	}
}

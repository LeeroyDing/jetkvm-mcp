package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
	sharedfake "github.com/leeroyding/jetkvm-mcp/internal/testdata/fakedevice"
)

var (
	cliKeyboardNeutral = []byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	cliMouseNeutral    = []byte{0x06, 0x00, 0x00, 0x00}
)

type cliControlExecutionCase struct {
	name        string
	args        []string
	wantJSON    map[string]any
	requiredHID [][]byte
	wantRPC     map[string]any
}

func cliKeyboardFrame(modifier byte, keys ...byte) []byte {
	return append([]byte{0x02, modifier}, keys...)
}

func cliPointerFrame(x, y int32, buttons byte) []byte {
	return []byte{
		0x03,
		byte(uint32(x) >> 24), byte(uint32(x) >> 16), byte(uint32(x) >> 8), byte(x),
		byte(uint32(y) >> 24), byte(uint32(y) >> 16), byte(uint32(y) >> 8), byte(y),
		buttons,
	}
}

func cliMouseFrame(dx, dy int8, buttons byte) []byte {
	return []byte{0x06, byte(dx), byte(dy), buttons}
}

func cliLeaseUnit(frames ...[]byte) [][]byte {
	result := append([][]byte(nil), frames...)
	return append(result, cliKeyboardNeutral, cliMouseNeutral)
}

func cliAbsoluteLeaseUnit(x, y int32, frames ...[]byte) [][]byte {
	result := cliLeaseUnit(frames...)
	return append(result, cliPointerFrame(x, y, 0))
}

func cliControlArgs(baseURL string, args []string) []string {
	result := []string{args[0], "--url", baseURL, "--timeout", "15s", "--allow-control"}
	return append(result, args[1:]...)
}

func clearCLIAuthEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"JETKVM_AUTH_TOKEN",
		"JETKVM_PASSWORD",
		"JETKVM_PASSWORD_KEYCHAIN_SERVICE",
		"JETKVM_PASSWORD_KEYCHAIN_ACCOUNT",
	} {
		t.Setenv(name, "")
	}
}

func normalizeJSONValue(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal expected JSON: %v", err)
	}
	var normalized any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		t.Fatalf("normalize expected JSON: %v", err)
	}
	return normalized
}

func assertCLIJSON(t *testing.T, output string, want map[string]any) {
	t.Helper()
	var got any
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("CLI output is not JSON: %v\n%s", err, output)
	}
	wantNormalized := normalizeJSONValue(t, want)
	if !reflect.DeepEqual(got, wantNormalized) {
		t.Fatalf("CLI JSON = %#v, want %#v", got, wantNormalized)
	}
}

func assertCLILeaseWire(t *testing.T, fd *sharedfake.Device, required [][]byte) {
	t.Helper()
	for i, want := range required {
		got, isString := fd.NextHIDWireFrame(t)
		if isString {
			t.Fatalf("HID frame %d used a text WebRTC message, want binary", i+1)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("HID frame %d = % x, want exact bytes % x", i+1, got, want)
		}
	}

	// Client.Close may conservatively repeat the canonical keyboard/relative
	// neutral pair after the command's lease has already released. Requiring
	// that any tail consists only of complete neutral pairs preserves the
	// safety contract without pinning a harmless redundant cleanup count.
	tail := fd.DrainHIDWireFrames(t, 40*time.Millisecond)
	if len(tail)%2 != 0 {
		t.Fatalf("trailing HID frames = %d, want complete keyboard/mouse neutral pairs", len(tail))
	}
	for i := 0; i < len(tail); i += 2 {
		if tail[i].IsString || tail[i+1].IsString {
			t.Fatalf("trailing HID neutral pair %d used a text WebRTC message, want binary", i/2+1)
		}
		if !bytes.Equal(tail[i].Data, cliKeyboardNeutral) || !bytes.Equal(tail[i+1].Data, cliMouseNeutral) {
			t.Fatalf("trailing HID frames %d/%d = % x / % x, want canonical neutral pair % x / % x",
				len(required)+i+1, len(required)+i+2,
				tail[i].Data, tail[i+1].Data, cliKeyboardNeutral, cliMouseNeutral)
		}
	}
}

func assertCLIRPCWire(t *testing.T, fd *sharedfake.Device, want map[string]any) {
	t.Helper()
	if want == nil {
		if extra := fd.DrainRPCWireFrames(t, 40*time.Millisecond); len(extra) != 0 {
			t.Fatalf("unexpected RPC frames: %q", extra[0].Data)
		}
		return
	}
	data, isString := fd.NextRPCWireFrame(t)
	if !isString {
		t.Fatal("RPC request used a binary WebRTC message, want text")
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("RPC frame is not JSON: %q: %v", data, err)
	}
	request, ok := decoded.(map[string]any)
	if !ok {
		t.Fatalf("RPC frame root = %T, want object", decoded)
	}
	id, ok := request["id"].(float64)
	if !ok || id <= 0 || math.Trunc(id) != id {
		t.Fatalf("RPC request id = %#v, want a positive integer", request["id"])
	}
	delete(request, "id")
	wantNormalized := normalizeJSONValue(t, want)
	if !reflect.DeepEqual(request, wantNormalized) {
		t.Fatalf("RPC frame excluding allocated id = %#v, want %#v", request, wantNormalized)
	}
	if extra := fd.DrainRPCWireFrames(t, 40*time.Millisecond); len(extra) != 0 {
		t.Fatalf("unexpected trailing RPC frame: %q", extra[0].Data)
	}
}

func TestCLIControlExecutionUsesExactWireValuesAndTerminalNeutralization(t *testing.T) {
	clearCLIAuthEnvironment(t)

	typeFrames := append(
		cliLeaseUnit(cliKeyboardFrame(0, 0x04)),
		cliLeaseUnit(cliKeyboardFrame(jetkvm.ModifierLeftShift, 0x04))...,
	)
	sequenceFrames := append(
		cliLeaseUnit(cliKeyboardFrame(jetkvm.ModifierLeftControl, jetkvm.KeyUsageC)),
		cliLeaseUnit(cliKeyboardFrame(jetkvm.ModifierLeftAlt, jetkvm.KeyUsageTab))...,
	)
	dragFrames := [][]byte{
		cliPointerFrame(0, 0, jetkvm.MouseButtonLeft),
		cliPointerFrame(3, 2, jetkvm.MouseButtonLeft),
		cliPointerFrame(6, 4, jetkvm.MouseButtonLeft),
		cliPointerFrame(9, 6, jetkvm.MouseButtonLeft),
		cliPointerFrame(9, 6, 0),
	}
	dragFrames = cliAbsoluteLeaseUnit(9, 6, dragFrames...)

	cases := []cliControlExecutionCase{
		{
			name:     "keypress maximum wire bytes",
			args:     []string{"keypress", "--key", "255", "--modifier", "255"},
			wantJSON: map[string]any{"sent": "keypress", "key": 255, "modifier": 255},
			requiredHID: cliLeaseUnit(
				cliKeyboardFrame(0xff, 0xff),
			),
		},
		{
			name:        "type preserves per-character lease boundaries",
			args:        []string{"type", "--text", "aA", "--delay-ms", "0"},
			wantJSON:    map[string]any{"sent": "type", "runes": 2, "delayMs": 0},
			requiredHID: typeFrames,
		},
		{
			name:     "key combo",
			args:     []string{"key-combo", "--combo", "ctrl+alt+del"},
			wantJSON: map[string]any{"sent": "key-combo", "combo": "ctrl+alt+del", "modifier": 5, "keys": []int{jetkvm.KeyUsageDelete}},
			requiredHID: cliLeaseUnit(
				cliKeyboardFrame(jetkvm.ModifierLeftControl|jetkvm.ModifierLeftAlt, jetkvm.KeyUsageDelete),
			),
		},
		{
			name:     "hold key releases after hold",
			args:     []string{"hold-key", "--combo", "ctrl+c", "--hold-ms", "1"},
			wantJSON: map[string]any{"sent": "hold-key", "combo": "ctrl+c", "holdMs": 1, "modifier": 1, "keys": []int{jetkvm.KeyUsageC}},
			requiredHID: cliLeaseUnit(
				cliKeyboardFrame(jetkvm.ModifierLeftControl, jetkvm.KeyUsageC),
			),
		},
		{
			name:        "key sequence preserves chord order and lease boundaries",
			args:        []string{"key-sequence", "--combo", "ctrl+c", "--combo", "alt+tab", "--delay-ms", "0"},
			wantJSON:    map[string]any{"sent": "key-sequence", "combos": 2, "delayMs": 0},
			requiredHID: sequenceFrames,
		},
		{
			name:     "mouse button uses zero relative deltas",
			args:     []string{"mouse-button", "--button", "right", "--action", "press"},
			wantJSON: map[string]any{"sent": "mouse-button", "button": "right", "action": "press"},
			requiredHID: cliLeaseUnit(
				cliMouseFrame(0, 0, jetkvm.MouseButtonRight),
			),
		},
		{
			name:     "mouse move maximum coordinate and mask",
			args:     []string{"mouse-move", "--x", "32767", "--y", "0", "--buttons", "31"},
			wantJSON: map[string]any{"sent": "mouse-move", "x": 32767, "y": 0, "buttons": 31},
			requiredHID: cliAbsoluteLeaseUnit(
				32767, 0, cliPointerFrame(32767, 0, 31),
			),
		},
		{
			name:        "scroll signed wire boundaries use legacy RPC",
			args:        []string{"scroll", "--dx", "-127", "--dy", "127"},
			wantJSON:    map[string]any{"sent": "scroll", "dx": -127, "dy": 127},
			requiredHID: cliLeaseUnit(),
			wantRPC: map[string]any{
				"jsonrpc": "2.0",
				"method":  "wheelReport",
				"params":  map[string]any{"wheelX": -127, "wheelY": 127},
			},
		},
		{
			name:     "click releases at the same coordinates",
			args:     []string{"click", "--x", "321", "--y", "654", "--button", "2"},
			wantJSON: map[string]any{"sent": "click", "x": 321, "y": 654, "button": 2},
			requiredHID: cliAbsoluteLeaseUnit(
				321, 654,
				cliPointerFrame(321, 654, 2),
				cliPointerFrame(321, 654, 0),
			),
		},
		{
			name:     "double click sends two complete clicks",
			args:     []string{"double-click", "--x", "111", "--y", "222", "--button", "1"},
			wantJSON: map[string]any{"sent": "double-click", "x": 111, "y": 222, "button": 1},
			requiredHID: cliAbsoluteLeaseUnit(
				111, 222,
				cliPointerFrame(111, 222, 1),
				cliPointerFrame(111, 222, 0),
				cliPointerFrame(111, 222, 1),
				cliPointerFrame(111, 222, 0),
			),
		},
		{
			name:        "drag sends interpolated reports and releases at destination",
			args:        []string{"drag", "--x1", "0", "--y1", "0", "--x2", "9", "--y2", "6", "--button", "1", "--steps", "2"},
			wantJSON:    map[string]any{"sent": "drag", "x1": 0, "y1": 0, "x2": 9, "y2": 6, "button": 1, "steps": 2},
			requiredHID: dragFrames,
		},
		{
			name:        "release all reports transport acknowledgement only after neutralization",
			args:        []string{"release-all"},
			wantJSON:    map[string]any{"sent": "release-all", "peerTransportAcknowledged": true},
			requiredHID: cliLeaseUnit(),
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fd := sharedfake.Start(t, sharedfake.Options{CaptureWire: true})
			exitCode := -1
			output, err := captureStdout(t, func() error {
				var runErr error
				exitCode, runErr = runCLI(cliControlArgs(fd.BaseURL(), test.args))
				return runErr
			})
			if err != nil || exitCode != 0 {
				t.Fatalf("runCLI(%s) = exit %d, error %v", test.args[0], exitCode, err)
			}
			assertCLIJSON(t, output, test.wantJSON)

			assertCLILeaseWire(t, fd, test.requiredHID)
			assertCLIRPCWire(t, fd, test.wantRPC)
		})
	}
}

func TestCLIControlRejectsGateAndOutOfRangeValuesBeforeDeviceActivity(t *testing.T) {
	clearCLIAuthEnvironment(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "allow-control gate", args: []string{"keypress", "--key", "255", "--modifier", "255"}, want: "--allow-control"},
		{name: "keypress above byte", args: []string{"keypress", "--allow-control", "--key", "256"}, want: "[0,255]"},
		{name: "modifier below byte", args: []string{"keypress", "--allow-control", "--key", "0", "--modifier", "-1"}, want: "[0,255]"},
		{name: "pointer above wire coordinate", args: []string{"mouse-move", "--allow-control", "--x", "32768", "--y", "0"}, want: "[0,32767]"},
		{name: "gesture zero button", args: []string{"click", "--allow-control", "--x", "0", "--y", "32767", "--button", "0"}, want: "[1,31]"},
		{name: "scroll negative 128 is outside HID contract", args: []string{"scroll", "--allow-control", "--dx", "-128", "--dy", "127"}, want: "[-127,127]"},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fd := sharedfake.Start(t, sharedfake.Options{CaptureWire: true})
			args := append([]string{test.args[0], "--url", fd.BaseURL()}, test.args[1:]...)
			exitCode := -1
			output, err := captureStdout(t, func() error {
				var runErr error
				exitCode, runErr = runCLI(args)
				return runErr
			})
			if err == nil || exitCode != 1 {
				t.Fatalf("runCLI(%s) = exit %d, error %v; want rejected command", test.args[0], exitCode, err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("rejection error = %v, want %q", err, test.want)
			}
			if output != "" {
				t.Fatalf("rejected command printed success output: %q", output)
			}
			status, login, signaling, rpc := fd.Counts()
			_, hid := fd.WireCounts()
			if status != 0 || login != 0 || signaling != 0 || rpc != 0 || hid != 0 {
				t.Fatalf("rejected command touched device: status=%d login=%d signaling=%d rpc=%d hid=%d",
					status, login, signaling, rpc, hid)
			}
		})
	}
}

func TestCLIControlAuthenticationFailureSendsNoInputOrSuccess(t *testing.T) {
	clearCLIAuthEnvironment(t)
	const wrongPassword = "wrong-password-canary"
	t.Setenv("JETKVM_PASSWORD", wrongPassword)
	fd := sharedfake.Start(t, sharedfake.Options{Password: "expected-password", CaptureWire: true})

	exitCode := -1
	output, err := captureStdout(t, func() error {
		var runErr error
		exitCode, runErr = runCLI(cliControlArgs(fd.BaseURL(), []string{"keypress", "--key", "255"}))
		return runErr
	})
	if err == nil || exitCode != 1 {
		t.Fatalf("authenticated keypress = exit %d, error %v; want failure", exitCode, err)
	}
	if kind := jetkvm.ErrorKindOf(err); kind != jetkvm.ErrorKindAuthFailed {
		t.Fatalf("authentication error kind = %q, want %q: %v", kind, jetkvm.ErrorKindAuthFailed, err)
	}
	if strings.Contains(err.Error(), wrongPassword) || strings.Contains(formatCLIError(err), wrongPassword) {
		t.Fatalf("authentication error reflected password canary: %v", err)
	}
	if output != "" {
		t.Fatalf("failed keypress printed success output: %q", output)
	}
	if rpc, hid := fd.WireCounts(); rpc != 0 || hid != 0 {
		t.Fatalf("authentication failure sent device input: rpc=%d hid=%d", rpc, hid)
	}
}

func TestCLILeaseDenialPreservesWireBoundariesAndPrintsNoSuccess(t *testing.T) {
	clearCLIAuthEnvironment(t)

	t.Run("scroll", func(t *testing.T) {
		calls := 0
		output, err := captureStdout(t, func() error {
			return runScrollWithSender(
				[]string{"--url", "http://device.invalid", "--allow-control", "--dx", "-127", "--dy", "127"},
				func(ctx context.Context, cf *commonFlags, dx, dy int8) error {
					calls++
					if dx != -127 || dy != 127 || !cf.allowControl {
						t.Fatalf("lease-denied scroll payload = dx=%d dy=%d flags=%+v", dx, dy, cf)
					}
					if _, ok := ctx.Deadline(); !ok {
						t.Fatal("lease-denied scroll context has no deadline")
					}
					return jetkvm.ErrControlHeld
				},
			)
		})
		if !errors.Is(err, jetkvm.ErrControlHeld) || calls != 1 {
			t.Fatalf("lease-denied scroll = calls %d, error %v", calls, err)
		}
		if output != "" {
			t.Fatalf("lease-denied scroll printed success output: %q", output)
		}
	})

	t.Run("double click", func(t *testing.T) {
		calls := 0
		output, err := captureStdout(t, func() error {
			return runDoubleClickWithSender(
				[]string{"--url", "http://device.invalid", "--allow-control", "--x", "32767", "--y", "32767", "--button", "31"},
				func(_ context.Context, _ *commonFlags, x, y int32, button byte) error {
					calls++
					if x != 32767 || y != 32767 || button != 31 {
						t.Fatalf("lease-denied double-click payload = x=%d y=%d button=%d", x, y, button)
					}
					return jetkvm.ErrControlHeld
				},
			)
		})
		if !errors.Is(err, jetkvm.ErrControlHeld) || calls != 1 {
			t.Fatalf("lease-denied double click = calls %d, error %v", calls, err)
		}
		if output != "" {
			t.Fatalf("lease-denied double click printed success output: %q", output)
		}
	})

	t.Run("drag", func(t *testing.T) {
		calls := 0
		output, err := captureStdout(t, func() error {
			return runDragWithSender(
				[]string{"--url", "http://device.invalid", "--allow-control", "--x1", "0", "--y1", "0", "--x2", "32767", "--y2", "32767", "--button", "31", "--steps", "0"},
				func(_ context.Context, _ *commonFlags, reports []jetkvm.PointerDragReport) error {
					calls++
					want := []jetkvm.PointerDragReport{
						{X: 0, Y: 0, Buttons: 31},
						{X: 32767, Y: 32767, Buttons: 31},
						{X: 32767, Y: 32767, Buttons: 0},
					}
					if !reflect.DeepEqual(reports, want) {
						t.Fatalf("lease-denied drag reports = %+v, want %+v", reports, want)
					}
					return jetkvm.ErrControlHeld
				},
			)
		})
		if !errors.Is(err, jetkvm.ErrControlHeld) || calls != 1 {
			t.Fatalf("lease-denied drag = calls %d, error %v", calls, err)
		}
		if output != "" {
			t.Fatalf("lease-denied drag printed success output: %q", output)
		}
	})
}

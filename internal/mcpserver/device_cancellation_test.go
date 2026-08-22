package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/hidproto"
	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

func TestClientDeviceMouseButtonLateMirrorCancellationNeutralizesBeforeCommit(t *testing.T) {
	fd := startFakeDevice(t)
	connectCtx, cancelConnect := context.WithTimeout(
		context.Background(),
		connectTimeout(t, 15*time.Second),
	)
	defer cancelConnect()
	client, err := jetkvm.Connect(connectCtx, jetkvm.Options{
		BaseURL:      fd.baseURL(),
		AllowControl: true,
	})
	if err != nil {
		t.Fatalf("jetkvm.Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	device := &clientDevice{client: client}

	if err := device.mouseButton(connectCtx, jetkvm.MouseButtonLeft, true); err != nil {
		t.Fatalf("press retained left button: %v", err)
	}
	if err := device.mouseMove(connectCtx, 222, 333, 0); err != nil {
		t.Fatalf("mirror retained left button onto absolute interface: %v", err)
	}
	relativeLeft, _ := hidproto.EncodeMouseReport(0, 0, jetkvm.MouseButtonLeft)
	absoluteLeft, _ := hidproto.EncodePointerReport(222, 333, jetkvm.MouseButtonLeft)
	for i, expected := range [][]byte{relativeLeft, absoluteLeft} {
		if got := fd.nextHIDFrame(t); !bytes.Equal(got, expected) {
			t.Fatalf("setup frame %d = % x, want % x", i, got, expected)
		}
	}

	callCtx, cancelCall := context.WithCancel(context.Background())
	defer cancelCall()
	device.retainedPointerSend = func(
		_ context.Context,
		held *jetkvm.Held,
		x, y int32,
		buttons byte,
	) error {
		// Model a lower layer that completes successfully just as the caller
		// abandons the operation. The adapter must observe the original context
		// after this late nil result and end the retained generation.
		if err := held.SendPointerReport(context.Background(), x, y, buttons); err != nil {
			return err
		}
		cancelCall()
		return nil
	}
	err = device.mouseButton(callCtx, jetkvm.MouseButtonRight, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("late mirror cancellation = %v, want context.Canceled", err)
	}

	relativeBoth, _ := hidproto.EncodeMouseReport(
		0,
		0,
		jetkvm.MouseButtonLeft|jetkvm.MouseButtonRight,
	)
	absoluteBoth, _ := hidproto.EncodePointerReport(
		222,
		333,
		jetkvm.MouseButtonLeft|jetkvm.MouseButtonRight,
	)
	releaseKeyboard, _ := hidproto.ReleaseAllKeyboardReport()
	releaseMouse, _ := hidproto.ReleaseAllMouseReport()
	releaseAbsolute, _ := hidproto.EncodePointerReport(222, 333, 0)
	for i, expected := range [][]byte{
		relativeBoth,
		absoluteBoth,
		releaseKeyboard,
		releaseMouse,
		releaseAbsolute,
	} {
		if got := fd.nextHIDFrame(t); !bytes.Equal(got, expected) {
			t.Fatalf("late-cancel frame %d = % x, want % x", i, got, expected)
		}
	}

	absolute, relative := fd.mouseInterfaceState()
	if absolute.X != 222 || absolute.Y != 333 || absolute.Buttons != 0 || relative.Buttons != 0 {
		t.Fatalf("mouse state after late cancellation = hidg1 %+v hidg2 %+v, want neutral", absolute, relative)
	}
	assertClientDeviceButtonsCleared(t, device)
}

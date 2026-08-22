package mcpserver

import (
	"testing"

	sharedfake "github.com/leeroyding/jetkvm-mcp/internal/testdata/fakedevice"
)

// Keep the package-local fixture vocabulary stable for the existing MCP
// tests while sharing the implementation with adapter-level test packages.
type fakeRPCResponseMode = sharedfake.RPCResponseMode

const (
	fakeRPCNormal    = sharedfake.RPCNormal
	fakeRPCTruncated = sharedfake.RPCTruncated
	fakeRPCOversized = sharedfake.RPCOversized
)

type fakeDeviceOptions = sharedfake.Options
type fakeAbsoluteMouseState = sharedfake.AbsoluteMouseState
type fakeRelativeMouseState = sharedfake.RelativeMouseState

type fakeDevice struct {
	*sharedfake.Device
}

func startFakeDevice(t *testing.T) *fakeDevice {
	t.Helper()
	return startFakeDeviceWithOptions(t, fakeDeviceOptions{})
}

func startFakeDeviceWithOptions(t *testing.T, opts fakeDeviceOptions) *fakeDevice {
	t.Helper()
	return &fakeDevice{Device: sharedfake.Start(t, opts)}
}

func (fd *fakeDevice) baseURL() string {
	return fd.BaseURL()
}

func (fd *fakeDevice) counts() (status, login, signaling, rpc int) {
	return fd.Counts()
}

func (fd *fakeDevice) wireCounts() (rpc, hid int) {
	return fd.WireCounts()
}

func (fd *fakeDevice) mouseInterfaceState() (fakeAbsoluteMouseState, fakeRelativeMouseState) {
	return fd.MouseInterfaceState()
}

func (fd *fakeDevice) nextHIDFrame(t *testing.T) []byte {
	t.Helper()
	return fd.NextHIDFrame(t)
}

func (fd *fakeDevice) nextHIDWireFrame(t *testing.T) ([]byte, bool) {
	t.Helper()
	return fd.NextHIDWireFrame(t)
}

func (fd *fakeDevice) nextRPCWireFrame(t *testing.T) ([]byte, bool) {
	t.Helper()
	return fd.NextRPCWireFrame(t)
}

func (fd *fakeDevice) pendingFrames() (rpc, hid int) {
	return fd.PendingFrames()
}

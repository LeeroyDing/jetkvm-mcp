package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

func connectMockDevice(t *testing.T, fd *fakeDevice) *jetkvm.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := jetkvm.Connect(ctx, jetkvm.Options{BaseURL: fd.baseURL()})
	if err != nil {
		t.Fatalf("jetkvm.Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return client
}

func harnessConnector(fd *fakeDevice, attempts *int) deviceConnector {
	return func(ctx context.Context) (device, error) {
		(*attempts)++
		client, err := jetkvm.Connect(ctx, jetkvm.Options{BaseURL: fd.baseURL()})
		if err != nil {
			return nil, err
		}
		return &clientDevice{client: client}, nil
	}
}

func TestMockDeviceSlowStatusResponseRespectsDeadline(t *testing.T) {
	fd := startFakeDeviceWithOptions(t, fakeDeviceOptions{DeviceStatusDelay: 300 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := jetkvm.Connect(ctx, jetkvm.Options{BaseURL: fd.baseURL()})
	elapsed := time.Since(start)
	if kind := jetkvm.ErrorKindOf(err); kind != jetkvm.ErrorKindTimeout {
		t.Fatalf("error kind = %q, want timeout: %v", kind, err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("slow status response took %v despite a 40ms deadline", elapsed)
	}
	statusRequests, _, signalingConnections, rpcRequests := fd.counts()
	if statusRequests != 1 || signalingConnections != 0 || rpcRequests != 0 {
		t.Fatalf("slow status path progressed or retried: status=%d signaling=%d rpc=%d",
			statusRequests, signalingConnections, rpcRequests)
	}
}

func TestMockDeviceAuthFailureIsNotRetried(t *testing.T) {
	fd := startFakeDeviceWithOptions(t, fakeDeviceOptions{Password: "correct-password"})
	connectAttempts := 0
	connector := func(ctx context.Context) (device, error) {
		connectAttempts++
		client, err := jetkvm.Connect(ctx, jetkvm.Options{
			BaseURL:     fd.baseURL(),
			Credentials: jetkvm.Credentials{Password: jetkvm.NewSecret("wrong-password")},
		})
		if err != nil {
			return nil, err
		}
		return &clientDevice{client: client}, nil
	}
	retrying := newRetryingDeviceWithConnector(false, connector, immediateRetryPolicy(3, nil))
	t.Cleanup(func() { _ = retrying.close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := retrying.status(ctx)
	if kind := jetkvm.ErrorKindOf(err); kind != jetkvm.ErrorKindAuthFailed {
		t.Fatalf("error kind = %q, want auth-failed: %v", kind, err)
	}
	statusRequests, loginRequests, signalingConnections, rpcRequests := fd.counts()
	if connectAttempts != 1 || statusRequests != 1 || loginRequests != 1 || signalingConnections != 0 || rpcRequests != 0 {
		t.Fatalf("auth failure path retried or progressed: connects=%d status=%d login=%d signaling=%d rpc=%d",
			connectAttempts, statusRequests, loginRequests, signalingConnections, rpcRequests)
	}
}

func TestMockDeviceSlowRPCResponseRespectsDeadline(t *testing.T) {
	fd := startFakeDeviceWithOptions(t, fakeDeviceOptions{RPCResponseDelay: 300 * time.Millisecond})
	client := connectMockDevice(t, fd)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.Status(ctx)
	elapsed := time.Since(start)
	if kind := jetkvm.ErrorKindOf(err); kind != jetkvm.ErrorKindTimeout {
		t.Fatalf("error kind = %q, want timeout: %v", kind, err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("slow RPC took %v despite a 40ms deadline", elapsed)
	}
	_, _, _, rpcRequests := fd.counts()
	if rpcRequests != 1 {
		t.Fatalf("RPC requests = %d, want 1", rpcRequests)
	}
}

func TestMockDeviceBadRPCFramesFailImmediately(t *testing.T) {
	for _, mode := range []fakeRPCResponseMode{fakeRPCTruncated, fakeRPCOversized} {
		t.Run(string(mode), func(t *testing.T) {
			fd := startFakeDeviceWithOptions(t, fakeDeviceOptions{RPCResponseMode: mode})
			client := connectMockDevice(t, fd)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			start := time.Now()
			_, err := client.Status(ctx)
			elapsed := time.Since(start)
			if kind := jetkvm.ErrorKindOf(err); kind != jetkvm.ErrorKindBadFrame {
				t.Fatalf("error kind = %q, want bad-frame: %v", kind, err)
			}
			if !strings.Contains(err.Error(), "bad-frame") {
				t.Fatalf("error does not carry stable bad-frame marker: %v", err)
			}
			if elapsed > time.Second {
				t.Fatalf("bad %s frame degraded into a timeout after %v", mode, elapsed)
			}
		})
	}
}

func TestMockDeviceConnectFailuresRecoverWithinOneCall(t *testing.T) {
	fd := startFakeDeviceWithOptions(t, fakeDeviceOptions{DeviceStatusFailures: 2})
	connectAttempts := 0
	retrying := newRetryingDeviceWithConnector(false, harnessConnector(fd, &connectAttempts), immediateRetryPolicy(3, nil))
	t.Cleanup(func() { _ = retrying.close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	status, err := retrying.status(ctx)
	if err != nil {
		t.Fatalf("status did not recover: %v", err)
	}
	if !status.RPCReachable {
		t.Fatal("recovered status did not reach RPC")
	}
	statusRequests, _, signalingConnections, rpcRequests := fd.counts()
	if connectAttempts != 3 || statusRequests != 3 || signalingConnections != 1 || rpcRequests != 1 {
		t.Fatalf("retry path counts: connects=%d status=%d signaling=%d rpc=%d",
			connectAttempts, statusRequests, signalingConnections, rpcRequests)
	}
}

func TestMockDeviceDroppedRPCReconnectsWithinOneCall(t *testing.T) {
	fd := startFakeDeviceWithOptions(t, fakeDeviceOptions{RPCDisconnects: 1})
	connectAttempts := 0
	retrying := newRetryingDeviceWithConnector(false, harnessConnector(fd, &connectAttempts), immediateRetryPolicy(3, nil))
	t.Cleanup(func() { _ = retrying.close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	status, err := retrying.status(ctx)
	if err != nil {
		t.Fatalf("status did not recover from dropped RPC channel: %v", err)
	}
	if !status.RPCReachable {
		t.Fatal("reconnected status did not reach RPC")
	}
	statusRequests, _, signalingConnections, rpcRequests := fd.counts()
	if connectAttempts != 2 || statusRequests != 2 || signalingConnections != 2 || rpcRequests != 2 {
		t.Fatalf("reconnect path counts: connects=%d status=%d signaling=%d rpc=%d",
			connectAttempts, statusRequests, signalingConnections, rpcRequests)
	}
}

func TestMockDevicePersistentFailureStopsAtRetryLimit(t *testing.T) {
	fd := startFakeDeviceWithOptions(t, fakeDeviceOptions{DeviceStatusFailures: 10})
	connectAttempts := 0
	retrying := newRetryingDeviceWithConnector(false, harnessConnector(fd, &connectAttempts), immediateRetryPolicy(3, nil))
	t.Cleanup(func() { _ = retrying.close(context.Background()) })

	_, err := retrying.status(context.Background())
	if kind := jetkvm.ErrorKindOf(err); kind != jetkvm.ErrorKindUnreachable {
		t.Fatalf("error kind = %q, want unreachable: %v", kind, err)
	}
	statusRequests, _, signalingConnections, rpcRequests := fd.counts()
	if connectAttempts != 3 || statusRequests != 3 || signalingConnections != 0 || rpcRequests != 0 {
		t.Fatalf("persistent failure escaped bound: connects=%d status=%d signaling=%d rpc=%d",
			connectAttempts, statusRequests, signalingConnections, rpcRequests)
	}
}

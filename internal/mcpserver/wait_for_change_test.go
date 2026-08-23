package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

type waitForChangeMetadata struct {
	Changed             bool    `json:"changed"`
	FramesSampled       int     `json:"framesSampled"`
	FinalChangeFraction float64 `json:"finalChangeFraction"`
	Elapsed             string  `json:"elapsed"`
}

func decodeWaitForChangeMetadata(t testing.TB, result *mcp.CallToolResult) waitForChangeMetadata {
	t.Helper()
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshalling wait-for-change structured content: %v", err)
	}
	var metadata waitForChangeMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("decoding wait-for-change structured content: %v", err)
	}
	return metadata
}

func TestWaitForChangeToolDefaultsAndCustomOptions(t *testing.T) {
	tests := []struct {
		name          string
		args          map[string]any
		wantThreshold float64
		wantPoll      time.Duration
	}{
		{
			name:          "defaults",
			wantThreshold: jetkvm.DefaultWaitForChangeThreshold,
			wantPoll:      jetkvm.DefaultWaitForChangePollInterval,
		},
		{
			name:          "custom including explicit zeros",
			args:          map[string]any{"threshold": 0, "poll_interval_ms": 0},
			wantThreshold: 0,
			wantPoll:      0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			device := &mockDevice{waitForChangeFunc: func(_ context.Context, opts jetkvm.WaitForChangeOptions) (jetkvm.WaitForChangeResult, error) {
				calls++
				if opts.Threshold == nil || *opts.Threshold != test.wantThreshold ||
					opts.PollInterval == nil || *opts.PollInterval != test.wantPoll {
					return jetkvm.WaitForChangeResult{}, errors.New("wait-for-change handler passed unexpected options")
				}
				return jetkvm.WaitForChangeResult{
					Changed:             true,
					FramesSampled:       3,
					FinalChangeFraction: 0.5,
					Elapsed:             125 * time.Millisecond,
				}, nil
			}}
			cs := newTestServerSessionForDevice(t, device, true)
			params := &mcp.CallToolParams{Name: "jetkvm_wait_for_change"}
			if test.args != nil {
				params.Arguments = test.args
			}
			result, err := cs.CallTool(context.Background(), params)
			if err != nil {
				t.Fatalf("CallTool failed: %v", err)
			}
			if result == nil || result.IsError {
				t.Fatalf("wait-for-change result = %+v, want success", result)
			}
			if calls != 1 {
				t.Fatalf("device calls = %d, want 1", calls)
			}
			metadata := decodeWaitForChangeMetadata(t, result)
			if !metadata.Changed || metadata.FramesSampled != 3 ||
				metadata.FinalChangeFraction != 0.5 || metadata.Elapsed != "125ms" {
				t.Errorf("metadata = %+v", metadata)
			}
			wantText := "changed=true framesSampled=3 finalChangeFraction=0.5 elapsed=125ms"
			if got := toolResultText(t, result); got != wantText {
				t.Errorf("result text = %q, want %q", got, wantText)
			}
		})
	}
}

func TestWaitForChangeToolTimeoutReportsPartialObservations(t *testing.T) {
	want := jetkvm.WaitForChangeResult{
		FramesSampled:       4,
		FinalChangeFraction: 0.125,
		Elapsed:             2 * time.Second,
	}
	device := &mockDevice{waitForChangeFunc: func(context.Context, jetkvm.WaitForChangeOptions) (jetkvm.WaitForChangeResult, error) {
		return want, deviceFailure(jetkvm.ErrorKindTimeout, "waiting for screen change")
	}}
	cs := newTestServerSessionForDevice(t, device, true)
	result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "jetkvm_wait_for_change"})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("timeout result = %+v, want tool error", result)
	}
	metadata := decodeWaitForChangeMetadata(t, result)
	if metadata.Changed || metadata.FramesSampled != want.FramesSampled ||
		metadata.FinalChangeFraction != want.FinalChangeFraction || metadata.Elapsed != want.Elapsed.String() {
		t.Errorf("partial metadata = %+v", metadata)
	}
	for _, wantText := range []string{
		"jetkvm: timeout:",
		"changed=false framesSampled=4 finalChangeFraction=0.125 elapsed=2s",
	} {
		if got := toolResultText(t, result); !strings.Contains(got, wantText) {
			t.Errorf("timeout text %q does not contain %q", got, wantText)
		}
	}
}

func TestWaitForChangeToolRejectsInvalidOptionsBeforeDeviceCall(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
	}{
		{name: "threshold below zero", args: map[string]any{"threshold": -0.01}},
		{name: "threshold above one", args: map[string]any{"threshold": 1.01}},
		{name: "negative poll interval", args: map[string]any{"poll_interval_ms": -1}},
		{name: "overflowing poll interval", args: map[string]any{"poll_interval_ms": maxWaitStablePollIntervalMS + 1}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			device := &mockDevice{waitForChangeFunc: func(context.Context, jetkvm.WaitForChangeOptions) (jetkvm.WaitForChangeResult, error) {
				calls++
				return jetkvm.WaitForChangeResult{}, nil
			}}
			cs := newTestServerSessionForDevice(t, device, true)
			_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "jetkvm_wait_for_change",
				Arguments: test.args,
			})
			if err == nil {
				t.Fatal("invalid wait-for-change arguments were accepted")
			}
			var rpcErr *jsonrpc.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
				t.Fatalf("rejection = %v, want JSON-RPC InvalidParams", err)
			}
			if calls != 0 {
				t.Fatalf("invalid arguments reached device %d times", calls)
			}
		})
	}
}

func TestWaitForChangeArgumentAdapterBoundaries(t *testing.T) {
	valid := waitForChangeArgs{
		Threshold:      jetkvm.DefaultWaitForChangeThreshold,
		PollIntervalMS: jetkvm.DefaultWaitForChangePollInterval.Milliseconds(),
	}
	tests := []struct {
		name string
		args waitForChangeArgs
	}{
		{name: "NaN threshold", args: waitForChangeArgs{Threshold: math.NaN(), PollIntervalMS: valid.PollIntervalMS}},
		{name: "infinite threshold", args: waitForChangeArgs{Threshold: math.Inf(1), PollIntervalMS: valid.PollIntervalMS}},
		{name: "negative poll", args: waitForChangeArgs{Threshold: valid.Threshold, PollIntervalMS: -1}},
		{name: "overflowing poll", args: waitForChangeArgs{Threshold: valid.Threshold, PollIntervalMS: maxWaitStablePollIntervalMS + 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := waitForChangeOptionsFromArgs(test.args); err == nil {
				t.Fatalf("adapter accepted %+v", test.args)
			}
		})
	}
}

func TestWaitForChangeToolSchemaAndAnnotations(t *testing.T) {
	cs := newTestServerSessionForDevice(t, &mockDevice{}, true)
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	for _, tool := range listed.Tools {
		if tool.Name != "jetkvm_wait_for_change" {
			continue
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint ||
			tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint ||
			tool.Annotations.IdempotentHint {
			t.Errorf("annotations = %+v", tool.Annotations)
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshalling schema: %v", err)
		}
		var schema struct {
			Properties map[string]struct {
				Type    string          `json:"type"`
				Default json.RawMessage `json:"default"`
				Minimum *float64        `json:"minimum"`
				Maximum *float64        `json:"maximum"`
			} `json:"properties"`
			Required             []string `json:"required"`
			AdditionalProperties any      `json:"additionalProperties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decoding schema: %v", err)
		}
		if len(schema.Properties) != 2 || schema.Required != nil || schema.AdditionalProperties != false {
			t.Errorf("schema shape = %+v", schema)
		}
		threshold := schema.Properties["threshold"]
		if threshold.Type != "number" || string(threshold.Default) != "0.01" ||
			threshold.Minimum == nil || *threshold.Minimum != 0 ||
			threshold.Maximum == nil || *threshold.Maximum != 1 {
			t.Errorf("threshold schema = %+v", threshold)
		}
		poll := schema.Properties["poll_interval_ms"]
		if poll.Type != "integer" || string(poll.Default) != "250" ||
			poll.Minimum == nil || *poll.Minimum != 0 ||
			poll.Maximum == nil || *poll.Maximum != float64(maxWaitStablePollIntervalMS) {
			t.Errorf("poll schema = %+v", poll)
		}
		return
	}
	t.Fatal("jetkvm_wait_for_change was not advertised")
}

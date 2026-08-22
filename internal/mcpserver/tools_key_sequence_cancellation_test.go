package mcpserver

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

func TestKeySequenceInterComboTimeoutStopsAndReleasesAdmission(t *testing.T) {
	type sentCombo struct {
		modifier byte
		keys     []byte
	}

	var (
		combos     []sentCombo
		keypresses [][2]byte
	)
	device := &mockDevice{
		keyComboFunc: func(ctx context.Context, modifier byte, keys []byte) error {
			combos = append(combos, sentCombo{
				modifier: modifier,
				keys:     append([]byte(nil), keys...),
			})
			// Let the handler's own deadline expire after the first send. Returning
			// nil then makes the handler observe cancellation in the inter-combo
			// delay, before it can send the second chord.
			<-ctx.Done()
			return nil
		},
		keypressFunc: func(_ context.Context, modifier, key byte) error {
			keypresses = append(keypresses, [2]byte{modifier, key})
			return nil
		},
	}
	cs := newWaitForTextTestSession(t, device, true, 25*time.Millisecond, nil)

	result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "jetkvm_key_sequence",
		Arguments: map[string]any{
			"combos":   []string{"ctrl+c", "enter"},
			"delay_ms": 1,
		},
	})
	if err != nil {
		t.Fatalf("key sequence timeout returned protocol error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("key sequence timeout result = %+v, want MCP tool error", result)
	}
	if len(combos) != 1 || combos[0].modifier != jetkvm.ModifierLeftControl ||
		!bytes.Equal(combos[0].keys, []byte{jetkvm.KeyUsageC}) {
		t.Fatalf("key combos sent = %+v, want only ctrl+c", combos)
	}

	errorText := toolResultText(t, result)
	for _, want := range []string{"jetkvm: timeout:", "during MCP tool call", "call deadline expired"} {
		if !strings.Contains(errorText, want) {
			t.Errorf("timeout error %q does not contain %q", errorText, want)
		}
	}
	for _, leaked := range []string{"context deadline exceeded", "waiting before key sequence"} {
		if strings.Contains(errorText, leaked) {
			t.Errorf("timeout error exposed raw internal detail %q: %q", leaked, errorText)
		}
	}

	// The admission token is scoped to the complete composite call, including
	// its delay, and must be released on this error path.
	after, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "jetkvm_keypress",
		Arguments: map[string]any{"key": 4},
	})
	if err != nil {
		t.Fatalf("keypress after timed-out sequence returned protocol error: %v", err)
	}
	if after == nil || after.IsError {
		t.Fatalf("keypress after timed-out sequence = %+v, want success", after)
	}
	if len(keypresses) != 1 || keypresses[0] != [2]byte{0, 4} {
		t.Fatalf("keypresses sent after timeout = %v, want exact modifier/key bytes [[0 4]]", keypresses)
	}
}

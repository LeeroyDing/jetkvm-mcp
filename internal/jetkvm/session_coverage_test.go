package jetkvm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

func TestPumpSignalingEventsBoundaries(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{
			name: "pre-canceled session stops before reading",
			run: func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				diag := newVideoDiagnostics()

				pumpSignalingEvents(ctx, nil, nil, diag)

				snapshot := diag.snapshot(nil)
				if snapshot.SignalingPumpStillActive || snapshot.SignalingPumpStopped != "session-closed" {
					t.Fatalf("pre-canceled pump state = active %v, reason %q", snapshot.SignalingPumpStillActive, snapshot.SignalingPumpStopped)
				}
			},
		},
		{
			name: "pre-answer candidate queue is capped",
			run: func(t *testing.T) {
				s := dialTestSignaler(t, func(ctx context.Context, conn *websocket.Conn) {
					for i := 0; i < maxPendingRemoteCandidates+1; i++ {
						payload, err := json.Marshal(map[string]any{
							"type": "new-ice-candidate",
							"data": map[string]any{
								"candidate": "candidate:1 1 UDP 2130706431 192.0.2.1 5000 typ host",
							},
						})
						if err != nil {
							t.Errorf("marshal candidate %d: %v", i, err)
							return
						}
						if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
							return
						}
					}
					marker, err := json.Marshal(map[string]any{"type": "queue-boundary-observed", "data": nil})
					if err != nil {
						t.Errorf("marshal marker: %v", err)
						return
					}
					_ = conn.Write(ctx, websocket.MessageText, marker)
				})

				pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
				if err != nil {
					t.Fatalf("NewPeerConnection: %v", err)
				}
				defer pc.Close()

				diag := newVideoDiagnostics()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan struct{})
				go func() {
					defer close(done)
					pumpSignalingEvents(ctx, s, pc, diag)
				}()

				waitForCondition(t, 5*time.Second, func() bool {
					return diag.snapshot(pc).UnhandledSignalingMsgs == 1
				})
				snapshot := diag.snapshot(pc)
				if snapshot.RemoteICECandidatesQueued != maxPendingRemoteCandidates {
					t.Errorf("queued candidates = %d, want cap %d", snapshot.RemoteICECandidatesQueued, maxPendingRemoteCandidates)
				}
				if snapshot.RemoteICECandidatesBad != 1 {
					t.Errorf("rejected candidates = %d, want 1 overflow", snapshot.RemoteICECandidatesBad)
				}

				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("pump did not stop after cancellation")
				}
			},
		},
		{
			name: "invalid answer is recorded as rejected",
			run: func(t *testing.T) {
				s := dialTestSignaler(t, func(ctx context.Context, conn *websocket.Conn) {
					answerJSON, err := json.Marshal(webrtc.SessionDescription{
						Type: webrtc.SDPTypeAnswer,
						SDP:  "not-valid-sdp",
					})
					if err != nil {
						t.Errorf("marshal answer: %v", err)
						return
					}
					encoded, err := json.Marshal(base64.StdEncoding.EncodeToString(answerJSON))
					if err != nil {
						t.Errorf("marshal encoded answer: %v", err)
						return
					}
					frame, err := json.Marshal(signalingMessage{Type: "answer", Data: encoded})
					if err != nil {
						t.Errorf("marshal answer frame: %v", err)
						return
					}
					if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
						return
					}
					marker, err := json.Marshal(map[string]any{"type": "answer-boundary-observed", "data": nil})
					if err != nil {
						t.Errorf("marshal marker: %v", err)
						return
					}
					_ = conn.Write(ctx, websocket.MessageText, marker)
				})

				pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
				if err != nil {
					t.Fatalf("NewPeerConnection: %v", err)
				}
				defer pc.Close()

				diag := newVideoDiagnostics()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan struct{})
				go func() {
					defer close(done)
					pumpSignalingEvents(ctx, s, pc, diag)
				}()

				waitForCondition(t, 5*time.Second, func() bool {
					return diag.snapshot(pc).UnhandledSignalingMsgs == 1
				})
				snapshot := diag.snapshot(pc)
				if !snapshot.AnswerRejected || snapshot.AnswerApplied {
					t.Fatalf("invalid answer outcome = applied %v, rejected %v", snapshot.AnswerApplied, snapshot.AnswerRejected)
				}

				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("pump did not stop after cancellation")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

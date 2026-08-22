package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/leeroyding/jetkvm-mcp/internal/jetkvm"
)

func TestRetryCoverageJitterDelayEdges(t *testing.T) {
	tests := []struct {
		name  string
		delay time.Duration
		want  time.Duration
	}{
		{name: "negative", delay: -time.Nanosecond, want: 0},
		{name: "zero", delay: 0, want: 0},
		{name: "too small to spread", delay: 3 * time.Nanosecond, want: 3 * time.Nanosecond},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := jitterDelay(tc.delay); got != tc.want {
				t.Fatalf("jitterDelay(%v) = %v, want %v", tc.delay, got, tc.want)
			}
		})
	}
}

func TestRetryCoverageSleepContextOutcomes(t *testing.T) {
	tests := []struct {
		name    string
		delay   time.Duration
		cancel  bool
		wantErr error
	}{
		{name: "timer fires", delay: 25 * time.Millisecond},
		{name: "caller canceled", delay: time.Hour, cancel: true, wantErr: context.Canceled},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if tc.cancel {
					cancel()
				}

				err := sleepContext(ctx, tc.delay)
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("sleepContext(%v) error = %v, want %v", tc.delay, err, tc.wantErr)
				}
			})
		})
	}
}

func TestRetryCoveragePolicyNormalizationAndDelayEdges(t *testing.T) {
	tests := []struct {
		name            string
		policy          retryPolicy
		failedAttempt   int
		wantDelay       time.Duration
		wantBaseDelay   time.Duration
		wantMaxDelay    time.Duration
		wantMaxAttempts int
	}{
		{
			name:            "negative base is normalized and default jitter is identity",
			policy:          retryPolicy{maxAttempts: 1, baseDelay: -time.Millisecond, maxDelay: time.Second},
			failedAttempt:   1,
			wantDelay:       0,
			wantBaseDelay:   0,
			wantMaxDelay:    time.Second,
			wantMaxAttempts: 1,
		},
		{
			name:            "uncapped exponential step doubles",
			policy:          retryPolicy{maxAttempts: 3, baseDelay: 10 * time.Millisecond, maxDelay: 100 * time.Millisecond},
			failedAttempt:   2,
			wantDelay:       20 * time.Millisecond,
			wantBaseDelay:   10 * time.Millisecond,
			wantMaxDelay:    100 * time.Millisecond,
			wantMaxAttempts: 3,
		},
		{
			name:            "base above hard cap is clamped",
			policy:          retryPolicy{maxAttempts: 1, baseDelay: 20 * time.Millisecond, maxDelay: 10 * time.Millisecond},
			failedAttempt:   1,
			wantDelay:       10 * time.Millisecond,
			wantBaseDelay:   20 * time.Millisecond,
			wantMaxDelay:    10 * time.Millisecond,
			wantMaxAttempts: 1,
		},
		{
			name: "negative jitter is clamped to zero",
			policy: retryPolicy{
				maxAttempts: 1,
				baseDelay:   10 * time.Millisecond,
				maxDelay:    100 * time.Millisecond,
				jitter:      func(time.Duration) time.Duration { return -time.Nanosecond },
			},
			failedAttempt:   1,
			wantDelay:       0,
			wantBaseDelay:   10 * time.Millisecond,
			wantMaxDelay:    100 * time.Millisecond,
			wantMaxAttempts: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newRetryingDeviceWithConnector(false, func(context.Context) (device, error) {
				return &mockDevice{}, nil
			}, tc.policy)

			if got := client.retryDelay(tc.failedAttempt); got != tc.wantDelay {
				t.Errorf("retryDelay(%d) = %v, want %v", tc.failedAttempt, got, tc.wantDelay)
			}
			if client.policy.baseDelay != tc.wantBaseDelay || client.policy.maxDelay != tc.wantMaxDelay {
				t.Errorf("normalized delays = %v/%v, want %v/%v",
					client.policy.baseDelay, client.policy.maxDelay, tc.wantBaseDelay, tc.wantMaxDelay)
			}
			if client.policy.maxAttempts != tc.wantMaxAttempts {
				t.Errorf("normalized maxAttempts = %d, want %d", client.policy.maxAttempts, tc.wantMaxAttempts)
			}
			if client.policy.jitter == nil || client.policy.sleep == nil {
				t.Fatal("constructor left retry policy callbacks nil")
			}
		})
	}
}

func TestRetryCoverageProductionConstructorRejectsInvalidURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{name: "unsupported scheme", baseURL: "ftp://jetkvm.invalid", want: "scheme must be http or https"},
		{name: "userinfo", baseURL: "http://operator:secret@jetkvm.invalid", want: "must not contain userinfo"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newRetryingDevice(Options{BaseURL: tc.baseURL})
			_, err := client.status(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("status with invalid base URL %q = %v, want validation error", tc.baseURL, err)
			}
			if client.current != nil {
				t.Fatal("invalid base URL retained a device session")
			}
		})
	}
}

func TestRetryCoverageCancellationAfterPreflight(t *testing.T) {
	tests := []struct {
		name           string
		retryOperation bool
	}{
		{name: "read only", retryOperation: true},
		{name: "control", retryOperation: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			connectCalls := 0
			operationCalls := 0
			client := newRetryingDeviceWithConnector(true, func(context.Context) (device, error) {
				connectCalls++
				return &mockDevice{}, nil
			}, immediateRetryPolicy(1, nil))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			err := client.doWithPreflight(
				ctx,
				"post-preflight cancellation",
				tc.retryOperation,
				false,
				func(context.Context) error {
					cancel()
					return nil
				},
				func(device) error {
					operationCalls++
					return nil
				},
			)
			if jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindTimeout ||
				!strings.Contains(err.Error(), "call deadline expired") {
				t.Fatalf("post-preflight cancellation error = %v, want stable timeout", err)
			}
			if connectCalls != 0 || operationCalls != 0 {
				t.Fatalf("canceled call performed work: connects=%d operations=%d", connectCalls, operationCalls)
			}
		})
	}
}

func TestRetryCoverageChainedDiscardWaitsAndJoinsCleanup(t *testing.T) {
	firstErr := errors.New("first cleanup failed")
	secondErr := errors.New("second cleanup failed")
	tests := []struct {
		name      string
		firstErr  error
		secondErr error
	}{
		{name: "successful chain"},
		{name: "failed chain", firstErr: firstErr, secondErr: secondErr},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				firstStarted := make(chan struct{})
				secondStarted := make(chan struct{})
				releaseFirst := make(chan struct{})
				releaseSecond := make(chan struct{})
				first := &mockDevice{closeFunc: func(context.Context) error {
					close(firstStarted)
					<-releaseFirst
					return tc.firstErr
				}}
				second := &mockDevice{closeFunc: func(context.Context) error {
					close(secondStarted)
					<-releaseSecond
					return tc.secondErr
				}}
				client := newRetryingDeviceWithConnector(true, nil, immediateRetryPolicy(1, nil))

				client.current = first
				client.discard(first)
				client.current = second
				client.discard(second)
				pending := client.cleanup
				synctest.Wait()

				select {
				case <-firstStarted:
				default:
					t.Fatal("first discarded session did not start cleanup")
				}
				select {
				case <-secondStarted:
				default:
					t.Fatal("second discarded session did not start cleanup")
				}

				close(releaseSecond)
				synctest.Wait()
				select {
				case <-pending.done:
					t.Fatal("latest cleanup completed before its predecessor")
				default:
				}

				close(releaseFirst)
				synctest.Wait()
				got, err := client.awaitCleanup(context.Background(), "test cleanup chain")
				if err != nil {
					t.Fatalf("awaitCleanup: %v", err)
				}
				for _, want := range []error{tc.firstErr, tc.secondErr} {
					if want != nil && !errors.Is(got, want) {
						t.Errorf("cleanup error %v does not contain %v", got, want)
					}
				}
				if tc.firstErr == nil && tc.secondErr == nil && got != nil {
					t.Errorf("successful cleanup chain returned %v", got)
				}
			})
		})
	}
}

func TestRetryCoverageControlValidationPrecedesConnection(t *testing.T) {
	tests := []struct {
		name         string
		allowControl bool
		invoke       func(*retryingDevice) (bool, error)
		wantErr      bool
	}{
		{
			name: "release all without control is a no-op",
			invoke: func(client *retryingDevice) (bool, error) {
				return client.releaseAll(context.Background())
			},
		},
		{
			name:         "zero hold duration",
			allowControl: true,
			invoke: func(client *retryingDevice) (bool, error) {
				return false, client.holdKey(context.Background(), 0, []byte{0x04}, 0)
			},
			wantErr: true,
		},
		{
			name:         "invalid key usage",
			allowControl: true,
			invoke: func(client *retryingDevice) (bool, error) {
				return false, client.holdKey(context.Background(), 0, []byte{0}, 1)
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			connectCalls := 0
			client := newRetryingDeviceWithConnector(tc.allowControl, func(context.Context) (device, error) {
				connectCalls++
				return &mockDevice{}, nil
			}, immediateRetryPolicy(1, nil))

			result, err := tc.invoke(client)
			if (err != nil) != tc.wantErr {
				t.Fatalf("operation error = %v, wantErr %v", err, tc.wantErr)
			}
			if result {
				t.Fatal("rejected operation reported a successful release")
			}
			if connectCalls != 0 {
				t.Fatalf("rejected operation made %d connection attempts, want zero", connectCalls)
			}
		})
	}
}

func TestRetryCoverageCloseRespectsCanceledGateWait(t *testing.T) {
	tests := []struct {
		name       string
		newContext func() (context.Context, context.CancelFunc)
	}{
		{
			name: "canceled",
			newContext: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
		},
		{
			name: "deadline exceeded",
			newContext: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Unix(1, 0))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newRetryingDeviceWithConnector(false, nil, immediateRetryPolicy(1, nil))
			client.gate <- struct{}{}
			defer func() { <-client.gate }()
			ctx, cancel := tc.newContext()
			defer cancel()

			err := client.close(ctx)
			if jetkvm.ErrorKindOf(err) != jetkvm.ErrorKindTimeout ||
				!strings.Contains(err.Error(), "waiting for another MCP device call") {
				t.Fatalf("close while gate is held = %v, want stable acquire timeout", err)
			}
		})
	}
}

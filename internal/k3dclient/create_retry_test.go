package k3dclient

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	l "github.com/k3d-io/k3d/v5/pkg/logger"
)

const swallowedWrite = "Node k3d-demo-server-0: Failed executing preStartAction 'WriteFileAction': " +
	"Error response from daemon: error while creating mount source path '/host_mnt/data/storage'"

// The real trigger needs Docker Desktop and a volume source whose parent was
// deleted and recreated after a cluster mounted it, so CI cannot reproduce it.
// These tests cover the seam instead: what k3d logs and what it returns.
func TestRunWithRetry(t *testing.T) {
	failed := errors.New("failed to get ready")

	tests := []struct {
		name         string
		attempts     []func() error
		wantCalls    int
		wantCleanups int
		wantErr      []string
	}{
		{
			name:      "success does not retry",
			attempts:  []func() error{func() error { return nil }},
			wantCalls: 1,
		},
		{
			name: "swallowed entrypoint write retries and recovers",
			attempts: []func() error{
				func() error { l.Log().Error(swallowedWrite); return failed },
				func() error { return nil },
			},
			wantCalls:    2,
			wantCleanups: 1,
		},
		{
			name: "swallowed entrypoint write fails twice",
			attempts: []func() error{
				func() error { l.Log().Error(swallowedWrite); return failed },
				func() error { l.Log().Error(swallowedWrite); return failed },
			},
			wantCalls:    2,
			wantCleanups: 2,
			wantErr:      []string{"failed to get ready", "creating mount source path"},
		},
		{
			name: "other failure does not retry",
			attempts: []func() error{
				func() error { return failed },
				func() error { t.Error("retried a failure k3d did not hide"); return nil },
			},
			wantCalls:    1,
			wantCleanups: 1,
			wantErr:      []string{"failed to get ready"},
		},
		{
			name: "other logged error is reported but does not retry",
			attempts: []func() error{
				func() error { l.Log().Error("image pull failed"); return failed },
				func() error { t.Error("retried an unrelated failure"); return nil },
			},
			wantCalls:    1,
			wantCleanups: 1,
			wantErr:      []string{"failed to get ready", "image pull failed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls, cleanups := 0, 0
			err := runWithRetry(context.Background(), time.Millisecond,
				func() error {
					calls++
					return tt.attempts[calls-1]()
				},
				func(cause error) error {
					cleanups++
					return cause
				},
			)

			if calls != tt.wantCalls {
				t.Errorf("attempts = %d, want %d", calls, tt.wantCalls)
			}
			if cleanups != tt.wantCleanups {
				t.Errorf("cleanups = %d, want %d", cleanups, tt.wantCleanups)
			}
			if len(tt.wantErr) == 0 && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, want := range tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("error %v does not contain %q", err, want)
				}
			}
		})
	}
}

func TestRunWithRetryStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0

	err := runWithRetry(ctx, time.Hour,
		func() error {
			calls++
			cancel()
			l.Log().Error(swallowedWrite)
			return errors.New("failed to get ready")
		},
		func(cause error) error { return cause },
	)

	if calls != 1 {
		t.Errorf("attempts = %d, want 1", calls)
	}
	if err == nil || !strings.Contains(err.Error(), "creating mount source path") {
		t.Errorf("error %v does not include the logged cause", err)
	}
}

func TestLogRecorderIgnoresErrorsOutsideARun(t *testing.T) {
	l.Log().Error("before the run")

	err := runWithRetry(context.Background(), time.Millisecond,
		func() error { return errors.New("failed") },
		func(cause error) error { return cause },
	)

	if strings.Contains(err.Error(), "before the run") {
		t.Errorf("error %v includes a message logged outside the run", err)
	}
}

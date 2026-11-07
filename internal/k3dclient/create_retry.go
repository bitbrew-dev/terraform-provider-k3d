package k3dclient

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	l "github.com/k3d-io/k3d/v5/pkg/logger"
	"github.com/sirupsen/logrus"
)

const (
	createAttempts      = 2
	createRetryDelay    = 5 * time.Second
	swallowedHookSignal = "Failed executing preStartAction"
	maxReportedErrors   = 2
)

var k3dErrors = &errorRecorder{}

func init() {
	l.Log().AddHook(k3dErrors)
}

// errorRecorder keeps what k3d logs at error level during a create, because k3d
// logs some failures without returning them.
type errorRecorder struct {
	mu        sync.Mutex
	recording bool
	messages  []string
}

// Levels selects the log levels the recorder receives.
func (r *errorRecorder) Levels() []logrus.Level {
	return []logrus.Level{logrus.ErrorLevel}
}

// Fire stores an entry while a create is running.
func (r *errorRecorder) Fire(entry *logrus.Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recording {
		r.messages = append(r.messages, entry.Message)
	}
	return nil
}

func (r *errorRecorder) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recording = true
	r.messages = nil
}

func (r *errorRecorder) stop() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recording = false
	messages := r.messages
	r.messages = nil
	return messages
}

// runWithRetry runs one cluster create and retries once if k3d hid a failed
// pre-start hook. k3d writes its entrypoint script in such a hook, logs a failed
// write, and starts the container anyway. The container then exits with 127, and
// k3d only reports that the node failed to get ready.
//
// Docker Desktop causes this when a volume source's parent was deleted and
// recreated after a cluster mounted it, and the path works again moments later.
// Other failures, such as a k3s crash, are not retried. Callers must serialize
// calls, because the recorder is shared.
func runWithRetry(ctx context.Context, delay time.Duration, run func() error, cleanup func(cause error) error) error {
	var err error
	for attempt := 1; attempt <= createAttempts; attempt++ {
		k3dErrors.start()
		runErr := run()
		logged := k3dErrors.stop()
		if runErr == nil {
			return nil
		}

		err = cleanup(withLoggedErrors(runErr, logged))
		if attempt == createAttempts || !hasSwallowedHook(logged) {
			return err
		}

		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
	}
	return err
}

func hasSwallowedHook(logged []string) bool {
	for _, message := range logged {
		if strings.Contains(message, swallowedHookSignal) {
			return true
		}
	}
	return false
}

func withLoggedErrors(err error, logged []string) error {
	if len(logged) == 0 {
		return err
	}
	shown := logged[:min(len(logged), maxReportedErrors)]
	report := strings.Join(shown, "\n")
	if hidden := len(logged) - len(shown); hidden > 0 {
		report += fmt.Sprintf("\n(%d more)", hidden)
	}
	return fmt.Errorf("%w\nk3d logged:\n%s", err, report)
}

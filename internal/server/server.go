package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"egg-emulator/internal/environment"
	"egg-emulator/internal/interaction"
	"egg-emulator/pkg/ansi"
)

// StatusCallback is invoked whenever the server status or activity changes.
type StatusCallback func(status string, stats environment.Stats, activity string)

// Server manages the lifecycle of a single test run for one egg image.
// It does NOT handle installation — that is the runner's responsibility.
type Server struct {
	env         environment.Environment
	interaction *interaction.Manager
	donePattern string
}

// New creates a Server.
// donePattern overrides whatever the egg specifies; if empty, "Started" is used.
func New(env environment.Environment, im *interaction.Manager, donePattern string) *Server {
	if donePattern == "" {
		donePattern = "Started"
	}
	return &Server{
		env:         env,
		interaction: im,
		donePattern: donePattern,
	}
}

const (
	maxAttempts    = 3
	retryDelay     = 2 * time.Second
	statsInterval  = 2 * time.Second
	cumulativeMax  = 20_000 // bytes: trim cumulative buffer above this size
	cumulativeTrim = 10_000 // bytes: keep this many bytes after trimming
)

// Run executes the full server boot sequence with retry logic.
// It calls Create → UploadFiles → Start on the environment, then monitors
// the log until donePattern is found or an error/timeout occurs.
// Installation must be completed before calling Run.
func (s *Server) Run(ctx context.Context, logWriter io.Writer, callback StatusCallback) error {
	update := func(status, activity string) {
		if callback != nil {
			callback(status, environment.Stats{}, activity)
		}
	}
	updateWithStats := func(status, activity string, stats environment.Stats) {
		if callback != nil {
			callback(status, stats, activity)
		}
	}

	update("Creating environment...", "")
	if err := s.env.Create(ctx); err != nil {
		return fmt.Errorf("server: create: %w", err)
	}

	update("Uploading files...", "")
	if err := s.env.UploadFiles(); err != nil {
		return fmt.Errorf("server: upload files: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			update("Retrying...", fmt.Sprintf("Attempt %d/%d — previous error: %v", attempt, maxAttempts, lastErr))
			s.env.Stop(ctx) //nolint:errcheck
			time.Sleep(retryDelay)
		}

		update("Starting container...", "")
		if err := s.env.Start(ctx); err != nil {
			lastErr = fmt.Errorf("server: start (attempt %d): %w", attempt, err)
			continue
		}

		update("Waiting for boot pattern...", fmt.Sprintf("watching for %q", s.donePattern))

		// Reset once-interaction state for each fresh attempt
		s.interaction.ResetFired()

		if err := s.waitForBoot(ctx, logWriter, update, updateWithStats); err != nil {
			lastErr = err
			if attempt < maxAttempts {
				continue
			}
			update("Test Failed!", err.Error())
			return err
		}
		return nil // success
	}

	return lastErr
}

// waitForBoot monitors the container log until the done pattern is found,
// an error pattern matches, the container exits unexpectedly, or ctx expires.
func (s *Server) waitForBoot(
	ctx context.Context,
	logWriter io.Writer,
	update func(string, string),
	updateWithStats func(string, string, environment.Stats),
) error {
	attemptCtx, cancelAttempt := context.WithCancel(ctx)
	defer cancelAttempt()

	// successCh is closed when the done pattern is found
	successCh := make(chan struct{})
	// errCh carries the first fatal error from any goroutine
	errCh := make(chan error, 2)

	envWriter := &stdinWriter{env: s.env}

	// ── Stats ticker ────────────────────────────────────────────────────────
	go func() {
		ticker := time.NewTicker(statsInterval)
		defer ticker.Stop()
		for {
			select {
			case <-attemptCtx.Done():
				return
			case <-ticker.C:
				if s.env.State() == environment.StateRunning {
					if st, err := s.env.Stats(attemptCtx); err == nil {
						updateWithStats("Running", "", st)
					}
				}
			}
		}
	}()

	// ── Log processor ────────────────────────────────────────────────────────
	go func() {
		pr, pw := io.Pipe()
		go func() {
			err := s.env.ReadLog(attemptCtx, pw)
			pw.CloseWithError(err) //nolint:errcheck
		}()

		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)

		cumulative := strings.Builder{}

		for scanner.Scan() {
			line := scanner.Text()
			fmt.Fprintln(logWriter, line)

			clean := ansi.Strip(line)
			cumulative.WriteString(clean)
			cumulative.WriteByte('\n')

			// Trim cumulative buffer to avoid unbounded memory growth
			if cumulative.Len() > cumulativeMax {
				old := cumulative.String()
				cumulative.Reset()
				cumulative.WriteString(old[len(old)-cumulativeTrim:])
			}

			buf := cumulative.String()

			// Error pattern check (per-line, fast)
			if errMsg, found := s.interaction.CheckError(clean); found {
				errCh <- fmt.Errorf("error pattern matched: %s", errMsg)
				return
			}

			// Interaction check against accumulated buffer
			result := s.interaction.Check(buf, envWriter)
			if result.ShouldFail {
				errCh <- fmt.Errorf("fail interaction triggered: %s", result.FailMessage)
				return
			}
			if result.ShouldSucceed {
				close(successCh)
				return
			}

			// Activity update for TUI
			if display := strings.TrimSpace(clean); display != "" {
				if len(display) > 120 {
					display = display[:117] + "..."
				}
				update("Running", display)
			}

			// Done pattern check
			if strings.Contains(buf, s.donePattern) {
				close(successCh)
				return
			}
		}
		// Scanner ended (container closed pipe)
		select {
		case <-successCh:
		default:
			errCh <- fmt.Errorf("log stream ended before done pattern was found")
		}
	}()

	// ── Container exit monitor ───────────────────────────────────────────────
	go func() {
		exitCode, err := s.env.WaitForStop(attemptCtx)
		if err != nil {
			if attemptCtx.Err() == nil {
				errCh <- fmt.Errorf("environment stopped unexpectedly: %w", err)
			}
			return
		}
		// Give the log processor a moment to find the pattern in the last output
		time.Sleep(500 * time.Millisecond)
		select {
		case <-successCh:
			return // already succeeded
		default:
			if exitCode != 0 {
				errCh <- fmt.Errorf("container exited with code %d before done pattern", exitCode)
			} else {
				errCh <- fmt.Errorf("container stopped cleanly before done pattern was found")
			}
		}
	}()

	// ── Wait for resolution ─────────────────────────────────────────────────
	select {
	case <-successCh:
		update("Test Passed!", fmt.Sprintf("matched %q", s.donePattern))
		return nil
	case err := <-errCh:
		return err
	case <-ctx.Done():
		update("Test Timed Out", "")
		return ctx.Err()
	}
}

// stdinWriter wraps an environment to implement io.Writer for interaction responses.
type stdinWriter struct {
	env environment.Environment
}

func (w *stdinWriter) Write(p []byte) (int, error) {
	cmd := strings.TrimSpace(string(p))
	if cmd != "" {
		if err := w.env.SendCommand(cmd); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

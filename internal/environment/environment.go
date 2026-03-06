package environment

import (
	"context"
	"io"
)

// State represents the lifecycle state of a server environment.
type State string

const (
	StateOffline  State = "offline"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
)

// Stats holds real-time resource usage for a running environment.
type Stats struct {
	CPUUsage    float64 // percentage (0–100*ncores)
	MemoryUsage float64 // bytes
	MemoryLimit float64 // bytes
}

// Environment is the interface every backend (Docker, etc.) must satisfy.
type Environment interface {
	// Create pulls the image and creates the container (does NOT start it).
	Create(ctx context.Context) error

	// Install runs the egg's installation script inside a temporary container.
	// Progress output is written to writer.
	Install(ctx context.Context, writer io.Writer) error

	// UploadFiles writes pattern-defined files into the data directory
	// and applies Pterodactyl config-file transformations.
	UploadFiles() error

	// Start attaches to the container streams and starts it.
	Start(ctx context.Context) error

	// Stop gracefully stops the container.
	Stop(ctx context.Context) error

	// Terminate forcibly removes the container (used for cleanup).
	Terminate(ctx context.Context) error

	// State returns the current lifecycle state.
	State() State

	// ReadLog streams the container's combined stdout/stderr to writer.
	// Blocks until the context is cancelled or the container exits.
	ReadLog(ctx context.Context, writer io.Writer) error

	// SendCommand writes a command line to the container's stdin.
	SendCommand(command string) error

	// WaitForStop blocks until the container exits and returns its exit code.
	WaitForStop(ctx context.Context) (int, error)

	// Stats returns the current CPU/memory usage.
	Stats(ctx context.Context) (Stats, error)
}

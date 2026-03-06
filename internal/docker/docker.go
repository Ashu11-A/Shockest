// Package docker provides a Docker-backed implementation of environment.Environment.
package docker

import (
	"egg-emulator/internal/config"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

// DockerEnvironment implements environment.Environment using the Docker daemon.
type DockerEnvironment struct {
	client        *client.Client
	containerID   string
	egg           *config.Egg
	envVars       []string
	files         []config.FileContent
	dataPath      string
	selectedImage string
	logsDir       string
	// stream holds the hijacked attach connection used for stdin/stdout/stderr.
	stream *types.HijackedResponse

	// Ensure DockerEnvironment satisfies the environment.Environment interface
	// at compile time (checked in container.go via var _ = ...)
}

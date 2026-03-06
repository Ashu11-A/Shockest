package docker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"egg-emulator/internal/config"
	"egg-emulator/internal/parser"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
)

// New creates a DockerEnvironment for the given egg and resolves the data path.
// envVars is a slice of "KEY=VALUE" strings that override the egg's defaults.
// files are extra files to write into the container before startup.
// selectedImage is the key into egg.DockerImages.
// logsDir, when non-empty, is mounted for proxy CA trust (CURL_CA_BUNDLE) for HTTPS MITM.
func New(
	egg *config.Egg,
	envVars []string,
	files []config.FileContent,
	selectedImage string,
	logsDir string,
) (*DockerEnvironment, error) {
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker: create client: %w", err)
	}

	absPath, err := filepath.Abs(filepath.Join("data", egg.Name))
	if err != nil {
		return nil, fmt.Errorf("docker: resolve data path: %w", err)
	}
	if err := os.MkdirAll(absPath, 0o777); err != nil {
		return nil, fmt.Errorf("docker: mkdir data path: %w", err)
	}
	os.Chmod(absPath, 0o777) // Ensure it's 0777 even if it already exists

	// Sanitize env vars (strip carriage returns)
	clean := make([]string, len(envVars))
	for i, v := range envVars {
		clean[i] = strings.ReplaceAll(v, "\r", "")
	}

	return &DockerEnvironment{
		client:        cli,
		egg:           egg,
		envVars:       clean,
		files:         files,
		dataPath:      absPath,
		selectedImage: selectedImage,
		logsDir:       logsDir,
	}, nil
}

// imageURL returns the Docker image URL for the selected image name.
func (d *DockerEnvironment) imageURL() (string, error) {
	url, ok := d.egg.DockerImages[d.selectedImage]
	if !ok || url == "" {
		// Fall back to first available image
		for _, u := range d.egg.DockerImages {
			return strings.TrimSpace(u), nil
		}
		return "", fmt.Errorf("docker: no docker image configured for egg %q", d.egg.Name)
	}
	return strings.TrimSpace(url), nil
}

// Create pulls the server image and creates the container (does NOT start it).
// FIX: Install() is intentionally NOT called here — see runner.go for install flow.
func (d *DockerEnvironment) Create(ctx context.Context) error {
	imgURL, err := d.imageURL()
	if err != nil {
		return err
	}

	pullReader, err := d.client.ImagePull(ctx, imgURL, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("docker: pull %s: %w", imgURL, err)
	}
	io.Copy(io.Discard, pullReader) //nolint:errcheck
	pullReader.Close()

	startup := strings.ReplaceAll(d.egg.Startup, "\r", "")

	startScriptPath := filepath.Join(d.logsDir, d.egg.Name+".start.sh")
	if d.logsDir != "" {
		if err := os.WriteFile(startScriptPath, []byte(startup), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to write startup script to %s: %v\n", startScriptPath, err)
		}
	}
	// Build the full environment for the container
	env := make([]string, 0, len(d.envVars)+15)
	env = append(env, d.envVars...)
	env = append(env,
		"STARTUP="+startup,
		"SERVER_MEMORY=4096",
		"SERVER_PORT=25565",
		"SERVER_IP=0.0.0.0",
		"TZ=Etc/UTC",
		"P_SERVER_LOCATION=BR",
		// Proxy configuration
		"HTTP_PROXY=http://host.docker.internal:8080",
		"HTTPS_PROXY=http://host.docker.internal:8080",
		"http_proxy=http://host.docker.internal:8080",
		"https_proxy=http://host.docker.internal:8080",
		"NO_PROXY=localhost,127.0.0.1",
		"GIT_SSL_NO_VERIFY=true",
	)
	mounts := []mount.Mount{
		{
			Type:   mount.TypeBind,
			Source: d.dataPath,
			Target: "/home/container",
		},
	}
	if d.logsDir != "" {
		if absLogs, err := filepath.Abs(d.logsDir); err == nil {
			env = append(env, "CURL_CA_BUNDLE=/tmp/egg-proxy-ca/proxy-ca.pem", "SSL_CERT_FILE=/tmp/egg-proxy-ca/proxy-ca.pem")
			mounts = append(mounts, mount.Mount{
				Type:   mount.TypeBind,
				Source: absLogs,
				Target: "/tmp/egg-proxy-ca",
			})
		}
	}

	dockerConfig := &container.Config{
		Image: imgURL,
		Cmd: []string{
			"/bin/bash", "-c",
			`cd /home/container && MODIFIED_STARTUP=$(echo -e ${STARTUP} | sed -e 's/{{/${/g' -e 's/}}/}/g'); eval ${MODIFIED_STARTUP}`,
		},
		Env:          env,
		Tty:          true,
		OpenStdin:    true,
		StdinOnce:    false,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		WorkingDir:   "/home/container",
	}

	hostConfig := &container.HostConfig{
		AutoRemove: false,
		Mounts:     mounts,
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
	}

	resp, err := d.client.ContainerCreate(ctx, dockerConfig, hostConfig, nil, nil, "")
	if err != nil {
		return fmt.Errorf("docker: create container: %w", err)
	}
	d.containerID = resp.ID
	return nil
}

// UploadFiles writes pattern-defined files and applies egg config-file transforms.
func (d *DockerEnvironment) UploadFiles() error {
	// 1. Write pattern-defined files (TOML [[files]])
	for _, f := range d.files {
		filePath := filepath.Join(d.dataPath, f.Path)
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			return fmt.Errorf("docker: mkdir for %s: %w", f.Path, err)
		}
		if err := os.WriteFile(filePath, []byte(f.Content), 0o644); err != nil {
			return fmt.Errorf("docker: write %s: %w", f.Path, err)
		}
	}

	// 2. Apply egg config-file transformations (server.properties, etc.)
	if err := parser.ApplyAllConfigs(d.egg.Config.Files, d.envVars, d.dataPath); err != nil {
		return fmt.Errorf("docker: apply egg config files: %w", err)
	}
	return nil
}

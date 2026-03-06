package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"egg-emulator/internal/environment"
	"egg-emulator/internal/proxy"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
)

// ────────────────────────────────────────────────────────────────────────────
// Installation
// ────────────────────────────────────────────────────────────────────────────

const installerMarker = ".installed"
const defaultInstallerImage = "ghcr.io/ashu11-a/installers:alpine"

// Install runs the egg's installation script inside a temporary container.
// If a .installed marker file exists in the data directory, installation is skipped.
// Output is streamed to writer (may be nil to discard).
func (d *DockerEnvironment) Install(ctx context.Context, writer io.Writer) error {
	markerPath := filepath.Join(d.dataPath, installerMarker)
	if _, err := os.Stat(markerPath); err == nil {
		logf(writer, "Installation already complete, skipping.\n")
		return nil
	}

	installerImg := d.egg.Scripts.Installation.Container
	if installerImg == "" {
		installerImg = defaultInstallerImage
	}

	logf(writer, "Pulling installer image: %s\n", installerImg)
	pullReader, err := d.client.ImagePull(ctx, installerImg, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("docker: pull installer %s: %w", installerImg, err)
	}
	io.Copy(io.Discard, pullReader) //nolint:errcheck
	pullReader.Close()

	script := d.egg.Scripts.Installation.Script

	shell := d.egg.Scripts.Installation.Entrypoint
	if shell == "" {
		if strings.Contains(installerImg, "alpine") {
			shell = "/bin/ash"
		} else {
			shell = "/bin/bash"
		}
	}

	installScriptPath := filepath.Join(d.logsDir, d.egg.Name+".install.sh")
	if d.logsDir != "" {
		if err := os.WriteFile(installScriptPath, []byte(script), 0644); err != nil {
			logf(writer, "Failed to write install script to %s: %v\n", installScriptPath, err)
		}
	}

	installEnv := append([]string{}, d.envVars...)
	installEnv = append(installEnv,
		"HTTP_PROXY=http://host.docker.internal:8080",
		"HTTPS_PROXY=http://host.docker.internal:8080",
		"http_proxy=http://host.docker.internal:8080",
		"https_proxy=http://host.docker.internal:8080",
		"NO_PROXY=localhost,127.0.0.1",
	)
	installMounts := []mount.Mount{
		{Type: mount.TypeBind, Source: d.dataPath, Target: "/mnt/server"},
	}
	if d.logsDir != "" {
		if absLogs, err := filepath.Abs(d.logsDir); err == nil {
			installEnv = append(installEnv,
				"CURL_CA_BUNDLE=/tmp/egg-proxy-ca/proxy-ca.pem",
				"SSL_CERT_FILE=/tmp/egg-proxy-ca/proxy-ca.pem",
			)
			installMounts = append(installMounts, mount.Mount{
				Type:   mount.TypeBind,
				Source: absLogs,
				Target: "/tmp/egg-proxy-ca",
			})
		}
	}

	conf := &container.Config{
		Image:        installerImg,
		Cmd:          []string{shell, "-c", script},
		Env:          installEnv,
		WorkingDir:   "/mnt/server",
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
	}
	hostConf := &container.HostConfig{
		AutoRemove: true,
		Mounts:     installMounts,
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
	}

	resp, err := d.client.ContainerCreate(ctx, conf, hostConf, nil, nil, "")
	if err != nil {
		return fmt.Errorf("docker: create installer container: %w", err)
	}

	// Attach before start to guarantee no output is missed
	hijack, err := d.client.ContainerAttach(ctx, resp.ID, container.AttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return fmt.Errorf("docker: attach installer: %w", err)
	}
	defer hijack.Close()

	if err := d.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("docker: start installer: %w", err)
	}

	// Register IP for proxy logging (after Start so IP is assigned)
	if inspect, err := d.client.ContainerInspect(ctx, resp.ID); err == nil {
		ip := inspect.NetworkSettings.IPAddress
		if ip == "" && len(inspect.NetworkSettings.Networks) > 0 {
			for _, net := range inspect.NetworkSettings.Networks {
				if net.IPAddress != "" {
					ip = net.IPAddress
					break
				}
			}
		}
		if ip != "" {
			proxy.Register(ip, d.egg.Name+" (Installer)")
		}
	}

	if writer != nil {
		go io.Copy(writer, hijack.Reader) //nolint:errcheck
	} else {
		go io.Copy(io.Discard, hijack.Reader) //nolint:errcheck
	}

	statusCh, errCh := d.client.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		return fmt.Errorf("docker: installer wait: %w", err)
	case status := <-statusCh:
		if status.StatusCode != 0 {
			return fmt.Errorf("docker: installation failed (exit %d)", status.StatusCode)
		}
	}

	logf(writer, "Installation complete.\n")
	return os.WriteFile(markerPath, []byte("installed"), 0o644)
}

// ────────────────────────────────────────────────────────────────────────────
// Lifecycle: Start / Stop / Terminate / State
// ────────────────────────────────────────────────────────────────────────────

// Start attaches to the container streams then starts it.
func (d *DockerEnvironment) Start(ctx context.Context) error {
	if d.containerID == "" {
		return fmt.Errorf("docker: container not created")
	}

	// Register IP for proxy logging
	if inspect, err := d.client.ContainerInspect(ctx, d.containerID); err == nil {
		if inspect.NetworkSettings != nil && inspect.NetworkSettings.IPAddress != "" {
			proxy.Register(inspect.NetworkSettings.IPAddress, d.egg.Name)
		}
	}

	hijack, err := d.client.ContainerAttach(ctx, d.containerID, container.AttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return fmt.Errorf("docker: attach container: %w", err)
	}
	d.stream = &hijack

	if err := d.client.ContainerStart(ctx, d.containerID, container.StartOptions{}); err != nil {
		d.stream.Close()
		d.stream = nil
		return fmt.Errorf("docker: start container: %w", err)
	}
	return nil
}

// Stop gracefully stops the container and closes the stream.
func (d *DockerEnvironment) Stop(ctx context.Context) error {
	d.closeStream()
	return d.client.ContainerStop(ctx, d.containerID, container.StopOptions{})
}

// Terminate forcibly removes the container.
func (d *DockerEnvironment) Terminate(ctx context.Context) error {
	d.closeStream()
	return d.client.ContainerRemove(ctx, d.containerID, container.RemoveOptions{Force: true})
}

// State returns the current lifecycle state by inspecting the container.
func (d *DockerEnvironment) State() environment.State {
	if d.containerID == "" {
		return environment.StateOffline
	}
	inspect, err := d.client.ContainerInspect(context.Background(), d.containerID)
	if err != nil {
		return environment.StateOffline
	}
	switch inspect.State.Status {
	case "running":
		return environment.StateRunning
	case "restarting":
		return environment.StateStarting
	default:
		return environment.StateOffline
	}
}

// ────────────────────────────────────────────────────────────────────────────
// Monitoring: ReadLog / SendCommand / WaitForStop / Stats
// ────────────────────────────────────────────────────────────────────────────

// ReadLog streams the container's output to writer until ctx is cancelled.
func (d *DockerEnvironment) ReadLog(ctx context.Context, writer io.Writer) error {
	if d.stream != nil {
		_, err := io.Copy(writer, d.stream.Reader)
		return err
	}
	// Fallback: ContainerLogs (e.g. if Start was not called yet)
	out, err := d.client.ContainerLogs(ctx, d.containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		return fmt.Errorf("docker: container logs: %w", err)
	}
	defer out.Close()
	_, err = io.Copy(writer, out)
	return err
}

// SendCommand writes a line to the container's stdin.
func (d *DockerEnvironment) SendCommand(command string) error {
	if d.stream == nil {
		return fmt.Errorf("docker: container stream not available")
	}
	_, err := fmt.Fprintln(d.stream.Conn, command)
	return err
}

// WaitForStop blocks until the container exits and returns the exit code.
func (d *DockerEnvironment) WaitForStop(ctx context.Context) (int, error) {
	statusCh, errCh := d.client.ContainerWait(ctx, d.containerID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		return -1, fmt.Errorf("docker: wait: %w", err)
	case status := <-statusCh:
		return int(status.StatusCode), nil
	}
}

// Stats returns the current CPU and memory usage.
func (d *DockerEnvironment) Stats(ctx context.Context) (environment.Stats, error) {
	if d.containerID == "" {
		return environment.Stats{}, fmt.Errorf("docker: container not created")
	}
	resp, err := d.client.ContainerStats(ctx, d.containerID, false)
	if err != nil {
		return environment.Stats{}, fmt.Errorf("docker: stats: %w", err)
	}
	defer resp.Body.Close()

	var v container.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return environment.Stats{}, fmt.Errorf("docker: decode stats: %w", err)
	}

	var cpuPercent float64
	cpuDelta := float64(v.CPUStats.CPUUsage.TotalUsage) - float64(v.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(v.CPUStats.SystemUsage) - float64(v.PreCPUStats.SystemUsage)
	if sysDelta > 0 && cpuDelta > 0 {
		cpuPercent = (cpuDelta / sysDelta) * float64(len(v.CPUStats.CPUUsage.PercpuUsage)) * 100
	}

	return environment.Stats{
		CPUUsage:    cpuPercent,
		MemoryUsage: float64(v.MemoryStats.Usage),
		MemoryLimit: float64(v.MemoryStats.Limit),
	}, nil
}

// ────────────────────────────────────────────────────────────────────────────
// Helpers
// ────────────────────────────────────────────────────────────────────────────

func (d *DockerEnvironment) closeStream() {
	if d.stream != nil {
		d.stream.Close()
		d.stream = nil
	}
}

func logf(w io.Writer, format string, args ...interface{}) {
	if w != nil {
		fmt.Fprintf(w, format, args...)
	}
}

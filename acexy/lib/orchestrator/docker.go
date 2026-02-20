package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
)

// randomHex generates a random hexadecimal string of n bytes.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

const (
	aceStreamVolumeBind   = "/opt/docker/volumes/localstreams/acestream:/home/localstreams/.ACEStream"
	defaultRegularNetwork = "bridge"
	vpnContainer          = "gluetun"
)

// containerRemoveOptions returns the standard options for removing containers.
func containerRemoveOptions() container.RemoveOptions {
	return container.RemoveOptions{
		Force:         true,
		RemoveVolumes: false, // keep the AceStream cache volume
	}
}

// createContainer creates and starts an AceStream container according to the configured profile.
// Communication is container-to-container on the shared Docker network (internal port 6878).
// Returns (containerID, containerName, host, error).
func (o *Orchestrator) createContainer(ctx context.Context) (string, string, string, error) {
	hash := randomHex(6)
	containerName := fmt.Sprintf("acestream-%s", hash)

	// Compose labels so the container belongs to the same stack
	labels := map[string]string{
		"com.docker.compose.project":             o.ComposeProject,
		"com.docker.compose.project.working_dir": o.ComposeWorkingDir,
		"com.docker.compose.service":             containerName,
		"com.docker.compose.oneoff":              "False",
	}

	cfg := &container.Config{
		Image:  o.image,
		Labels: labels,
	}

	hostCfg := &container.HostConfig{
		Binds:         []string{aceStreamVolumeBind},
		RestartPolicy: container.RestartPolicy{Name: "no"},
	}

	netCfg := &network.NetworkingConfig{}

	if o.profile == "vpn" {
		// In VPN mode, share the network namespace of the gluetun container
		hostCfg.NetworkMode = container.NetworkMode(fmt.Sprintf("container:%s", vpnContainer))
	}

	if err := o.pullImageIfNeeded(ctx); err != nil {
		return "", "", "", fmt.Errorf("failed to pull image: %w", err)
	}

	slog.Debug("Creating container", "name", containerName, "image", o.image, "profile", o.profile)

	resp, err := o.dockerClient.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, containerName)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to create container: %w", err)
	}

	if err := o.dockerClient.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = o.dockerClient.ContainerRemove(ctx, resp.ID, containerRemoveOptions())
		return "", "", "", fmt.Errorf("failed to start container: %w", err)
	}

	// In regular mode, explicitly connect to the network after the container starts
	if o.profile != "vpn" {
		net := o.ContainerNetwork
		if net == "" {
			net = defaultRegularNetwork
		}
		if err := o.dockerClient.NetworkConnect(ctx, net, resp.ID, &network.EndpointSettings{}); err != nil {
			_ = o.dockerClient.ContainerRemove(ctx, resp.ID, containerRemoveOptions())
			return "", "", "", fmt.Errorf("failed to connect container to network %s: %w", net, err)
		}
		slog.Debug("Container connected to network", "network", net, "containerID", resp.ID[:12])
	}

	// Get the container's IP on the correct network
	host, err := o.getContainerHost(ctx, resp.ID)
	if err != nil {
		_ = o.dockerClient.ContainerRemove(ctx, resp.ID, containerRemoveOptions())
		return "", "", "", fmt.Errorf("failed to get container host: %w", err)
	}

	slog.Info("Container created and started", "name", containerName, "id", resp.ID[:12], "host", host)
	return resp.ID, containerName, host, nil
}

// containerExists checks whether a container with the given ID exists in Docker.
func (o *Orchestrator) containerExists(ctx context.Context, containerID string) bool {
	f := filters.NewArgs()
	f.Add("id", containerID)
	containers, err := o.dockerClient.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: f,
	})
	if err != nil {
		return false
	}
	return len(containers) > 0
}

// getContainerHost returns the host to connect to in order to reach the container.
// In VPN mode it returns "localhost" (shared network with gluetun).
// In regular mode it returns the container IP on the configured network.
func (o *Orchestrator) getContainerHost(ctx context.Context, containerID string) (string, error) {
	if o.profile == "vpn" {
		return "localhost", nil
	}

	inspect, err := o.dockerClient.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("failed to inspect container: %w", err)
	}

	net := o.ContainerNetwork
	if net == "" {
		net = defaultRegularNetwork
	}
	if netSettings, ok := inspect.NetworkSettings.Networks[net]; ok {
		if netSettings.IPAddress != "" {
			return netSettings.IPAddress, nil
		}
	}

	// Fallback: use the default bridge IP
	if inspect.NetworkSettings.IPAddress != "" {
		return inspect.NetworkSettings.IPAddress, nil
	}

	return "", fmt.Errorf("could not determine IP for container %s", containerID[:12])
}

// pullImageIfNeeded checks whether the image is available locally and pulls it if not.
func (o *Orchestrator) pullImageIfNeeded(ctx context.Context) error {
	images, err := o.dockerClient.ImageList(ctx, image.ListOptions{})
	if err != nil {
		slog.Debug("Could not list images, attempting pull anyway", "error", err)
		return o.pullImage()
	}

	for _, img := range images {
		for _, tag := range img.RepoTags {
			if tag == o.image {
				slog.Debug("Image already present locally", "image", o.image)
				return nil
			}
		}
	}

	slog.Info("Image not found locally, pulling", "image", o.image)
	return o.pullImage()
}

// pullImage pulls the image with a 10-minute timeout.
func (o *Orchestrator) pullImage() error {
	pullCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	slog.Info("Pulling image", "image", o.image)
	reader, err := o.dockerClient.ImagePull(pullCtx, o.image, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("image pull failed: %w", err)
	}
	defer reader.Close()
	_, _ = io.Copy(io.Discard, reader)
	slog.Info("Image pulled successfully", "image", o.image)
	return nil
}

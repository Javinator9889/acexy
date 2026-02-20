package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/docker/docker/client"
)

type HealthStatus int

const (
	Healthy   HealthStatus = iota
	Degraded
	Unhealthy
	Dead
)

// AceStreamInstance represents an AceStream container instance in the pool.
type AceStreamInstance struct {
	ContainerID        string
	Name               string // container name, e.g. acestream-2968d06067f1
	Host               string
	Port               int
	Health             HealthStatus
	LastCheck          time.Time
	FailureCount       int  // consecutive container health check failures
	StreamFailureCount int  // consecutive times all active streams were stalled simultaneously
	ActiveStreams       int
	CreatedAt          time.Time
	LastActivity       time.Time
}

// Orchestrator manages the pool of AceStream instances.
type Orchestrator struct {
	instances          map[string]*AceStreamInstance
	mutex              *sync.RWMutex
	dockerClient       *client.Client
	minReplicas        int
	maxReplicas        int
	streamsPerInstance int
	idleTimeout        time.Duration
	profile              string    // "regular" or "vpn"
	image                string    // Docker image to use
	lastPoolActivity     time.Time // last time any stream was active across the whole pool
	recycled             bool      // true after a recycle, reset only when a new stream arrives
	recycleCheckInterval time.Duration // how often recycleIfIdle is checked
	scaleDownInterval    time.Duration // how often scaleDownIdle is checked

	// Exported for access from acexy.go
	MinReplicas               int
	MaxReplicas               int
	StreamsPerInstance        int
	IdleTimeout               time.Duration
	RecycleTimeout            time.Duration // idle time before recycling the entire pool
	RecycleCheckInterval      time.Duration // how often the recycle check runs (default 3s)
	ScaleDownInterval         time.Duration // how often the scale down check runs (default 30s)
	Profile                   string
	Image                     string
	DockerHost                string
	ComposeProject            string // value of com.docker.compose.project
	ComposeWorkingDir         string // value of com.docker.compose.project.working_dir
	ContainerNetwork          string // Docker network acexy is connected to; used to attach AceStream instances
	ContainerFailureThreshold int    // consecutive health check failures before marking Unhealthy
	StreamFailureThreshold    int    // consecutive times all streams stall before marking Unhealthy
}

// Init initializes the Orchestrator, connects to Docker and starts minReplicas instances.
func (o *Orchestrator) Init() error {
	// Copy exported fields to internal ones
	o.minReplicas = o.MinReplicas
	o.maxReplicas = o.MaxReplicas
	o.streamsPerInstance = o.StreamsPerInstance
	o.idleTimeout = o.IdleTimeout
	o.profile = o.Profile
	o.image = o.Image

	o.lastPoolActivity = time.Now()

	o.recycleCheckInterval = o.RecycleCheckInterval
	if o.recycleCheckInterval <= 0 {
		o.recycleCheckInterval = 3 * time.Second
	}
	o.scaleDownInterval = o.ScaleDownInterval
	if o.scaleDownInterval <= 0 {
		o.scaleDownInterval = 30 * time.Second
	}

	// Apply defaults for thresholds if not configured
	if o.ContainerFailureThreshold <= 0 {
		o.ContainerFailureThreshold = 3
	}
	if o.StreamFailureThreshold <= 0 {
		o.StreamFailureThreshold = 3
	}

	o.instances = make(map[string]*AceStreamInstance)
	o.mutex = &sync.RWMutex{}
	// Copiar campos exportados adicionales
	// Connect to Docker via socket proxy
	dockerHost := o.DockerHost
	if dockerHost == "" {
		dockerHost = "tcp://docker-proxy:2375"
	}

	var err error
	o.dockerClient, err = client.NewClientWithOpts(
		client.WithHost(dockerHost),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return fmt.Errorf("failed to create docker client: %w", err)
	}

	// Verify Docker connectivity
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := o.dockerClient.Ping(ctx); err != nil {
		return fmt.Errorf("failed to connect to docker: %w", err)
	}

	slog.Info("Orchestrator initialized", "minReplicas", o.minReplicas, "maxReplicas", o.maxReplicas,
		"streamsPerInstance", o.streamsPerInstance, "profile", o.profile, "image", o.image)

	// Start initial instances
	for i := 0; i < o.minReplicas; i++ {
		slog.Info("Scaling up initial instance", "index", i+1, "of", o.minReplicas)
		if _, err := o.ScaleUp(); err != nil {
			return fmt.Errorf("failed to scale up initial instance %d: %w", i+1, err)
		}
	}

	// Reset lastPoolActivity after all initial instances are ready so the recycle
	// timeout starts counting from when the pool is actually available, not from
	// when Init() was called.
	// Mark as recycled so the pool is not recycled until the first real stream arrives.
	o.mutex.Lock()
	o.lastPoolActivity = time.Now()
	o.recycled = true
	o.mutex.Unlock()

	return nil
}

// TotalInstances returns the number of active instances in the pool.
func (o *Orchestrator) TotalInstances() int {
	o.mutex.RLock()
	defer o.mutex.RUnlock()
	return len(o.instances)
}

// IsRecycling returns true if the pool has been recycled and is waiting for a new stream.
func (o *Orchestrator) IsRecycling() bool {
	o.mutex.RLock()
	defer o.mutex.RUnlock()
	return o.recycled
}

// WaitForInstance blocks until a healthy instance is available or the timeout expires.
// Used to avoid creating spurious instances during a pool recycle.
func (o *Orchestrator) WaitForInstance(timeout time.Duration) *AceStreamInstance {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if instance := o.SelectInstance(); instance != nil {
			return instance
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}

// SelectInstance picks the best available instance:
// - Health == Healthy
// - ActiveStreams < streamsPerInstance
// - Prefers the instance with the most active streams (bin-packing strategy)
// Returns nil if no instance is available.
func (o *Orchestrator) SelectInstance() *AceStreamInstance {
	o.mutex.RLock()
	defer o.mutex.RUnlock()

	var best *AceStreamInstance
	for _, inst := range o.instances {
		if inst.Health != Healthy {
			continue
		}
		if inst.ActiveStreams >= o.streamsPerInstance {
			continue
		}
		if best == nil || inst.ActiveStreams > best.ActiveStreams {
			best = inst
		}
	}
	return best
}

// ScaleUp creates a new AceStream container, waits for it to become healthy,
// adds it to the pool and returns it.
func (o *Orchestrator) ScaleUp() (*AceStreamInstance, error) {
	ctx := context.Background()

	slog.Info("Scaling up new AceStream instance", "profile", o.profile, "image", o.image)

	containerID, containerName, host, err := o.createContainer(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create container: %w", err)
	}

	// Container-to-container communication: always use internal port 6878
	const aceStreamPort = 6878

	instance := &AceStreamInstance{
		ContainerID:  containerID,
		Name:         containerName,
		Host:         host,
		Port:         aceStreamPort,
		Health:       Unhealthy,
		CreatedAt:    time.Now(),
		LastActivity: time.Now(),
	}

	slog.Info("Waiting for instance to be healthy", "name", containerName, "host", host, "port", aceStreamPort)
	if err := o.waitForHealthy(instance); err != nil {
		// If it never starts, clean up the container
		_ = o.removeContainer(ctx, containerID)
		return nil, fmt.Errorf("instance never became healthy: %w", err)
	}

	instance.Health = Healthy
	instance.LastCheck = time.Now()

	o.mutex.Lock()
	o.instances[containerID] = instance
	o.mutex.Unlock()

	slog.Info("New instance ready", "name", containerName, "host", host, "port", aceStreamPort)
	return instance, nil
}

// waitForHealthy polls /webui/api/service?method=get_version
// until it returns 200 or the timeout expires (2 minutes, polling every 5s).
func (o *Orchestrator) waitForHealthy(instance *AceStreamInstance) error {
	timeout := 2 * time.Minute
	interval := 5 * time.Second
	deadline := time.Now().Add(timeout)

	url := fmt.Sprintf("http://%s:%d/webui/api/service?method=get_version", instance.Host, instance.Port)
	httpClient := &http.Client{Timeout: 3 * time.Second}

	for time.Now().Before(deadline) {
		resp, err := httpClient.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		slog.Debug("Instance not ready yet, retrying...", "containerID", instance.ContainerID, "url", url)
		time.Sleep(interval)
	}

	return fmt.Errorf("timeout waiting for instance %s to become healthy", instance.ContainerID)
}

// removeContainer removes a container (used for cleanup after an error).
func (o *Orchestrator) removeContainer(ctx context.Context, containerID string) error {
	return o.dockerClient.ContainerRemove(ctx, containerID, containerRemoveOptions())
}

// TouchPoolActivity updates the last activity timestamp of the pool and resets the recycled flag.
// Must be called from acexy.go whenever a new stream is assigned to any instance.
func (o *Orchestrator) TouchPoolActivity() {
	o.mutex.Lock()
	o.lastPoolActivity = time.Now()
	o.recycled = false
	o.mutex.Unlock()
}

// totalActiveStreams returns the sum of active streams across all instances.
// The caller must hold at least a read lock.
func (o *Orchestrator) totalActiveStreams() int {
	total := 0
	for _, inst := range o.instances {
		total += inst.ActiveStreams
	}
	return total
}

// ScaleDownLoop periodically checks for idle instances and recycles the pool when appropriate.
// Two independent tickers are used:
//   - Every 3s: recycle check (in-memory only, cheap)
//   - Every 30s: scale down check (may call Docker API to remove containers)
//
// It must be run in a separate goroutine.
func (o *Orchestrator) ScaleDownLoop() {
	recycleTicker := time.NewTicker(o.recycleCheckInterval)
	scaleDownTicker := time.NewTicker(o.scaleDownInterval)
	defer recycleTicker.Stop()
	defer scaleDownTicker.Stop()

	for {
		select {
		case <-recycleTicker.C:
			o.recycleIfIdle()
		case <-scaleDownTicker.C:
			o.scaleDownIdle()
		}
	}
}

// scaleDownIdle removes instances that have been idle longer than IdleTimeout,
// as long as the pool stays above minReplicas.
func (o *Orchestrator) scaleDownIdle() {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	for id, instance := range o.instances {
		if instance.ActiveStreams > 0 {
			continue
		}
		if time.Since(instance.LastActivity) <= o.idleTimeout {
			continue
		}
		if len(o.instances) <= o.minReplicas {
			break
		}
		slog.Info("Scaling down idle instance", "name", instance.Name,
			"idleSince", instance.LastActivity)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := o.dockerClient.ContainerRemove(ctx, id, containerRemoveOptions()); err != nil {
			slog.Warn("Failed to remove idle instance", "containerID", id[:12], "error", err)
		} else {
			delete(o.instances, id)
		}
		cancel()
	}
}

// recycleIfIdle replaces the entire pool with fresh instances when the pool has had
// no active streams for longer than RecycleTimeout. This prevents AceStream state
// (cache, stale connections) from accumulating over time.
// Once recycled, the pool will not recycle again until a new stream arrives (recycled flag).
func (o *Orchestrator) recycleIfIdle() {
	if o.RecycleTimeout <= 0 {
		return
	}

	o.mutex.RLock()
	activeStreams := o.totalActiveStreams()
	idleSince := o.lastPoolActivity
	alreadyRecycled := o.recycled
	o.mutex.RUnlock()

	if alreadyRecycled {
		return
	}
	if activeStreams > 0 {
		return
	}
	if time.Since(idleSince) <= o.RecycleTimeout {
		return
	}

	slog.Info("Pool idle, recycling all instances",
		"idleSince", idleSince,
		"recycleTimeout", o.RecycleTimeout,
	)

	// Remove all current instances
	o.mutex.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for id, inst := range o.instances {
		slog.Info("Recycling instance", "name", inst.Name)
		if err := o.dockerClient.ContainerRemove(ctx, id, containerRemoveOptions()); err != nil {
			slog.Warn("Failed to remove instance during recycle", "name", inst.Name, "error", err)
		}
		delete(o.instances, id)
	}
	// Reset lastPoolActivity before unlocking so that the recycle check does not
	// fire again immediately while ScaleUp is still running.
	o.lastPoolActivity = time.Now()
	o.mutex.Unlock()

	// Start fresh minReplicas instances
	for i := 0; i < o.minReplicas; i++ {
		if _, err := o.ScaleUp(); err != nil {
			slog.Error("Failed to start replacement instance after recycle", "index", i+1, "error", err)
		}
	}

	// Mark as recycled so the pool is not recycled again until a new stream arrives.
	o.mutex.Lock()
	o.recycled = true
	o.lastPoolActivity = time.Now()
	o.mutex.Unlock()
}

// Shutdown removes all containers in the pool in an orderly fashion.
func (o *Orchestrator) Shutdown() {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	slog.Info("Shutting down orchestrator, removing all instances", "count", len(o.instances))
	for id, instance := range o.instances {
		slog.Info("Removing instance", "name", instance.Name, "host", instance.Host)
		if err := o.dockerClient.ContainerRemove(ctx, id, containerRemoveOptions()); err != nil {
			slog.Warn("Failed to remove instance", "containerID", id[:12], "error", err)
		}
		delete(o.instances, id)
	}
	slog.Info("Orchestrator shutdown complete")
}

package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// MonitorLoop periodically checks the health of all instances in the pool.
// It must be run in a separate goroutine.
//
// For each instance it checks two independent conditions:
//  1. The container responds to the HTTP health check (checkContainerHealth)
//  2. The active streams on the instance are receiving data (checkStreamHealth)
//
// If either condition exceeds its failure threshold, the instance is marked
// Unhealthy and replaced via killAndReplace.
func (o *Orchestrator) MonitorLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		o.mutex.RLock()
		instances := make([]*AceStreamInstance, 0, len(o.instances))
		for _, inst := range o.instances {
			instances = append(instances, inst)
		}
		o.mutex.RUnlock()

		for _, inst := range instances {
			o.monitorInstance(inst)
		}
	}
}

// monitorInstance evaluates the health of a single instance and acts if it is unhealthy.
func (o *Orchestrator) monitorInstance(inst *AceStreamInstance) {
	containerHealthy := o.checkContainerHealth(inst)
	streamsHealthy := o.checkStreamHealth(inst)

	if !containerHealthy || !streamsHealthy {
		if inst.Health == Unhealthy {
			slog.Warn("Instance unhealthy, replacing",
				"name", inst.Name,
				"containerHealthy", containerHealthy,
				"streamsHealthy", streamsHealthy,
			)
			o.killAndReplace(inst)
		}
	}
}

// checkContainerHealth checks whether the container responds to the AceStream version endpoint.
// It increments FailureCount on each failure and resets it on success.
// Marks the instance as Unhealthy when FailureCount >= ContainerFailureThreshold.
func (o *Orchestrator) checkContainerHealth(inst *AceStreamInstance) bool {
	url := fmt.Sprintf("http://%s:%d/webui/api/service?method=get_version", inst.Host, inst.Port)
	httpClient := &http.Client{Timeout: 3 * time.Second}

	resp, err := httpClient.Get(url)
	if err == nil && resp.StatusCode == http.StatusOK {
		resp.Body.Close()
		inst.FailureCount = 0
		if inst.Health == Degraded {
			slog.Info("Instance recovered", "name", inst.Name)
			inst.Health = Healthy
		}
		inst.LastCheck = time.Now()
		return true
	}
	if resp != nil {
		resp.Body.Close()
	}

	inst.FailureCount++
	inst.LastCheck = time.Now()
	slog.Warn("Container health check failed",
		"name", inst.Name,
		"failureCount", inst.FailureCount,
		"threshold", o.ContainerFailureThreshold,
	)

	if inst.FailureCount >= o.ContainerFailureThreshold {
		slog.Warn("Instance marked unhealthy due to container failures", "name", inst.Name)
		inst.Health = Unhealthy
		return false
	}

	inst.Health = Degraded
	return true // Degraded but not yet Unhealthy
}

// checkStreamHealth evaluates whether the active streams on the instance are failing.
// It does not access the Copier directly: acexy.go notifies via MarkStreamStalled
// and ResetStreamFailures. This method only reads StreamFailureCount.
func (o *Orchestrator) checkStreamHealth(inst *AceStreamInstance) bool {
	if inst.ActiveStreams == 0 {
		return true
	}

	if inst.StreamFailureCount >= o.StreamFailureThreshold {
		slog.Warn("Instance marked unhealthy due to stream failures",
			"name", inst.Name,
			"streamFailureCount", inst.StreamFailureCount,
			"threshold", o.StreamFailureThreshold,
		)
		inst.Health = Unhealthy
		return false
	}

	return true
}

// MarkStreamStalled notifies the orchestrator that a stream on an instance has stalled.
// It only increments the counter when ActiveStreams > 0, distinguishing a stream that
// failed during playback (instance problem) from an invalid ID that never started.
func (o *Orchestrator) MarkStreamStalled(inst *AceStreamInstance) {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	if inst.ActiveStreams == 0 {
		// The stream never started correctly — invalid ID or earlier error
		return
	}

	inst.StreamFailureCount++
	slog.Debug("Stream stall counted for instance",
		"name", inst.Name,
		"streamFailureCount", inst.StreamFailureCount,
		"threshold", o.StreamFailureThreshold,
	)
}

// ResetStreamFailures resets the stream failure counter for an instance.
// Should be called when a stream reconnects successfully, indicating the instance
// is working correctly again.
func (o *Orchestrator) ResetStreamFailures(inst *AceStreamInstance) {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	if inst.StreamFailureCount > 0 {
		slog.Debug("Resetting stream failure count for instance", "name", inst.Name)
		inst.StreamFailureCount = 0
	}
}

// killAndReplace removes an unhealthy instance from the pool and creates a replacement
// if needed to maintain minReplicas.
func (o *Orchestrator) killAndReplace(inst *AceStreamInstance) {
	o.mutex.Lock()
	delete(o.instances, inst.ContainerID)
	remaining := len(o.instances)
	o.mutex.Unlock()

	slog.Info("Killing unhealthy instance", "name", inst.Name, "remaining", remaining)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := o.dockerClient.ContainerRemove(ctx, inst.ContainerID, containerRemoveOptions()); err != nil {
		slog.Warn("Failed to remove unhealthy instance", "name", inst.Name, "error", err)
	}

	if remaining < o.minReplicas {
		slog.Info("Replacing killed instance to maintain minReplicas", "minReplicas", o.minReplicas)
		if _, err := o.ScaleUp(); err != nil {
			slog.Error("Failed to replace killed instance", "error", err)
		}
	}
}

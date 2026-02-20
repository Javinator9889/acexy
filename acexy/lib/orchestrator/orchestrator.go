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

// AceStreamInstance representa una instancia de contenedor AceStream en el pool
type AceStreamInstance struct {
	ContainerID        string
	Name               string // nombre del contenedor, ej: acestream-2968d06067f1
	Host               string
	Port               int
	Health             HealthStatus
	LastCheck          time.Time
	FailureCount       int  // fallos consecutivos del health check del contenedor
	StreamFailureCount int  // veces consecutivas que todos los streams activos estaban colgados
	ActiveStreams       int
	CreatedAt          time.Time
	LastActivity       time.Time
}

// Orchestrator gestiona el pool de instancias AceStream
type Orchestrator struct {
	instances          map[string]*AceStreamInstance
	mutex              *sync.RWMutex
	dockerClient       *client.Client
	minReplicas        int
	maxReplicas        int
	streamsPerInstance int
	idleTimeout        time.Duration
	profile            string // "regular" o "vpn"
	image              string // imagen Docker a usar

	// Exportados para acceso desde acexy.go
	MinReplicas               int
	MaxReplicas               int
	StreamsPerInstance        int
	IdleTimeout               time.Duration
	Profile                   string
	Image                     string
	DockerHost                string
	ComposeProject            string // valor de com.docker.compose.project
	ComposeWorkingDir         string // valor de com.docker.compose.project.working_dir
	ContainerFailureThreshold int    // fallos consecutivos del health check antes de marcar Unhealthy
	StreamFailureThreshold    int    // veces que todos los streams fallan antes de marcar Unhealthy
}

// Init inicializa el Orchestrator, conecta con Docker y levanta minReplicas instancias
func (o *Orchestrator) Init() error {
	// Copiar campos exportados a los internos
	o.minReplicas = o.MinReplicas
	o.maxReplicas = o.MaxReplicas
	o.streamsPerInstance = o.StreamsPerInstance
	o.idleTimeout = o.IdleTimeout
	o.profile = o.Profile
	o.image = o.Image

	// Defaults para umbrales si no se han configurado
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

	// Verificar conectividad con Docker
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := o.dockerClient.Ping(ctx); err != nil {
		return fmt.Errorf("failed to connect to docker: %w", err)
	}

	slog.Info("Orchestrator initialized", "minReplicas", o.minReplicas, "maxReplicas", o.maxReplicas,
		"streamsPerInstance", o.streamsPerInstance, "profile", o.profile, "image", o.image)

	// Levantar instancias iniciales
	for i := 0; i < o.minReplicas; i++ {
		slog.Info("Scaling up initial instance", "index", i+1, "of", o.minReplicas)
		if _, err := o.ScaleUp(); err != nil {
			return fmt.Errorf("failed to scale up initial instance %d: %w", i+1, err)
		}
	}

	return nil
}

// TotalInstances returns the number of active instances in the pool.
func (o *Orchestrator) TotalInstances() int {
	o.mutex.RLock()
	defer o.mutex.RUnlock()
	return len(o.instances)
}

// SelectInstance elige la mejor instancia disponible:
// - Health == Healthy
// - ActiveStreams < streamsPerInstance
// - Prefiere la instancia con menos streams activos
// Devuelve nil si no hay ninguna disponible
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
		if best == nil || inst.ActiveStreams < best.ActiveStreams {
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
		// Si no arranca limpiamos el contenedor
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

// waitForHealthy hace polling a /webui/api/service?method=get_version
// hasta que responde 200 o se agota el timeout (2 minutos, polling cada 5s)
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

// removeContainer elimina un contenedor (usado en cleanup tras error)
func (o *Orchestrator) removeContainer(ctx context.Context, containerID string) error {
	return o.dockerClient.ContainerRemove(ctx, containerID, containerRemoveOptions())
}

// ScaleDownLoop periodically checks for idle instances and removes them when appropriate.
// It must be run in a separate goroutine.
func (o *Orchestrator) ScaleDownLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		o.mutex.Lock()
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
		o.mutex.Unlock()
	}
}

// Shutdown elimina todos los contenedores del pool de forma ordenada
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

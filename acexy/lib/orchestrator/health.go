package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// MonitorLoop comprueba periódicamente el estado de todas las instancias del pool.
// Debe ejecutarse en una goroutine separada.
//
// Para cada instancia comprueba dos cosas independientes:
//  1. El contenedor responde al health check HTTP (checkContainerHealth)
//  2. Los streams activos en la instancia están recibiendo datos (checkStreamHealth)
//
// Si cualquiera de las dos condiciones supera el umbral de fallos, la instancia
// se marca como Unhealthy y se tumba y reemplaza (killAndReplace).
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

// monitorInstance evalúa el estado de una instancia y actúa si está enferma.
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

// checkContainerHealth comprueba si el contenedor responde al endpoint de versión de AceStream.
// Incrementa FailureCount en cada fallo y lo resetea cuando responde correctamente.
// Marca la instancia como Unhealthy si FailureCount >= ContainerFailureThreshold.
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
	return true // Degraded pero no Unhealthy todavía
}

// checkStreamHealth evalúa si los streams activos de la instancia están fallando.
// No accede al Copier directamente: acexy.go notifica mediante MarkStreamStalled
// y ResetStreamFailures. Este método solo consulta StreamFailureCount.
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

// MarkStreamStalled notifica al orquestador que un stream de una instancia se ha colgado.
// Solo incrementa el contador si ActiveStreams > 0, diferenciando así un stream que
// falló durante la reproducción (problema de instancia) de un ID inválido que nunca arrancó.
func (o *Orchestrator) MarkStreamStalled(inst *AceStreamInstance) {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	if inst.ActiveStreams == 0 {
		// El stream nunca arrancó correctamente — ID inválido u otro error previo
		return
	}

	inst.StreamFailureCount++
	slog.Debug("Stream stall counted for instance",
		"name", inst.Name,
		"streamFailureCount", inst.StreamFailureCount,
		"threshold", o.StreamFailureThreshold,
	)
}

// ResetStreamFailures resetea el contador de fallos de streams de una instancia.
// Debe llamarse cuando un stream se reconecta con éxito, indicando que la instancia
// está funcionando correctamente.
func (o *Orchestrator) ResetStreamFailures(inst *AceStreamInstance) {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	if inst.StreamFailureCount > 0 {
		slog.Debug("Resetting stream failure count for instance", "name", inst.Name)
		inst.StreamFailureCount = 0
	}
}

// killAndReplace elimina una instancia enferma del pool y crea una nueva si es necesario
// para mantener minReplicas.
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

// Acexy - Copyright (C) 2024 - Javinator9889 <dev at javinator9889 dot com>
// This program comes with ABSOLUTELY NO WARRANTY; for details type `show w'.
// This is free software, and you are welcome to redistribute it
// under certain conditions; type `show c' for details.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"javinator9889/acexy/lib/acexy"
	"javinator9889/acexy/lib/orchestrator"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	dockerclient "github.com/docker/docker/client"
	"github.com/dustin/go-humanize"
)

var (
	addr              string
	scheme            string
	host              string
	port              int
	streamTimeout     time.Duration
	m3u8              bool
	emptyTimeout      time.Duration
	emptyRetryCount   int
	size              Size
	noResponseTimeout time.Duration

	// Orchestrator config
	minReplicas               int
	maxReplicas               int
	streamsPerInstance        int
	idleTimeout               time.Duration
	recycleTimeout            time.Duration
	recycleCheckInterval      time.Duration
	scaleDownInterval         time.Duration
	composeProfile            string
	acestreamImage            string
	dockerHost                string
	containerFailureThreshold int
	streamFailureThreshold    int
)

//go:embed LICENSE.short
var LICENSE string

// The API URL we are listening to
const APIv1_URL = "/ace"

type Proxy struct {
	Acexy *acexy.Acexy
}

type Size struct {
	Bytes   uint64
	Default uint64
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case APIv1_URL + "/getstream":
		fallthrough
	case APIv1_URL + "/getstream/":
		p.HandleStream(w, r)
	case APIv1_URL + "/status":
		p.HandleStatus(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (p *Proxy) HandleStream(w http.ResponseWriter, r *http.Request) {
	// Verify the request method
	if r.Method != http.MethodGet {
		slog.Error("Method not allowed", "method", r.Method, "path", r.URL.Path)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	// Verify the client has included the ID parameter
	aceId, err := acexy.NewAceID(q.Get("id"), q.Get("infohash"))
	if err != nil {
		slog.Error("ID parameter is required", "path", r.URL.Path, "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Verify the client has not included the PID parameter
	if q.Has("pid") {
		slog.Error("PID parameter is not allowed", "path", r.URL.Path)
		http.Error(w, "PID parameter is not allowed", http.StatusBadRequest)
		return
	}

	// Gather the stream information
	stream, err := p.Acexy.FetchStream(aceId, q)
	if err != nil {
		slog.Error("Failed to start stream", "stream", aceId, "error", err)
		http.Error(w, "Failed to start stream: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// And start playing the stream. The `StartStream` will dump the contents of the new or
	// existing stream to the client. It takes an interface of `io.Writer` to write the stream
	// contents to. The `http.ResponseWriter` implements the `io.Writer` interface, so we can
	// pass it directly.
	slog.Debug("Starting stream", "path", r.URL.Path, "id", aceId)
	if err := p.Acexy.StartStream(stream, w); err != nil {
		slog.Error("Failed to start stream", "stream", aceId, "error", err)
		http.Error(w, "Failed to start stream: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Update the client headers
	w.WriteHeader(http.StatusOK)

	// Defer the stream finish. This will be called when the request is done. When in M3U8 mode,
	// the client connects directly to a subset of endpoints, so we are blind to what the client
	// is doing. However, it periodically polls the M3U8 list to verify nothing has changed,
	// simulating a new connection. Therefore, we can accumulate a lot of open streams and let
	// the timeout finish them.
	//
	// When in MPEG-TS mode, the client connects to the endpoint and waits for the stream to finish.
	// This is a blocking operation, so we can finish the stream when the client disconnects.
	switch p.Acexy.Endpoint {
	case acexy.M3U8_ENDPOINT:
		w.Header().Set("Content-Type", "application/x-mpegURL")
		timedOut := acexy.SetTimeout(streamTimeout)
		defer func() {
			<-timedOut
			p.Acexy.StopStream(stream, w)
		}()
	case acexy.MPEG_TS_ENDPOINT:
		w.Header().Set("Content-Type", "video/MP2T")
		w.Header().Set("Transfer-Encoding", "chunked")
		defer p.Acexy.StopStream(stream, w)
	}

	// And wait for the client to disconnect
	select {
	case <-r.Context().Done():
		slog.Debug("Client disconnected", "path", r.URL.Path)
	case <-p.Acexy.WaitStream(stream):
		slog.Debug("Stream finished", "path", r.URL.Path)
	}
}

func (p *Proxy) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		slog.Error("Method not allowed", "method", r.Method, "path", r.URL.Path)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var aceId *acexy.AceID
	q := r.URL.Query()
	slog.Debug("Status request", "path", r.URL.Path, "query", q)
	id, err := acexy.NewAceID(q.Get("id"), q.Get("infohash"))
	if err == nil {
		aceId = &id
	} else {
		// If no parameter is included, ask for the global status
		aceId = nil
	}

	// Get the status of the stream
	slog.Debug("Getting status", "id", aceId)
	status, err := p.Acexy.GetStatus(aceId)
	if err != nil {
		slog.Error("Failed to get status", "error", err)
		http.Error(w, "Stream not found", http.StatusNotFound)
		return
	}

	slog.Debug("Status", "status", status)
	// Write the status to the client as JSON
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(status); err != nil {
		slog.Error("Failed to write status", "error", err)
		http.Error(w, "Failed to write status", http.StatusInternalServerError)
		return
	}
}

func LookupEnvOrString(key string, def string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return def
}

func LookupEnvOrInt(key string, def int) int {
	if val, ok := os.LookupEnv(key); ok {
		i, err := strconv.Atoi(val)
		if err != nil {
			slog.Error("Failed to parse environment variable", "key", key, "value", val)
			return 0
		}
		return i
	}
	return def
}

func LookupEnvOrDuration(key string, def time.Duration) time.Duration {
	if val, ok := os.LookupEnv(key); ok {
		d, err := time.ParseDuration(val)
		if err != nil {
			slog.Error("Failed to parse environment variable", "key", key, "value", val)
			return 0
		}
		return d
	}
	return def
}

func LookupEnvOrBool(key string, def bool) bool {
	if val, ok := os.LookupEnv(key); ok {
		b, err := strconv.ParseBool(val)
		if err != nil {
			slog.Error("Failed to parse environment variable", "key", key, "value", val)
			return false
		}
		return b
	}
	return def
}

func LookupLogLevel() slog.Level {
	if level, ok := os.LookupEnv("ACEXY_LOG_LEVEL"); ok {
		var sl slog.Level

		if err := sl.UnmarshalText([]byte(level)); err != nil {
			slog.Warn("Failed to parse log level", "level", level)
			return slog.LevelInfo
		}
		return sl
	}
	return slog.LevelInfo
}

func LookupEnvOrSize(key string, def uint64) *Size {
	if val, ok := os.LookupEnv(key); ok {
		if err := size.Set(val); err != nil {
			slog.Error("Failed to parse environment variable", "key", key, "value", val)
			return nil
		}
	} else {
		size.Bytes = def
	}
	return &size
}

func (s *Size) Set(value string) error {
	size, err := humanize.ParseBytes(value)
	if err != nil {
		return err
	}
	s.Bytes = uint64(size)
	return nil
}

// readContainerInfo reads Docker Compose labels and the container network from the current container
// by reading /etc/hostname to obtain the container ID and then inspecting it.
// If it cannot be determined (e.g. running outside Docker), it returns empty strings.
func readContainerInfo() (project, workingDir, containerNetwork string) {
	hostname, err := os.Hostname()
	if err != nil {
		slog.Debug("Could not read hostname for compose labels", "error", err)
		return "", "", ""
	}

	// The hostname inside a Docker container is the container ID (first 12 chars).
	// We try to read it from /etc/hostname which contains the full ID.
	data, err := os.ReadFile("/etc/hostname")
	if err != nil {
		slog.Debug("Could not read /etc/hostname", "error", err)
		// Fallback: use the hostname directly
		data = []byte(hostname)
	}

	containerID := strings.TrimSpace(string(data))
	if containerID == "" {
		return "", "", ""
	}

	// Connect to Docker to inspect the current container
	dockerHost := LookupEnvOrString("DOCKER_HOST", "tcp://docker-proxy:2375")
	cli, err := dockerclient.NewClientWithOpts(
		dockerclient.WithHost(dockerHost),
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		slog.Debug("Could not create docker client to read compose labels", "error", err)
		return "", "", ""
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	info, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		slog.Debug("Could not inspect own container", "containerID", containerID[:12], "error", err)
		return "", "", ""
	}

	project = info.Config.Labels["com.docker.compose.project"]
	workingDir = info.Config.Labels["com.docker.compose.project.working_dir"]

	// Pick the first network the container is connected to
	for name := range info.NetworkSettings.Networks {
		containerNetwork = name
		break
	}

	slog.Debug("Read container info", "project", project, "workingDir", workingDir, "network", containerNetwork)
	return project, workingDir, containerNetwork
}

func (s *Size) String() string { return humanize.Bytes(s.Bytes) }

func (s *Size) Get() any { return s.Bytes }

func parseArgs() {
	// Parse the command-line arguments
	flag.BoolFunc("license", "print the license and exit", func(_ string) error {
		fmt.Println(LICENSE)
		os.Exit(0)
		return nil
	})
	flag.StringVar(
		&addr,
		"addr",
		LookupEnvOrString("ACEXY_LISTEN_ADDR", ":8080"),
		"address to listen on. Can be set with ACEXY_LISTEN_ADDR environment variable",
	)
	flag.StringVar(
		&scheme,
		"scheme",
		LookupEnvOrString("ACEXY_SCHEME", "http"),
		"scheme to use for the AceStream middleware. Can be set with ACEXY_SCHEME environment variable",
	)
	flag.StringVar(
		&host,
		"acestream-host",
		LookupEnvOrString("ACEXY_HOST", "localhost"),
		"host to use for the AceStream middleware. Can be set with ACEXY_HOST environment variable",
	)
	flag.IntVar(
		&port,
		"acestream-port",
		LookupEnvOrInt("ACEXY_PORT", 6878),
		"port to use for the AceStream middleware. Can be set with ACEXY_PORT environment variable",
	)
	flag.DurationVar(
		&streamTimeout,
		"m3u8-stream-timeout",
		LookupEnvOrDuration("ACEXY_M3U8_STREAM_TIMEOUT", 60*time.Second),
		"timeout in human-readable format to finish the stream. "+
			"Can be set with ACEXY_M3U8_STREAM_TIMEOUT environment variable",
	)
	flag.BoolVar(
		&m3u8,
		"m3u8",
		LookupEnvOrBool("ACEXY_M3U8", false),
		"enable M3U8 mode. Can be set with ACEXY_M3U8 environment variable.",
	)
	flag.DurationVar(
		&emptyTimeout,
		"empty-timeout",
		LookupEnvOrDuration("ACEXY_EMPTY_TIMEOUT", 30*time.Second),
		"timeout to consider a stream stalled when no data is received. "+
			"Can be set with ACEXY_EMPTY_TIMEOUT environment variable",
	)
	flag.IntVar(
		&emptyRetryCount,
		"empty-retry-count",
		LookupEnvOrInt("ACEXY_EMPTY_RETRY_COUNT", 3),
		"number of reconnect attempts when a stream stalls before giving up. "+
			"Set to 0 to disable retries. Can be set with ACEXY_EMPTY_RETRY_COUNT environment variable",
	)
	flag.Var(
		LookupEnvOrSize("ACEXY_BUFFER_SIZE", 4*1024*1024),
		"buffer-size",
		"buffer size in human-readable format to use when copying the data. "+
			"Can be set with ACEXY_BUFFER_SIZE environment variable",
	)
	flag.DurationVar(
		&noResponseTimeout,
		"no-response-timeout",
		LookupEnvOrDuration("ACEXY_NO_RESPONSE_TIMEOUT", 1*time.Second),
		"timeout in human-readable format to wait for a response from the AceStream middleware. "+
			"Can be set with ACEXY_NO_RESPONSE_TIMEOUT environment variable. "+
			"Depending on the network conditions, you may want to adjust this value",
	)
	flag.IntVar(
		&minReplicas,
		"min-replicas",
		LookupEnvOrInt("ACESTREAM_MIN_REPLICAS", 1),
		"minimum number of AceStream instances to keep running. Can be set with ACESTREAM_MIN_REPLICAS environment variable",
	)
	flag.IntVar(
		&maxReplicas,
		"max-replicas",
		LookupEnvOrInt("ACESTREAM_MAX_REPLICAS", 3),
		"maximum number of AceStream instances allowed. Can be set with ACESTREAM_MAX_REPLICAS environment variable",
	)
	flag.IntVar(
		&streamsPerInstance,
		"streams-per-instance",
		LookupEnvOrInt("ACESTREAM_STREAMS_PER_INSTANCE", 3),
		"maximum number of concurrent streams per AceStream instance. Can be set with ACESTREAM_STREAMS_PER_INSTANCE environment variable",
	)
	flag.DurationVar(
		&idleTimeout,
		"idle-timeout",
		LookupEnvOrDuration("ACESTREAM_IDLE_TIMEOUT", 300*time.Second),
		"time after which an idle AceStream instance is removed. Can be set with ACESTREAM_IDLE_TIMEOUT environment variable",
	)
	flag.DurationVar(
		&recycleTimeout,
		"recycle-timeout",
		LookupEnvOrDuration("ACESTREAM_RECYCLE_TIMEOUT", 60*time.Second),
		"idle time after which the entire pool is recycled with fresh instances. "+
			"Set to 0 to disable. Can be set with ACESTREAM_RECYCLE_TIMEOUT environment variable",
	)
	flag.DurationVar(
		&recycleCheckInterval,
		"recycle-check-interval",
		LookupEnvOrDuration("ACESTREAM_RECYCLE_CHECK_INTERVAL", 10*time.Second),
		"how often the recycle check runs. Can be set with ACESTREAM_RECYCLE_CHECK_INTERVAL environment variable",
	)
	flag.DurationVar(
		&scaleDownInterval,
		"scale-down-interval",
		LookupEnvOrDuration("ACESTREAM_SCALE_DOWN_INTERVAL", 15*time.Second),
		"how often the scale down check runs. Can be set with ACESTREAM_SCALE_DOWN_INTERVAL environment variable",
	)
	flag.StringVar(
		&composeProfile,
		"compose-profile",
		LookupEnvOrString("COMPOSE_PROFILE", "regular"),
		"Docker Compose profile to use (regular or vpn). Can be set with COMPOSE_PROFILE environment variable",
	)
	flag.StringVar(
		&acestreamImage,
		"acestream-image",
		LookupEnvOrString("ACESTREAM_IMAGE", "martinbjeldbak/acestream-http-proxy:latest"),
		"Docker image to use for AceStream instances. Can be set with ACESTREAM_IMAGE environment variable",
	)
	flag.StringVar(
		&dockerHost,
		"docker-host",
		LookupEnvOrString("DOCKER_HOST", "tcp://docker-proxy:2375"),
		"Docker host URL (socket proxy recommended). Can be set with DOCKER_HOST environment variable",
	)
	flag.IntVar(
		&containerFailureThreshold,
		"container-failure-threshold",
		LookupEnvOrInt("ACESTREAM_CONTAINER_FAILURE_THRESHOLD", 3),
		"consecutive container health check failures before marking instance unhealthy. "+
			"Can be set with ACESTREAM_CONTAINER_FAILURE_THRESHOLD environment variable",
	)
	flag.IntVar(
		&streamFailureThreshold,
		"stream-failure-threshold",
		LookupEnvOrInt("ACESTREAM_STREAM_FAILURE_THRESHOLD", 3),
		"consecutive times all streams in an instance stall before marking it unhealthy. "+
			"Can be set with ACESTREAM_STREAM_FAILURE_THRESHOLD environment variable",
	)
	flag.Parse()
}

func main() {
	// Parse the command-line arguments
	parseArgs()
	slog.SetLogLoggerLevel(LookupLogLevel())
	slog.Debug("CLI Args", "args", flag.CommandLine)

	var endpoint acexy.AcexyEndpoint
	if m3u8 {
		endpoint = acexy.M3U8_ENDPOINT
	} else {
		endpoint = acexy.MPEG_TS_ENDPOINT
	}
	// Read the Compose labels and network of the current container so that dynamic
	// instances belong to the same Compose stack and network
	composeProject, composeWorkingDir, containerNetwork := readContainerInfo()

	// Initialize the Orchestrator
	orch := &orchestrator.Orchestrator{
		MinReplicas:               minReplicas,
		MaxReplicas:               maxReplicas,
		StreamsPerInstance:        streamsPerInstance,
		IdleTimeout:               idleTimeout,
		RecycleTimeout:            recycleTimeout,
		RecycleCheckInterval:      recycleCheckInterval,
		ScaleDownInterval:         scaleDownInterval,
		Profile:                   composeProfile,
		Image:                     acestreamImage,
		DockerHost:                dockerHost,
		ComposeProject:            composeProject,
		ComposeWorkingDir:         composeWorkingDir,
		ContainerNetwork:          containerNetwork,
		ContainerFailureThreshold: containerFailureThreshold,
		StreamFailureThreshold:    streamFailureThreshold,
	}
	if err := orch.Init(); err != nil {
		slog.Error("Failed to initialize orchestrator", "error", err)
		os.Exit(1)
	}
	go orch.ScaleDownLoop()
	go orch.MonitorLoop()

	// Create a new Acexy instance
	acexy := &acexy.Acexy{
		Scheme:            scheme,
		Host:              host,
		Port:              port,
		Endpoint:          endpoint,
		EmptyTimeout:      emptyTimeout,
		EmptyRetryCount:   emptyRetryCount,
		BufferSize:        int(size.Bytes),
		NoResponseTimeout: noResponseTimeout,
		Orchestrator:      orch,
	}
	acexy.Init()
	slog.Debug("Acexy", "acexy", acexy)

	// Create a new HTTP server
	proxy := &Proxy{Acexy: acexy}
	mux := http.NewServeMux()
	mux.Handle(APIv1_URL+"/getstream", proxy)
	mux.Handle(APIv1_URL+"/getstream/", proxy)
	mux.Handle(APIv1_URL+"/status", proxy)
	mux.Handle("/", http.NotFoundHandler())

	// Capture shutdown signals to clean up containers
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	server := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-stop
		slog.Info("Shutdown signal received, cleaning up...")
		orch.Shutdown()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			slog.Warn("HTTP server shutdown error", "error", err)
		}
	}()

	// Start the HTTP server
	slog.Info("Starting server", "addr", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("Failed to start server", "error", err)
		os.Exit(1)
	}
}

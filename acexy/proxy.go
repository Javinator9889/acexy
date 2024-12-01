// Acexy - Copyright (C) 2024 - Javinator9889 <dev at javinator9889 dot com>
// This program comes with ABSOLUTELY NO WARRANTY; for details type `show w'.
// This is free software, and you are welcome to redistribute it
// under certain conditions; type `show c' for details.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"javinator9889/acexy/lib/acexy"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/dustin/go-humanize"
)

var (
	addr          string
	scheme        string
	host          string
	port          int
	streamTimeout time.Duration
	m3u8          bool
	emptyTimeout  time.Duration
	size          Size
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
	// Verify the request method
	if r.Method != http.MethodGet {
		slog.Error("Method not allowed", "method", r.Method, "path", r.URL.Path)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	// Verify the client has included the ID parameter
	aceId, err := acexy.AceIDFromParams(q)
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
		slog.Error("Failed to start stream", "error", err)
		http.Error(w, "Failed to start stream", http.StatusInternalServerError)
		return
	}

	// And start playing the stream. The `StartStream` will dump the contents of the new or
	// existing stream to the client. It takes an interface of `io.Writer` to write the stream
	// contents to. The `http.ResponseWriter` implements the `io.Writer` interface, so we can
	// pass it directly.
	slog.Debug("Starting stream", "path", r.URL.Path, "id", aceId)
	if err := p.Acexy.StartStream(stream, w); err != nil {
		slog.Error("Failed to start stream", "error", err)
		http.Error(w, "Failed to start stream", http.StatusInternalServerError)
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
		"address to listen on. Can be set with ACEXY_LISTEN_ADDR environment variable.",
	)
	flag.StringVar(
		&scheme,
		"scheme",
		LookupEnvOrString("ACEXY_SCHEME", "http"),
		"scheme to use for the AceStream middleware. Can be set with ACEXY_SCHEME environment variable.",
	)
	flag.StringVar(
		&host,
		"acestream-host",
		LookupEnvOrString("ACEXY_HOST", "localhost"),
		"host to use for the AceStream middleware. Can be set with ACEXY_HOST environment variable.",
	)
	flag.IntVar(
		&port,
		"acestream-port",
		LookupEnvOrInt("ACEXY_PORT", 6878),
		"port to use for the AceStream middleware. Can be set with ACEXY_PORT environment variable.",
	)
	flag.DurationVar(
		&streamTimeout,
		"m3u8-stream-timeout",
		LookupEnvOrDuration("ACEXY_M3U8_STREAM_TIMEOUT", 60*time.Second),
		"timeout in human-readable format to finish the stream. Can be set with ACEXY_M3U8_STREAM_TIMEOUT environment variable.",
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
		LookupEnvOrDuration("ACEXY_EMPTY_TIMEOUT", 1*time.Minute),
		"timeout in human-readable format to finish the stream when the source is empty. Can be set with ACEXY_EMPTY_TIMEOUT environment variable.",
	)
	flag.Var(
		LookupEnvOrSize("ACEXY_BUFFER_SIZE", 4*1024*1024),
		"buffer-size",
		"buffer size in human-readable format to use when copying the data. Can be set with ACEXY_BUFFER_SIZE environment variable.",
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
	// Create a new Acexy instance
	acexy := &acexy.Acexy{
		Scheme:       scheme,
		Host:         host,
		Port:         port,
		Endpoint:     endpoint,
		EmptyTimeout: emptyTimeout,
		BufferSize:   int(size.Bytes),
	}
	acexy.Init()
	slog.Debug("Acexy", "acexy", acexy)

	// Create a new HTTP server
	proxy := &Proxy{Acexy: acexy}
	mux := http.NewServeMux()
	mux.Handle(APIv1_URL+"/getstream", proxy)
	mux.Handle(APIv1_URL+"/getstream/", proxy)
	mux.Handle("/", http.NotFoundHandler())

	// Start the HTTP server
	slog.Info("Starting server", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("Failed to start server", "error", err)
		os.Exit(1)
	}
}

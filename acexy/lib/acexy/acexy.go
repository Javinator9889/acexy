// Acexy - Copyright (C) 2024 - Javinator9889 <dev at javinator9889 dot com>
// This program comes with ABSOLUTELY NO WARRANTY; for details type `show w'.
// This is free software, and you are welcome to redistribute it
// under certain conditions; type `show c' for details.
package acexy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"javinator9889/acexy/lib/orchestrator"
	"javinator9889/acexy/lib/pmw"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

// As of how the middleware is defined, we tell Go the structure that should match the HTTP
// response for AceStream: https://docs.acestream.net/developers/start-playback/#using-middleware.
// We are interested in the "playback_url" and the "command_url" fields: The first one
// references the stream, and the second one tells the stream to finish.
type AceStreamResponse struct {
	PlaybackURL       string `json:"playback_url"`
	StatURL           string `json:"stat_url"`
	CommandURL        string `json:"command_url"`
	Infohash          string `json:"infohash"`
	PlaybackSessionID string `json:"playback_session_id"`
	IsLive            int    `json:"is_live"`
	IsEncrypted       int    `json:"is_encrypted"`
	ClientSessionID   int    `json:"client_session_id"`
}

type AceStreamMiddleware struct {
	Response AceStreamResponse `json:"response"`
	Error    string            `json:"error"`
}

type AceStreamCommand struct {
	Response string `json:"response"`
	Error    string `json:"error"`
}

type AcexyStatus struct {
	Clients *uint  `json:"clients,omitempty"`
	Streams *uint  `json:"streams,omitempty"`
	ID      *AceID `json:"stream_id,omitempty"`
	StatURL string `json:"stat_url,omitempty"`
}

// The stream information is stored in a structure referencing the `AceStreamResponse`
// plus some extra information to determine whether we should keep the stream alive or not.
type AceStream struct {
	PlaybackURL string
	StatURL     string
	CommandURL  string
	ID          AceID
}

type ongoingStream struct {
	clients  uint
	done     chan struct{}
	player   *http.Response
	stream   *AceStream
	copier   *Copier
	writers  *pmw.PMultiWriter
	instance *orchestrator.AceStreamInstance // nil si no hay orquestación
}

// Structure referencing the AceStream Proxy - this is, ourselves
type Acexy struct {
	Scheme            string        // The scheme to be used when connecting to the AceStream middleware
	Host              string        // The host to be used when connecting to the AceStream middleware
	Port              int           // The port to be used when connecting to the AceStream middleware
	Endpoint          AcexyEndpoint // The endpoint to be used when connecting to the AceStream middleware
	EmptyTimeout      time.Duration // Timeout after which, if no data is written, the stream is closed
	EmptyRetryCount   int           // Number of reconnect attempts when a stream stalls (0 = no retry)
	BufferSize        int           // The buffer size to use when copying the data
	NoResponseTimeout time.Duration // Timeout to wait for a response from the AceStream middleware
	Orchestrator      *orchestrator.Orchestrator // nil si no hay orquestación dinámica

	// Information about ongoing streams
	streams    map[AceID]*ongoingStream
	mutex      *sync.Mutex
	middleware *http.Client
}

type AcexyEndpoint string

// The AceStream API available endpoints
const (
	M3U8_ENDPOINT    AcexyEndpoint = "/ace/manifest.m3u8"
	MPEG_TS_ENDPOINT AcexyEndpoint = "/ace/getstream"
)

// Initializes the Acexy structure
func (a *Acexy) Init() {
	a.streams = make(map[AceID]*ongoingStream)
	a.mutex = &sync.Mutex{}
	// The transport to be used when connecting to the AceStream middleware. We have to tweak it
	// a little bit to avoid compression and to limit the number of connections per host. Otherwise,
	// the AceStream Middleware won't work.
	a.middleware = &http.Client{
		Transport: &http.Transport{
			DisableCompression:    true,
			MaxIdleConns:          10,
			MaxConnsPerHost:       10,
			IdleConnTimeout:       30 * time.Second,
			ResponseHeaderTimeout: a.NoResponseTimeout,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// Starts a new stream. The stream is enqueued in the AceStream backend, returning a playback
// URL to reproduce it and a command URL to finish it. If the stream is already enqueued,
// the playback URL is returned. A number of clients can be reproducing the same stream at
// the same time through the middleware. When the last client finishes, the stream is removed.
// The stream is identified by the “id“ identifier. Optionally, takes extra parameters to
// customize the stream.
func (a *Acexy) FetchStream(aceId AceID, extraParams url.Values) (*AceStream, error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	// Check if the stream is already enqueued — no tocar instancias, el PMultiWriter ya distribuye
	if stream, ok := a.streams[aceId]; ok {
		return stream.stream, nil
	}

	// Elegir instancia via orchestrator o fallback al backend estático
	var middlewareResp *AceStreamMiddleware
	var err error
	var instance *orchestrator.AceStreamInstance

	if a.Orchestrator != nil {
		instance = a.Orchestrator.SelectInstance()
		if instance == nil {
			if a.Orchestrator.TotalInstances() < a.Orchestrator.MaxReplicas {
				instance, err = a.Orchestrator.ScaleUp()
				if err != nil {
					slog.Error("Failed to scale up", "error", err)
					return nil, fmt.Errorf("failed to scale up new instance: %w", err)
				}
			} else {
				return nil, errors.New("max replicas reached, no instance available")
			}
		}
		// Incrementar antes del request para evitar race condition:
		// si dos streams llegan a la vez, el segundo ya verá ActiveStreams=1
		instance.ActiveStreams++
		instance.LastActivity = time.Now()
		slog.Debug("Instance stream count", "instance", instance.Name,
			"activeStreams", instance.ActiveStreams)

		middlewareResp, err = GetStreamFromInstance(instance, a, aceId, extraParams)
		if err != nil {
			// Revertir si el request falla
			instance.ActiveStreams--
		}
	} else {
		middlewareResp, err = GetStream(a, aceId, extraParams)
	}

	if err != nil {
		slog.Error("Error getting stream middleware", "error", err)
		return nil, err
	}

	// We got the stream information, build the structure around it and register the stream
	slog.Debug("Middleware Information", "id", aceId, "middleware", middlewareResp)
	stream := &AceStream{
		PlaybackURL: middlewareResp.Response.PlaybackURL,
		StatURL:     middlewareResp.Response.StatURL,
		CommandURL:  middlewareResp.Response.CommandURL,
		ID:          aceId,
	}

	a.streams[aceId] = &ongoingStream{
		clients:  0,
		done:     make(chan struct{}),
		player:   nil,
		stream:   stream,
		writers:  pmw.New(),
		instance: instance,
	}
	return stream, nil
}

func (a *Acexy) StartStream(stream *AceStream, out io.Writer) error {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	// Get the ongoing stream
	ongoingStream, ok := a.streams[stream.ID]
	if !ok {
		slog.Debug("Stream not found", "stream", stream.ID)
		return fmt.Errorf(`stream "%s" not found`, stream.ID)
	}

	// Add the writer to the list of writers
	ongoingStream.writers.Add(out)

	// Register the new client
	ongoingStream.clients++

	// Calcular total de clientes en todos los streams
	var totalClients uint
	for _, s := range a.streams {
		totalClients += s.clients
	}

	if ongoingStream.player != nil {
		if ongoingStream.instance != nil {
			slog.Info("Reusing existing stream", "id", stream.ID, "stream_clients", ongoingStream.clients,
				"total_clients", totalClients, "instance", ongoingStream.instance.Name)
		} else {
			slog.Info("Reusing existing stream", "id", stream.ID, "stream_clients", ongoingStream.clients,
				"total_clients", totalClients)
		}
		return nil
	}
	if ongoingStream.instance != nil {
		slog.Info("Started new stream", "id", stream.ID, "stream_clients", ongoingStream.clients,
			"total_clients", totalClients, "instance", ongoingStream.instance.Name)
	} else {
		slog.Info("Started new stream", "id", stream.ID, "stream_clients", ongoingStream.clients,
			"total_clients", totalClients)
	}

	// Check if the stream is already being played

	resp, err := a.middleware.Get(stream.PlaybackURL)
	if err != nil {
		slog.Error("Failed to forward stream", "error", err)
		ongoingStream.clients--
		if ongoingStream.clients == 0 {
			if releaseErr := a.releaseStream(stream); releaseErr != nil {
				slog.Warn("Error releasing stream", "error", releaseErr)
			}
		}
		return err
	}

	// Forward the response to the writers
	idType, id := stream.ID.ID()
	ongoingStream.copier = &Copier{
		Destination:  ongoingStream.writers,
		Source:       resp.Body,
		EmptyTimeout: a.EmptyTimeout,
		BufferSize:   a.BufferSize,
		StreamID: string(idType) + ":" + id,
	}

	streamCopy := stream
	retryCount := a.EmptyRetryCount
	go func() {
		for {
			err := ongoingStream.copier.Copy()
			if errors.Is(err, ErrStreamStalled) && retryCount > 0 {
				retryCount--
				slog.Warn("Stream stalled, reconnecting",
					"stream", streamCopy.ID,
					"attemptsLeft", retryCount,
				)
				newResp, reconnErr := a.middleware.Get(streamCopy.PlaybackURL)
				if reconnErr != nil {
					slog.Error("Failed to reconnect stalled stream", "stream", streamCopy.ID, "error", reconnErr)
					break
				}
				ongoingStream.copier.Source = newResp.Body
				a.mutex.Lock()
				if ongoingStream.player != nil {
					_ = ongoingStream.player.Body.Close()
				}
				ongoingStream.player = newResp
				a.mutex.Unlock()
				continue
			}
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					slog.Debug("Connection closed", "stream", streamCopy.ID)
				} else {
					slog.Debug("Failed to copy response body", "stream", streamCopy.ID, "error", err)
				}
			}
			break
		}
		slog.Debug("Copy done", "stream", streamCopy.ID)
		select {
		case <-ongoingStream.done:
			slog.Debug("Stream already closed", "stream", streamCopy.ID)
		default:
			close(ongoingStream.done)
			slog.Debug("Stream closed", "stream", streamCopy.ID)
		}
	}()

	ongoingStream.player = resp
	return nil
}

// Releases a stream that is no longer being used. The stream is removed from the AceStream backend.
// If the stream is not enqueued, an error is returned. If the stream has clients reproducing it,
// the stream is not removed. The stream is identified by the “id“ identifier.
//
// Note: The global mutex is locked and unlocked by the caller.
func (a *Acexy) releaseStream(stream *AceStream) error {
	ongoingStream, ok := a.streams[stream.ID]
	if !ok {
		return fmt.Errorf(`stream "%s" not found`, stream.ID)
	}
	if ongoingStream.clients > 0 {
		return fmt.Errorf(`stream "%s" has clients`, stream.ID)
	}

	// Decrementar ActiveStreams en la instancia cuando el último cliente abandona
	if ongoingStream.instance != nil {
		if ongoingStream.instance.ActiveStreams > 0 {
			ongoingStream.instance.ActiveStreams--
		}
		ongoingStream.instance.LastActivity = time.Now()
		slog.Debug("Instance stream count after release", "instance", ongoingStream.instance.Name,
			"activeStreams", ongoingStream.instance.ActiveStreams)
	}

	// Remove the stream from the list
	defer delete(a.streams, stream.ID)
	slog.Debug("Stopping stream", "stream", stream.ID)
	// Close the stream
	if err := CloseStream(stream); err != nil {
		slog.Debug("Error closing stream", "error", err)
		return err
	}
	if ongoingStream.player != nil {
		slog.Debug("Closing player", "stream", stream.ID)
		ongoingStream.player.Body.Close()
	}

	// Close the `done' channel
	select {
	case <-ongoingStream.done:
		slog.Debug("Stream already closed", "stream", stream.ID)
	default:
		close(ongoingStream.done)
		slog.Debug("Stream done", "stream", stream.ID)
	}
	return nil
}

// Finishes a stream. The stream is removed from the AceStream backend. If the stream is not
// enqueued, an error is returned. If the stream has clients reproducing it, the stream is not
// removed. The stream is identified by the “id“ identifier.
func (a *Acexy) StopStream(stream *AceStream, out io.Writer) error {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	// Get the ongoing stream
	ongoingStream, ok := a.streams[stream.ID]
	if !ok {
		slog.Debug("Stream not found", "stream", stream.ID)
		return fmt.Errorf(`stream "%s" not found`, stream.ID)
	}

	// Remove the writer from the list of writers
	ongoingStream.writers.Remove(out)

	// Unregister the client
	if ongoingStream.clients > 0 {
		ongoingStream.clients--
		slog.Info("Client stopped", "stream", stream.ID, "clients", ongoingStream.clients)
	} else {
		slog.Warn("Stream has no clients", "stream", stream.ID)
	}

	// Check if we have to stop the stream
	if ongoingStream.clients == 0 {
		if err := a.releaseStream(stream); err != nil {
			slog.Warn("Error releasing stream", "error", err)
			return err
		}
		slog.Info("Stream done", "stream", stream.ID)
	}
	return nil
}

// Waits for the stream to finish. The stream is identified by the “id“ identifier. If the stream
// is not enqueued, nil is returned. The function returns a channel that will be closed when the
// stream finishes.
func (a *Acexy) WaitStream(stream *AceStream) <-chan struct{} {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	// Get the ongoing stream
	ongoingStream, ok := a.streams[stream.ID]
	if !ok {
		return nil
	}

	return ongoingStream.done
}

// Performs a request to the AceStream backend to start a new stream. It uses the Acexy
// structure to get the host and port of the AceStream backend. The stream is identified
// by the “id“ identifier. Optionally, takes extra parameters to customize the stream.
// Returns the response from the AceStream backend. If the request fails, an error is returned.
// If the `AceStreamMiddleware:error` field is not empty, an error is returned.
func GetStream(a *Acexy, aceId AceID, extraParams url.Values) (*AceStreamMiddleware, error) {
	slog.Debug("Getting stream", "id", aceId, "extraParams", extraParams)
	slog.Debug("Acexy Information", "scheme", a.Scheme, "host", a.Host, "port", a.Port)
	req, err := http.NewRequest("GET", a.Scheme+"://"+a.Host+":"+strconv.Itoa(a.Port)+string(a.Endpoint), nil)
	if err != nil {
		return nil, err
	}

	// Add the query parameters. We use a unique PID to identify the client accessing the stream.
	// This prevents errors when multiple streams are accessed at the same time. Because of
	// using the UUID package, we can be sure that the PID is unique.
	pid := uuid.NewString()
	slog.Debug("Temporary PID", "pid", pid, "stream", aceId)
	if extraParams == nil {
		extraParams = req.URL.Query()
	}
	idType, id := aceId.ID()
	extraParams.Set(string(idType), id)
	extraParams.Set("format", "json")
	extraParams.Set("pid", pid)
	// and set the headers
	req.Header.Set("Content-Type", "application/json")
	req.URL.RawQuery = extraParams.Encode()

	slog.Debug("Request URL", "url", req.URL.String())
	client := &http.Client{}
	res, err := client.Do(req)
	if err != nil {
		slog.Debug("Error getting stream", "error", err)
		return nil, err
	}
	slog.Debug("Stream response", "statusCode", res.StatusCode, "headers", res.Header, "res", res)
	defer res.Body.Close()

	// Read the response into the body
	body, err := io.ReadAll(res.Body)
	if err != nil {
		slog.Debug("Error reading stream response", "error", err)
		return nil, err
	}

	slog.Debug("Stream response", "response", string(body))
	var response AceStreamMiddleware
	if err := json.Unmarshal(body, &response); err != nil {
		slog.Debug("Error unmarshalling stream response", "error", err)
		return nil, err
	}

	if response.Error != "" {
		slog.Debug("Error in stream response", "error", response.Error)
		return nil, errors.New(response.Error)
	}
	return &response, nil
}

// Closes the stream by performing a request to the AceStream backend. The `stream` parameter
// contains the command URL to send data to the middleware. As of the documentation, it is needed
// to add a "method=stop" query parameter to finish the stream.
func CloseStream(stream *AceStream) error {
	req, err := http.NewRequest("GET", stream.CommandURL, nil)
	if err != nil {
		return err
	}

	q := req.URL.Query()
	q.Add("method", "stop")
	req.URL.RawQuery = q.Encode()

	client := &http.Client{}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	// Read the response into the body
	body, err := io.ReadAll(res.Body)
	if err != nil {
		slog.Debug("Error reading stream response", "error", err)
		return err
	}

	var response AceStreamCommand
	if err := json.Unmarshal(body, &response); err != nil {
		slog.Debug("Error unmarshalling stream response", "error", err)
		return err
	}

	if response.Error != "" {
		slog.Debug("Error in stream response", "error", response.Error)
		return errors.New(response.Error)
	}
	return nil
}

// Gets the status of a stream. If the `id` parameter is nil, the global status is returned.
// If the stream is not enqueued, an error is returned. The stream is identified by the “id“
// identifier.
func (a *Acexy) GetStatus(id *AceID) (AcexyStatus, error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	// Return the global status if no ID is given
	if id == nil {
		streams := uint(len(a.streams))
		return AcexyStatus{Streams: &streams}, nil
	}

	// Check if the stream is already enqueued
	if stream, ok := a.streams[*id]; ok {
		return AcexyStatus{
			Clients: &stream.clients,
			ID:      id,
			StatURL: stream.stream.StatURL,
		}, nil
	}

	return AcexyStatus{}, fmt.Errorf(`stream "%s" not found`, id)
}

// GetStreamFromInstance realiza la petición al backend de una instancia específica del pool.
// Es equivalente a GetStream pero usa el host/port de la instancia en lugar del estático.
func GetStreamFromInstance(instance *orchestrator.AceStreamInstance, a *Acexy, aceId AceID, extraParams url.Values) (*AceStreamMiddleware, error) {
	slog.Debug("Getting stream from instance", "id", aceId, "host", instance.Host, "port", instance.Port)

	req, err := http.NewRequest("GET", a.Scheme+"://"+instance.Host+":"+strconv.Itoa(instance.Port)+string(a.Endpoint), nil)
	if err != nil {
		return nil, err
	}

	pid := uuid.NewString()
	slog.Debug("Temporary PID", "pid", pid, "stream", aceId)
	if extraParams == nil {
		extraParams = req.URL.Query()
	}
	idType, id := aceId.ID()
	extraParams.Set(string(idType), id)
	extraParams.Set("format", "json")
	extraParams.Set("pid", pid)
	req.Header.Set("Content-Type", "application/json")
	req.URL.RawQuery = extraParams.Encode()

	slog.Debug("Request URL", "url", req.URL.String())
	client := &http.Client{}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	var response AceStreamMiddleware
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}

	if response.Error != "" {
		return nil, errors.New(response.Error)
	}
	return &response, nil
}

// Creates a timeout channel that will be closed after the given timeout
func SetTimeout(timeout time.Duration) chan struct{} {
	// Create a channel that will be closed after the given timeout
	timeoutChan := make(chan struct{})

	go func() {
		time.Sleep(timeout)
		close(timeoutChan)
	}()

	return timeoutChan
}

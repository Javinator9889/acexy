package main

import (
	"context"
	"fmt"
	"javinator9889/acexy/lib/acexy"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"
)

// Mock AceStream Backend
func startMockBackend() *httptest.Server {
	mux := http.NewServeMux()
	
	// /ace/getstream endpoint
	mux.HandleFunc("/ace/getstream", func(w http.ResponseWriter, r *http.Request) {
		// Return JSON response with playback URL
		// We'll point playback URL to another handler on this server
		host := r.Host
		jsonResp := fmt.Sprintf(`{
			"response": {
				"playback_url": "http://%s/stream",
				"stat_url": "http://%s/stat",
				"command_url": "http://%s/command",
				"infohash": "hash",
				"playback_session_id": "session",
				"is_live": 1,
				"is_encrypted": 0,
				"client_session_id": 123
			},
			"error": null
		}`, host, host, host)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(jsonResp))
	})

	// /stream endpoint (Infinite stream)
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		// Write data continuously
		chunk := make([]byte, 1024) // 1KB chunk
		for i := 0; i < len(chunk); i++ {
			chunk[i] = 'A'
		}
		
		for {
			_, err := w.Write(chunk)
			if err != nil {
				return
			}
			// Don't sleep, we want to fill buffers
			// But maybe a tiny sleep to yield
			// time.Sleep(1 * time.Millisecond)
		}
	})

	// /command endpoint
	mux.HandleFunc("/command", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"response": "ok", "error": null}`))
	})

	return httptest.NewServer(mux)
}

func TestDeadlockReproduction(t *testing.T) {
	// Setup Logging
	opts := &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}
	handler := slog.NewTextHandler(os.Stdout, opts)
	logger := slog.New(handler)
	slog.SetDefault(logger)

	// Start Mock Backend
	backend := startMockBackend()
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	backendPort := backendURL.Port()
	backendHost := backendURL.Hostname()

	// Configure Acexy
	// Small buffer size to fill it quickly
	bufferSize := 1024 

	a := &acexy.Acexy{
		Scheme:            "http",
		Host:              backendHost,
		Port:              mustParseInt(backendPort),
		Endpoint:          acexy.MPEG_TS_ENDPOINT,
		EmptyTimeout:      5 * time.Second,
		BufferSize:        bufferSize,
		NoResponseTimeout: 2 * time.Second,
	}
	a.Init()

	proxy := &Proxy{Acexy: a}
	
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Helper to make a request
	makeRequest := func(client *http.Client, id string) (*http.Response, error) {
		req, _ := http.NewRequest("GET", proxyServer.URL+"/ace/getstream?id="+id, nil)
		return client.Do(req)
	}

	streamID := "teststream"

	// 1. Client A connects and blocks (reads nothing)
	// We need a custom client that doesn't read the body
	fmt.Println("Step 1: Client A connecting...")
	clientA := &http.Client{Transport: &http.Transport{}}
	respA, err := makeRequest(clientA, streamID)
	if err != nil {
		t.Fatalf("Client A failed to connect: %v", err)
	}
	if respA.StatusCode != 200 {
		t.Fatalf("Client A got status %d", respA.StatusCode)
	}
	// Client A does NOT read body. It just holds the connection.
	// Since the backend sends infinite data, the buffer in Proxy (and OS buffers) will fill up.
	// Eventually pmw.Write should block.

	fmt.Println("Waiting for buffers to fill...")
	time.Sleep(2 * time.Second) // Give it time to fill buffers and block pmw.Write

	// 2. Client B connects to the SAME stream
	fmt.Println("Step 2: Client B connecting...")
	clientB := &http.Client{Timeout: 1 * time.Second} // Short timeout for B? No, we want it to connect then disconnect.
	// We use a context to cancel B
	ctxB, cancelB := context.WithCancel(context.Background())
	reqB, _ := http.NewRequestWithContext(ctxB, "GET", proxyServer.URL+"/ace/getstream?id="+streamID, nil)
	
	// Start B in goroutine so we can cancel it
	var wgB sync.WaitGroup
	wgB.Add(1)
	go func() {
		defer wgB.Done()
		respB, err := clientB.Do(reqB)
		if err == nil {
			respB.Body.Close()
		}
	}()

	// Wait a bit for B to connect and be added to writers
	time.Sleep(500 * time.Millisecond)

	// 3. Client B disconnects
	fmt.Println("Step 3: Client B disconnecting...")
	cancelB()
	wgB.Wait()

	// Wait for HandleStream to trigger StopStream -> Remove
	time.Sleep(500 * time.Millisecond)

	// 4. Client C connects (should succeed if no deadlock)
	fmt.Println("Step 4: Client C connecting...")
	clientC := &http.Client{Timeout: 2 * time.Second}
	
	doneC := make(chan error)
	go func() {
		respC, err := makeRequest(clientC, "otherstream") // different ID to trigger FetchStream -> Lock
		if err != nil {
			doneC <- err
			return
		}
		respC.Body.Close()
		doneC <- nil
	}()

	select {
	case err := <-doneC:
		if err != nil {
			t.Fatalf("Client C failed: %v", err)
		}
		fmt.Println("Client C connected successfully!")
	case <-time.After(5 * time.Second):
		t.Fatal("Client C timed out! Deadlock detected.")
	}

	// Cleanup Client A
	respA.Body.Close()
}

func mustParseInt(s string) int {
	var i int
	fmt.Sscanf(s, "%d", &i)
	return i
}

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

// TestMultiClientStreamSurvival verifies that when a slow client is evicted,
// the stream continues working for healthy clients. This is the core regression
// test for the bug where a single slow writer killed the entire stream.
func TestMultiClientStreamSurvival(t *testing.T) {
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, opts)))

	backend := startMockBackend()
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)

	// Use a small buffer so flushes happen quickly
	a := &acexy.Acexy{
		Scheme:            "http",
		Host:              backendURL.Hostname(),
		Port:              mustParseInt(backendURL.Port()),
		Endpoint:          acexy.MPEG_TS_ENDPOINT,
		EmptyTimeout:      10 * time.Second,
		BufferSize:        1024,
		NoResponseTimeout: 2 * time.Second,
	}
	a.Init()

	proxy := &Proxy{Acexy: a}
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	streamID := "survival-test"

	// Client A: healthy reader that actively consumes data
	t.Log("Client A connecting (healthy reader)...")
	clientA := &http.Client{Transport: &http.Transport{}}
	reqA, _ := http.NewRequest("GET", proxyServer.URL+"/ace/getstream?id="+streamID, nil)
	respA, err := clientA.Do(reqA)
	if err != nil {
		t.Fatalf("Client A failed to connect: %v", err)
	}
	defer respA.Body.Close()

	// Start actively reading Client A's data in background
	bytesReadA := make(chan int64, 1)
	clientAErr := make(chan error, 1)
	ctx, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	go func() {
		buf := make([]byte, 4096)
		var total int64
		for {
			select {
			case <-ctx.Done():
				bytesReadA <- total
				return
			default:
			}
			n, err := respA.Body.Read(buf)
			total += int64(n)
			if err != nil {
				clientAErr <- err
				bytesReadA <- total
				return
			}
		}
	}()

	// Wait for Client A to start receiving data
	time.Sleep(500 * time.Millisecond)

	// Client B: slow reader that never reads (will be evicted by PMultiWriter timeout)
	t.Log("Client B connecting (slow reader - will be evicted)...")
	clientB := &http.Client{Transport: &http.Transport{}}
	reqB, _ := http.NewRequest("GET", proxyServer.URL+"/ace/getstream?id="+streamID, nil)
	respB, err := clientB.Do(reqB)
	if err != nil {
		t.Fatalf("Client B failed to connect: %v", err)
	}
	// Client B does NOT read. Its OS buffers will fill, then PMultiWriter will evict it.

	// Wait for PMultiWriter's write timeout (5s) to evict Client B, plus margin
	t.Log("Waiting for Client B eviction...")
	time.Sleep(8 * time.Second)

	// Close Client B's body now (after eviction)
	respB.Body.Close()

	// Verify Client A is still receiving data AFTER eviction
	t.Log("Verifying Client A is still alive after eviction...")

	// Check that no read error occurred on Client A
	select {
	case err := <-clientAErr:
		t.Fatalf("Client A got an error (stream was killed): %v", err)
	default:
		// No error - good
	}

	// Read a bit more data to confirm the stream is still flowing
	time.Sleep(2 * time.Second)

	select {
	case err := <-clientAErr:
		t.Fatalf("Client A got an error after waiting (stream died): %v", err)
	default:
		// Still no error - stream survived!
	}

	// Stop Client A and check total bytes
	cancelA()
	totalBytes := <-bytesReadA
	t.Logf("Client A read %d bytes total - stream survived slow consumer eviction", totalBytes)

	if totalBytes == 0 {
		t.Fatal("Client A read 0 bytes - stream was not working")
	}
}

// TestSingleClientEvictionNoCrash reproduces the container crash reported in
// https://github.com/Javinator9889/acexy/issues/44#issuecomment-4057160200
//
// Scenario: a single-client stream where the only client never reads. The
// PMultiWriter write timeout evicts it, causing OnEvict → releaseStream →
// player.Body.Close(). This makes the copier's io.Copy return, and its
// cleanup goroutine also calls PMultiWriter.Close(). Before the fix,
// Close() panicked on the already-closed channel, crashing the process.
//
// In Go tests a panic in any goroutine kills the entire test binary, so
// this test reliably fails (crashes) without the idempotent-Close fix.
func TestSingleClientEvictionNoCrash(t *testing.T) {
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, opts)))

	backend := startMockBackend()
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)

	a := &acexy.Acexy{
		Scheme:            "http",
		Host:              backendURL.Hostname(),
		Port:              mustParseInt(backendURL.Port()),
		Endpoint:          acexy.MPEG_TS_ENDPOINT,
		EmptyTimeout:      10 * time.Second,
		BufferSize:        1024, // small buffer so flushes happen quickly
		NoResponseTimeout: 2 * time.Second,
	}
	a.Init()

	proxy := &Proxy{Acexy: a}
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Single client connects but NEVER reads — OS buffers fill,
	// PMultiWriter evicts after 5 s, OnEvict fires with clients 1→0,
	// releaseStream closes everything, copier goroutine also cleans up.
	t.Log("Connecting single slow client...")
	client := &http.Client{Transport: &http.Transport{}}
	resp, err := client.Get(proxyServer.URL + "/ace/getstream?id=crash-repro")
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}

	// Wait for the eviction (5 s timeout) + margin for the full
	// releaseStream → copier cleanup chain to complete.
	t.Log("Waiting for eviction + cleanup...")
	time.Sleep(8 * time.Second)

	// Close the response body after the dust has settled.
	resp.Body.Close()

	// Give a moment for any deferred goroutine panics to surface.
	time.Sleep(500 * time.Millisecond)

	// Verify the proxy is still alive by hitting the status endpoint.
	statusResp, err := http.Get(proxyServer.URL + "/ace/status")
	if err != nil {
		t.Fatalf("Proxy is dead after eviction: %v", err)
	}
	statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("Proxy status returned %d, expected 200", statusResp.StatusCode)
	}
	t.Log("Proxy survived single-client eviction without crashing")
}

func mustParseInt(s string) int {
	var i int
	fmt.Sscanf(s, "%d", &i)
	return i
}

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

// TestRapidChannelSwitching reproduces the crash from rapid IPTV channel
// scanning reported in https://github.com/Javinator9889/acexy/issues/44.
//
// The IPTV client connects to a channel, stays <1 second, disconnects, and
// immediately connects to the next channel. This creates a race between
// releaseStream (called from StopStream under mutex) and the copier
// goroutine, both trying to close(ongoingStream.done). Before the sync.Once
// fix, this was a select/default pattern that allowed concurrent callers
// to both call close() — panicking with "close of closed channel".
//
// The test launches 20 streams sequentially with rapid connect/disconnect
// to maximize the probability of hitting the race window. A panic in any
// goroutine kills the entire test binary, so if this test crashes, the
// race is present.
func TestRapidChannelSwitching(t *testing.T) {
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
		BufferSize:        1024, // small buffer for fast flushes
		NoResponseTimeout: 2 * time.Second,
	}
	a.Init()

	proxy := &Proxy{Acexy: a}
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Simulate rapid channel switching: connect, read briefly, disconnect, repeat
	for i := 0; i < 20; i++ {
		streamID := fmt.Sprintf("rapid-switch-%d", i)
		t.Logf("Channel %d: switching to %s", i, streamID)

		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET",
			proxyServer.URL+"/ace/getstream?id="+streamID, nil)

		client := &http.Client{Transport: &http.Transport{}}
		resp, err := client.Do(req)
		if err != nil {
			t.Logf("Channel %d: connection error (expected during rapid switching): %v", i, err)
			cancel()
			continue
		}

		// Read a tiny bit of data (or nothing at all) then immediately disconnect
		buf := make([]byte, 512)
		resp.Body.Read(buf) // may or may not succeed — doesn't matter
		cancel()            // trigger client disconnect
		resp.Body.Close()

		// Minimal delay — just enough for goroutine scheduling
		time.Sleep(10 * time.Millisecond)
	}

	// Wait for all goroutines spawned during the rapid switching to settle.
	// The copier goroutines and eviction callbacks run asynchronously.
	t.Log("Waiting for goroutine cleanup...")
	time.Sleep(2 * time.Second)

	// Verify the proxy is still alive
	statusResp, err := http.Get(proxyServer.URL + "/ace/status")
	if err != nil {
		t.Fatalf("Proxy is dead after rapid channel switching: %v", err)
	}
	statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("Proxy status returned %d, expected 200", statusResp.StatusCode)
	}
	t.Log("Proxy survived rapid channel switching without crashing")
}

// startSlowMockBackend returns a mock AceStream backend where the
// /ace/getstream API response is delayed by the given duration. This
// simulates the real AceStream engine which can take several seconds to
// set up a P2P stream before returning the playback URL.
func startSlowMockBackend(apiDelay time.Duration) *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/ace/getstream", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(apiDelay)
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

	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 1024)
		for i := range chunk {
			chunk[i] = 'A'
		}
		for {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})

	mux.HandleFunc("/command", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"response": "ok", "error": null}`))
	})

	return httptest.NewServer(mux)
}

// TestRapidSwitchingWithSlowBackend reproduces the proxy hang reported in
// https://github.com/Javinator9889/acexy/issues/44 when rapidly switching
// channels with a slow AceStream backend.
//
// Before the fix, FetchStream and StartStream held a.mutex during HTTP calls
// to the backend (up to NoResponseTimeout). Rapid channel switching piled up
// goroutines on the mutex, making the proxy completely unresponsive — even
// /ace/status would time out.
//
// The fix releases the mutex before HTTP calls and re-acquires after.
// This test verifies that /ace/status responds within 1 second while
// multiple slow FetchStream calls are in flight.
func TestRapidSwitchingWithSlowBackend(t *testing.T) {
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, opts)))

	// Backend takes 2 seconds to respond to each /ace/getstream request
	backend := startSlowMockBackend(2 * time.Second)
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)

	a := &acexy.Acexy{
		Scheme:            "http",
		Host:              backendURL.Hostname(),
		Port:              mustParseInt(backendURL.Port()),
		Endpoint:          acexy.MPEG_TS_ENDPOINT,
		EmptyTimeout:      10 * time.Second,
		BufferSize:        1024,
		NoResponseTimeout: 5 * time.Second,
	}
	a.Init()

	proxy := &Proxy{Acexy: a}
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Launch 5 rapid channel switches — each will block for 2s on the backend
	t.Log("Launching 5 concurrent stream requests (each takes 2s on backend)...")
	for i := 0; i < 5; i++ {
		go func(id int) {
			streamID := fmt.Sprintf("slow-backend-%d", id)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "GET",
				proxyServer.URL+"/ace/getstream?id="+streamID, nil)
			client := &http.Client{Transport: &http.Transport{}}
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}(i)
	}

	// Give the requests a moment to reach FetchStream and block on the backend
	time.Sleep(500 * time.Millisecond)

	// The critical test: /ace/status must respond quickly even while
	// 5 FetchStream calls are blocked waiting on the slow backend.
	// Before the fix, this would time out because all operations were
	// serialized through the mutex.
	t.Log("Checking /ace/status while backend calls are in flight...")
	statusClient := &http.Client{Timeout: 1 * time.Second}
	statusResp, err := statusClient.Get(proxyServer.URL + "/ace/status")
	if err != nil {
		t.Fatalf("Status endpoint blocked while backend calls in flight: %v"+
			"\nThis means the mutex is held during HTTP calls (pre-fix behavior)", err)
	}
	statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("Status returned %d, expected 200", statusResp.StatusCode)
	}
	t.Log("PASS: Status endpoint responded while slow backend calls were in flight")

	// Wait for all backend calls to finish
	time.Sleep(3 * time.Second)
}

// TestThreeClientStress exercises the proxy under a realistic multi-client
// workload that mixes long-playing streams with rapid channel switching.
//
// The test runs five phases:
//  1. Three clients each long-play a different channel
//  2. One client rapid-switches while two long-play
//  3. Two clients rapid-switch while one long-plays
//  4. Two clients long-play the SAME channel while one rapid-switches
//  5. All three long-play different channels again (stability check)
//
// Throughout all phases, the proxy must stay responsive (health check via
// /ace/status responds within 1 second) and long-playing clients must
// continue to receive data without interruption.
func TestThreeClientStress(t *testing.T) {
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
		EmptyTimeout:      30 * time.Second,
		BufferSize:        1024,
		NoResponseTimeout: 2 * time.Second,
	}
	a.Init()

	proxy := &Proxy{Acexy: a}
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// longPlay streams data from a channel, recording bytes received.
	// Returns a cancel func and a channel that will receive total bytes
	// when the stream ends.
	longPlay := func(name, streamID string) (context.CancelFunc, <-chan int64, <-chan error) {
		ctx, cancel := context.WithCancel(context.Background())
		bytesRead := make(chan int64, 1)
		errCh := make(chan error, 1)
		go func() {
			req, _ := http.NewRequestWithContext(ctx, "GET",
				proxyServer.URL+"/ace/getstream?id="+streamID, nil)
			client := &http.Client{Transport: &http.Transport{}}
			resp, err := client.Do(req)
			if err != nil {
				errCh <- err
				bytesRead <- 0
				return
			}
			defer resp.Body.Close()
			buf := make([]byte, 4096)
			var total int64
			for {
				n, err := resp.Body.Read(buf)
				total += int64(n)
				if err != nil {
					bytesRead <- total
					return
				}
			}
		}()
		return cancel, bytesRead, errCh
	}

	// rapidSwitch connects and disconnects from N different channels quickly.
	rapidSwitch := func(name string, count int, interval time.Duration) {
		for i := 0; i < count; i++ {
			streamID := fmt.Sprintf("%s-switch-%d", name, i)
			ctx, cancel := context.WithCancel(context.Background())
			req, _ := http.NewRequestWithContext(ctx, "GET",
				proxyServer.URL+"/ace/getstream?id="+streamID, nil)
			client := &http.Client{Transport: &http.Transport{}}
			resp, err := client.Do(req)
			if err == nil {
				buf := make([]byte, 512)
				resp.Body.Read(buf)
				resp.Body.Close()
			}
			cancel()
			time.Sleep(interval)
		}
	}

	checkHealth := func(phase string) {
		t.Helper()
		statusClient := &http.Client{Timeout: 1 * time.Second}
		resp, err := statusClient.Get(proxyServer.URL + "/ace/status")
		if err != nil {
			t.Fatalf("[%s] Proxy unresponsive: %v", phase, err)
		}
		resp.Body.Close()
	}

	// ── Phase 1: Three clients long-play different channels ──
	t.Log("Phase 1: Three clients long-playing different channels")
	cancelA, bytesA, _ := longPlay("A", "long-A")
	cancelB, bytesB, _ := longPlay("B", "long-B")
	cancelC, bytesC, _ := longPlay("C", "long-C")
	time.Sleep(500 * time.Millisecond)
	checkHealth("Phase 1")
	cancelA()
	cancelB()
	cancelC()
	bA := <-bytesA
	bB := <-bytesB
	bC := <-bytesC
	t.Logf("Phase 1: A=%d B=%d C=%d bytes", bA, bB, bC)
	if bA == 0 || bB == 0 || bC == 0 {
		t.Fatal("Phase 1: at least one client received no data")
	}

	time.Sleep(200 * time.Millisecond) // let goroutines settle

	// ── Phase 2: A+C long-play, B rapid-switches ──
	t.Log("Phase 2: A+C long-play, B rapid-switches")
	cancelA, bytesA, errA := longPlay("A", "long-A-p2")
	cancelC, bytesC, errC := longPlay("C", "long-C-p2")
	time.Sleep(300 * time.Millisecond)
	rapidSwitch("B", 8, 50*time.Millisecond)
	checkHealth("Phase 2 after switching")
	// Verify A and C are still alive (no error)
	select {
	case err := <-errA:
		t.Fatalf("Phase 2: Client A died during switching: %v", err)
	default:
	}
	select {
	case err := <-errC:
		t.Fatalf("Phase 2: Client C died during switching: %v", err)
	default:
	}
	cancelA()
	cancelC()
	bA = <-bytesA
	bC = <-bytesC
	t.Logf("Phase 2: A=%d C=%d bytes", bA, bC)
	if bA == 0 || bC == 0 {
		t.Fatal("Phase 2: long-playing client received no data")
	}

	time.Sleep(200 * time.Millisecond)

	// ── Phase 3: A+B rapid-switch, C long-plays ──
	t.Log("Phase 3: A+B rapid-switch concurrently, C long-plays")
	cancelC, bytesC, errC = longPlay("C", "long-C-p3")
	time.Sleep(300 * time.Millisecond)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); rapidSwitch("A", 6, 30*time.Millisecond) }()
	go func() { defer wg.Done(); rapidSwitch("B", 6, 30*time.Millisecond) }()
	wg.Wait()
	checkHealth("Phase 3 after dual switching")
	select {
	case err := <-errC:
		t.Fatalf("Phase 3: Client C died during dual switching: %v", err)
	default:
	}
	cancelC()
	bC = <-bytesC
	t.Logf("Phase 3: C=%d bytes", bC)
	if bC == 0 {
		t.Fatal("Phase 3: long-playing client received no data")
	}

	time.Sleep(200 * time.Millisecond)

	// ── Phase 4: A+B long-play SAME channel, C rapid-switches ──
	t.Log("Phase 4: A+B same channel, C rapid-switches")
	cancelA, bytesA, errA = longPlay("A", "shared-channel")
	cancelB, bytesB, errB := longPlay("B", "shared-channel")
	time.Sleep(300 * time.Millisecond)
	rapidSwitch("C", 8, 50*time.Millisecond)
	checkHealth("Phase 4 after switching")
	select {
	case err := <-errA:
		t.Fatalf("Phase 4: Client A died during switching: %v", err)
	default:
	}
	select {
	case err := <-errB:
		t.Fatalf("Phase 4: Client B died during switching: %v", err)
	default:
	}
	cancelA()
	cancelB()
	bA = <-bytesA
	bB = <-bytesB
	t.Logf("Phase 4: A=%d B=%d bytes (same channel)", bA, bB)
	if bA == 0 || bB == 0 {
		t.Fatal("Phase 4: shared-channel client received no data")
	}

	time.Sleep(200 * time.Millisecond)

	// ── Phase 5: All three long-play again (stability) ──
	t.Log("Phase 5: All three long-play (final stability check)")
	cancelA, bytesA, _ = longPlay("A", "final-A")
	cancelB, bytesB, _ = longPlay("B", "final-B")
	cancelC, bytesC, _ = longPlay("C", "final-C")
	time.Sleep(500 * time.Millisecond)
	checkHealth("Phase 5")
	cancelA()
	cancelB()
	cancelC()
	bA = <-bytesA
	bB = <-bytesB
	bC = <-bytesC
	t.Logf("Phase 5: A=%d B=%d C=%d bytes", bA, bB, bC)
	if bA == 0 || bB == 0 || bC == 0 {
		t.Fatal("Phase 5: at least one client received no data after stress test")
	}

	t.Log("PASS: All 5 phases completed — proxy survived 3-client stress test")
}

func mustParseInt(s string) int {
	var i int
	fmt.Sscanf(s, "%d", &i)
	return i
}

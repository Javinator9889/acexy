package pmw

import (
	"context"
	"sync"
	"testing"
	"time"
)

type blockWriter struct {
	mu      sync.Mutex
	blocked bool
}

func (b *blockWriter) Write(p []byte) (n int, err error) {
	b.mu.Lock()
	b.blocked = true
	b.mu.Unlock()
	
	// Block forever
	select {}
}

type fastWriter struct {
	mu    sync.Mutex
	wrote int
}

func (f *fastWriter) Write(p []byte) (n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wrote += len(p)
	return len(p), nil
}

func TestSlowWriterEviction(t *testing.T) {
	fast := &fastWriter{}
	slow := &blockWriter{}
	
	timeout := 100 * time.Millisecond
	pmw := New(context.Background(), timeout, fast, slow)
	
	data := []byte("test data")
	_, err := pmw.Write(data)
	
	if err == nil {
		t.Error("Expected error from Write due to slow consumer, got nil")
	}
	
	// The fast writer should have received data
	fast.mu.Lock()
	if fast.wrote != len(data) {
		t.Errorf("Fast writer should have received %d bytes, got %d", len(data), fast.wrote)
	}
	fast.mu.Unlock()
	
	// Give some time for the async removal to happen
	time.Sleep(200 * time.Millisecond)
	
	pmw.RLock()
	defer pmw.RUnlock()
	if len(pmw.writers) != 1 {
		t.Errorf("Expected 1 writer after eviction, got %d", len(pmw.writers))
	}
	
	if pmw.writers[0] != fast {
		t.Error("Remaining writer should be the fast one")
	}
}

func TestNormalWrite(t *testing.T) {
	w1 := &fastWriter{}
	w2 := &fastWriter{}
	
	pmw := New(context.Background(), 1*time.Second, w1, w2)
	
	data := []byte("hello")
	n, err := pmw.Write(data)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if n != len(data) {
		t.Errorf("Expected n=%d, got %d", len(data), n)
	}
	
	if w1.wrote != len(data) || w2.wrote != len(data) {
		t.Error("Both writers should have received data")
	}
}

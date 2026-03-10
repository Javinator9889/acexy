// Acexy - Copyright (C) 2024 - Javinator9889 <dev at javinator9889 dot com>
// This program comes with ABSOLUTELY NO WARRANTY; for details type `show w'.
// This is free software, and you are welcome to redistribute it
// under certain conditions; type `show c' for details.
//
// Package pmw (Parallel MultiWriter) contains an implementation of an "io.Writer" that
// duplicates it's writes to all the provided writers, similar to the Unix
// tee(1) command. Writers can be added and removed dynamically after creation. Each write is
// done in a separate goroutine, so the writes are done in parallel. This package is useful
// when you want to write to multiple writers at the same time, but don't want to block on
// each write. Errors that may occur are gathered and returned after all writes are done.
//
// Example:
//
//	package main
//
//	import (
//		"os"
//		"lib/pmw"
//	)
//
//	func main() {
//		w := multiwriter.New(os.Stdout, os.Stderr)
//
//		w.Write([]byte("written to stdout AND stderr\n"))
//
//		w.Remove(os.Stderr)
//
//		w.Write([]byte("written to ONLY stdout\n"))
//
//		w.Remove(os.Stdout)
//		w.Add(os.Stderr)
//
//		w.Write([]byte("written to ONLY stderr\n"))
//	}
package pmw

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// PMultiWriter is an implementation of an "io.Writer" that duplicates its writes
// to all the provided writers, similar to the Unix tee(1) command. Writers can be
// added and removed dynamically after creation. Each write is done in a separate
// goroutine, so the writes are done in parallel.
type PMultiWriter struct {
	sync.RWMutex
	writers      []io.Writer
	closed       chan struct{}
	ctx          context.Context
	writeTimeout time.Duration
	onEvict      func(io.Writer)
}

// PMultiWriterError is an error that occurs when writing to multiple writers.
type PMultiWriterError struct {
	Errors  []error
	Writers int
}

// Error returns a string representation of the error.
func (e PMultiWriterError) Error() string {
	sb := strings.Builder{}
	sb.WriteString(fmt.Sprintf("errors (%d) when writing to %d writers\n", len(e.Errors), e.Writers))
	for _, err := range e.Errors {
		sb.WriteString(err.Error())
		sb.WriteString("\n")
	}
	return sb.String()
}

// New creates a writer that duplicates its writes to all the provided writers,
// similar to the Unix tee(1) command. Writers can be added and removed
// dynamically after creation.
//
// Each write is written to each listed writer, one at a time. If a listed
// writer returns an error, that overall write operation stops and returns the
// error; it does not continue down the list.
func New(ctx context.Context, writeTimeout time.Duration, writers ...io.Writer) *PMultiWriter {
	pmw := &PMultiWriter{
		writers:      writers,
		closed:       make(chan struct{}),
		ctx:          ctx,
		writeTimeout: writeTimeout,
	}
	return pmw
}

// SetOnEvict sets a callback that is called when a writer is evicted due to a write timeout.
// The callback receives the evicted writer and is called asynchronously.
func (pmw *PMultiWriter) SetOnEvict(fn func(io.Writer)) {
	pmw.Lock()
	defer pmw.Unlock()
	pmw.onEvict = fn
}

// evict removes a writer from the list and fires the OnEvict callback if set.
func (pmw *PMultiWriter) evict(w io.Writer) {
	pmw.Remove(w)
	pmw.RLock()
	fn := pmw.onEvict
	pmw.RUnlock()
	if fn != nil {
		fn(w)
	}
}

// Write writes some bytes to all the writers.
// If a writer takes longer than the configured timeout, it is evicted from the list.
// A write is considered successful if at least one writer succeeds. An error is only
// returned when ALL writers fail, preserving the stream for healthy clients.
func (pmw *PMultiWriter) Write(p []byte) (n int, err error) {
	pmw.RLock()
	// Copy the writers so we can release the lock
	writers := make([]io.Writer, len(pmw.writers))
	copy(writers, pmw.writers)
	pmw.RUnlock()

	if len(writers) == 0 {
		return len(p), nil
	}

	type writeResult struct {
		w   io.Writer
		err error
	}

	results := make(chan writeResult, len(writers))
	for _, w := range writers {
		go func(w io.Writer) {
			done := make(chan error, 1)
			go func() {
				_, err := w.Write(p)
				done <- err
			}()

			select {
			case err := <-done:
				results <- writeResult{w: w, err: err}
			case <-time.After(pmw.writeTimeout):
				slog.Warn("writer timed out, evicting", "writer", w)
				results <- writeResult{w: w, err: context.DeadlineExceeded}
				go pmw.evict(w)
			case <-pmw.closed:
				results <- writeResult{w: w, err: io.ErrClosedPipe}
			case <-pmw.ctx.Done():
				results <- writeResult{w: w, err: pmw.ctx.Err()}
			}
		}(w)
	}

	// Wait for all writes to finish or timeout.
	successCount := 0
	var writeErrors []error
	for range writers {
		select {
		case <-pmw.closed:
			slog.Debug("closed pmw", "pmw", pmw)
			return 0, io.ErrClosedPipe
		case res := <-results:
			if res.err != nil {
				slog.Debug("writer failed", "writer", res.w, "error", res.err)
				writeErrors = append(writeErrors, res.err)
			} else {
				successCount++
			}
		}
	}

	// Only return an error when ALL writers failed. If at least one succeeded,
	// the stream is healthy and should continue.
	if successCount == 0 && len(writeErrors) > 0 {
		return 0, PMultiWriterError{Errors: writeErrors, Writers: len(writers)}
	}

	return len(p), nil
}

// Add appends a writer to the list of writers this multiwriter writes to.
func (pmw *PMultiWriter) Add(w io.Writer) {
	pmw.Lock()
	defer pmw.Unlock()

	// Check if the writer is already in the list
	if !slices.Contains(pmw.writers, w) {
		pmw.writers = append(pmw.writers, w)
	}
}

// Remove will remove a previously added writer from the list of writers.
func (pmw *PMultiWriter) Remove(w io.Writer) {
	pmw.Lock()
	defer pmw.Unlock()

	var writers []io.Writer
	for _, ew := range pmw.writers {
		if ew != w {
			writers = append(writers, ew)
		}
	}
	pmw.writers = writers
}

// Closes all the writers in the list.
func (pmw *PMultiWriter) Close() error {
	pmw.RLock()
	defer pmw.RUnlock()

	var errors []error

	close(pmw.closed)
	for _, w := range pmw.writers {
		if c, ok := w.(io.Closer); ok {
			if err := c.Close(); err != nil {
				errors = append(errors, err)
			}
			slog.Debug("closed", "w", w)
		}
		/* there could be non-closeable writers. Rely on the channel to exit the goroutine */
	}
	if len(errors) > 0 {
		return PMultiWriterError{Errors: errors, Writers: len(pmw.writers)}
	}
	return nil
}
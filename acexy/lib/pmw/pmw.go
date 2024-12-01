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
	"fmt"
	"io"
	"strings"
	"sync"
)

// PMultiWriter is an implementation of an "io.Writer" that duplicates its writes
// to all the provided writers, similar to the Unix tee(1) command. Writers can be
// added and removed dynamically after creation. Each write is done in a separate
// goroutine, so the writes are done in parallel.
type PMultiWriter struct {
	sync.RWMutex
	writers []io.Writer
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
func New(writers ...io.Writer) *PMultiWriter {
	pmw := &PMultiWriter{writers: writers}
	return pmw
}

// Write writes some bytes to all the writers.
func (pmw *PMultiWriter) Write(p []byte) (n int, err error) {
	pmw.RLock()
	defer pmw.RUnlock()

	errs := make(chan error, len(pmw.writers))
	for _, w := range pmw.writers {
		go func(w io.Writer) {
			n, err := w.Write(p)
			// Forward the error and early return
			if err != nil || n < len(p) {
				if err == nil && n < len(p) {
					err = io.ErrShortWrite
				}
				errs <- err
			} else {
				errs <- nil
			}
		}(w)
	}

	// Wait for all writes to finish. If an error occurs, return it.
	errors := make([]error, 0)
	for range pmw.writers {
		if err := <-errs; err != nil {
			errors = append(errors, err)
		}
	}
	if len(errors) > 0 {
		return len(p), PMultiWriterError{Errors: errors, Writers: len(pmw.writers)}
	}

	return len(p), nil
}

// Add appends a writer to the list of writers this multiwriter writes to.
func (pmw *PMultiWriter) Add(w io.Writer) {
	pmw.Lock()
	defer pmw.Unlock()

	// Check if the writer is already in the list
	for _, ew := range pmw.writers {
		if ew == w {
			return
		}
	}
	pmw.writers = append(pmw.writers, w)
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
	pmw.Lock()
	defer pmw.Unlock()

	var errors []error
	for _, w := range pmw.writers {
		if c, ok := w.(io.Closer); ok {
			if err := c.Close(); err != nil {
				errors = append(errors, err)
			}
		}
	}
	if len(errors) > 0 {
		return PMultiWriterError{Errors: errors, Writers: len(pmw.writers)}
	}
	return nil
}

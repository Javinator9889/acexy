package acexy

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"time"
)

// ErrStreamStalled is returned by Copy when no data is received within EmptyTimeout.
var ErrStreamStalled = errors.New("stream stalled: no data received within timeout")

// Copier is an implementation that copies the data from the source to the destination.
// If no data is received within EmptyTimeout, the stream is considered stalled:
// the source is closed and ErrStreamStalled is returned so the caller can retry.
type Copier struct {
	// The destination to copy the data to.
	Destination io.Writer
	// The source to copy the data from.
	Source io.Reader
	// EmptyTimeout is the time without receiving any data after which the stream
	// is considered stalled and forcibly closed.
	EmptyTimeout time.Duration
	// The buffer size to use when copying the data.
	BufferSize int
	// StreamID is used for logging purposes only.
	StreamID string

	/**! Private Data */
	timer          *time.Timer
	bufferedWriter *bufio.Writer
	stalled        atomic.Bool
}

// Copy starts copying data from Source to Destination.
// Returns ErrStreamStalled if EmptyTimeout expires before data arrives.
func (c *Copier) Copy() error {
	c.bufferedWriter = bufio.NewWriterSize(c.Destination, c.BufferSize)
	c.timer = time.NewTimer(c.EmptyTimeout)
	c.stalled.Store(false)
	done := make(chan struct{})
	defer close(done)

	go func() {
		select {
		case <-done:
			slog.Debug("Done copying", "stream", c.StreamID)
		case <-c.timer.C:
			slog.Warn("Stream stalled, no data received",
				"stream", c.StreamID,
				"emptyTimeout", c.EmptyTimeout,
			)
			c.stalled.Store(true)
			c.bufferedWriter.Flush()
			if closer, ok := c.Source.(io.Closer); ok {
				closer.Close()
			}
		}
	}()

	_, err := io.Copy(c, c.Source)
	if c.stalled.Load() {
		return ErrStreamStalled
	}
	return err
}

// Write writes the data to the destination and resets the stall timer.
func (c *Copier) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.timer == nil || c.bufferedWriter == nil {
		return 0, io.ErrClosedPipe
	}
	c.timer.Reset(c.EmptyTimeout)
	return c.bufferedWriter.Write(p)
}

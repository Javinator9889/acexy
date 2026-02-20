package acexy

import (
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"time"
)

// ErrStreamStalled is returned by Copy when no data is received within EmptyTimeout.
var ErrStreamStalled = errors.New("stream stalled: no data received within timeout")

// Copier reads from Source and writes directly to Destination.
// It buffers reads from the source (AceStream) to smooth out network jitter,
// but writes directly to the destination so client disconnections are detected immediately.
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
	// BufferSize is the size of the read buffer used to read from the source.
	BufferSize int
	// StreamID is used for logging purposes only.
	StreamID string

	/**! Private Data */
	timer   *time.Timer
	stalled atomic.Bool
}

// Copy starts copying data from Source to Destination.
// Returns ErrStreamStalled if EmptyTimeout expires before data arrives.
func (c *Copier) Copy() error {
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
			if closer, ok := c.Source.(io.Closer); ok {
				closer.Close()
			}
		}
	}()

	buf := make([]byte, c.BufferSize)
	_, err := io.CopyBuffer(c, c.Source, buf)
	if c.stalled.Load() {
		return ErrStreamStalled
	}
	return err
}

// Write writes the data directly to the destination and resets the stall timer.
// Writing directly (without buffering) ensures client disconnections are detected
// on the next write rather than being absorbed by a buffer.
func (c *Copier) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.timer == nil {
		return 0, io.ErrClosedPipe
	}
	c.timer.Reset(c.EmptyTimeout)
	return c.Destination.Write(p)
}

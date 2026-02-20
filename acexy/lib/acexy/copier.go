package acexy

import (
	"bufio"
	"io"
	"log/slog"
	"time"
)

// Copier is an implementation that copies the data from the source to the destination.
// It has an empty timeout that is used to determine when the source is empty - this is,
// it has no more data to read after the timeout. If no data is received within EmptyTimeout,
// the stream is considered stalled and forcibly closed.
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
}

// Starts copying the data from the source to the destination.
func (c *Copier) Copy() error {
	c.bufferedWriter = bufio.NewWriterSize(c.Destination, c.BufferSize)
	c.timer = time.NewTimer(c.EmptyTimeout)
	done := make(chan struct{})
	defer close(done)

	go func() {
		select {
		case <-done:
			slog.Debug("Done copying", "stream", c.StreamID)
			return
		case <-c.timer.C:
			slog.Warn("Stream stalled, no data received, closing",
				"stream", c.StreamID,
				"emptyTimeout", c.EmptyTimeout,
			)
			c.bufferedWriter.Flush()
			if closer, ok := c.Source.(io.Closer); ok {
				closer.Close()
			}
			if closer, ok := c.Destination.(io.Closer); ok {
				closer.Close()
			}
		}
	}()

	_, err := io.Copy(c, c.Source)
	return err
}

// Write writes the data to the destination. It also resets the timer if there is data to write.
func (c *Copier) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.timer == nil || c.bufferedWriter == nil {
		return 0, io.ErrClosedPipe
	}
	// Reset the timer since we have data — stream is still alive
	c.timer.Reset(c.EmptyTimeout)
	// Write the data to the destination
	return c.bufferedWriter.Write(p)
}

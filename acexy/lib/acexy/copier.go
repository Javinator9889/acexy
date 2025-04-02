package acexy

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"syscall"
	"time"
)

// Copier is an implementation that copies the data from the source to the destination.
// It has an empty timeout that is used to determine when the source is empty - this is,
// it has no more data to read after the timeout.
type Copier struct {
	// The destination to copy the data to.
	Destination io.Writer
	// The source to copy the data from.
	Source io.Reader
	// The timeout to use when the source is empty.
	EmptyTimeout time.Duration
	// The buffer size to use when copying the data.
	BufferSize int

	/**! Private Data */
	timer          *time.Timer
	bufferedWriter *bufio.Writer
}

// Starts copying the data from the source to the destination.
func (c *Copier) Copy() error {
	c.bufferedWriter = bufio.NewWriterSize(c.Destination, c.BufferSize)
	c.timer = time.NewTimer(c.EmptyTimeout)
	defer c.timer.Stop()

	done := make(chan struct{})
	defer close(done)

	go func() {
		for {
			c.timer.Reset(c.EmptyTimeout)
			select {
			case <-done:
				slog.Debug("Done copying", "source", c.Source, "destination", c.Destination)
				return
			case <-c.timer.C:
				// On timeout, flush the buffer, and close both the source and the destination
				c.bufferedWriter.Flush()
				if closer, ok := c.Source.(io.Closer); ok {
					slog.Debug("Closing source", "source", c.Source)
					closer.Close()
				}
				if closer, ok := c.Destination.(io.Closer); ok {
					slog.Debug("Closing destination", "destination", c.Destination)
					closer.Close()
				}
				return
			}
		}
	}()

	// Copy data from source to destination with write error handling
	buf := make([]byte, c.BufferSize)
	for {
		n, err := c.Source.Read(buf)
		if err != nil {
			if err == io.EOF {
				return nil // Stream finished normally
			}
			slog.Error("Stream read error", "error", err)
			return err
		}

		_, writeErr := c.bufferedWriter.Write(buf[:n])
		if writeErr != nil {
			// Detect client disconnection (broken pipe / reset connection)
			if errors.Is(writeErr, syscall.EPIPE) || errors.Is(writeErr, syscall.ECONNRESET) {
				slog.Debug("Client disconnected, stopping stream")
				return nil
			}
			slog.Error("Write error", "error", writeErr)
			return writeErr
		}

		// Ensure data is sent to the client immediately
		if flushErr := c.bufferedWriter.Flush(); flushErr != nil {
			slog.Error("Flush error", "error", flushErr)
			return flushErr
		}
	}
}

// Write writes the data to the destination. It also resets the timer if there is data to write.
func (c *Copier) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.timer == nil || c.bufferedWriter == nil {
		return 0, io.ErrClosedPipe
	}
	// Reset the timer, since we have data to write
	c.timer.Reset(c.EmptyTimeout)
	// Write the data to the destination
	return c.bufferedWriter.Write(p)
}

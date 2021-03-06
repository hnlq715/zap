// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package zapcore

import (
	"bufio"
	"io"
	"sync"
	"time"

	"go.uber.org/multierr"
)

// A WriteSyncer is an io.Writer that can also flush any buffered data. Note
// that *os.File (and thus, os.Stderr and os.Stdout) implement WriteSyncer.
type WriteSyncer interface {
	io.Writer
	Sync() error
}

// AddSync converts an io.Writer to a WriteSyncer. It attempts to be
// intelligent: if the concrete type of the io.Writer implements WriteSyncer,
// we'll use the existing Sync method. If it doesn't, we'll add a no-op Sync.
func AddSync(w io.Writer) WriteSyncer {
	switch w := w.(type) {
	case WriteSyncer:
		return w
	default:
		return writerWrapper{w}
	}
}

type lockedWriteSyncer struct {
	sync.Mutex
	ws WriteSyncer
}

// Lock wraps a WriteSyncer in a mutex to make it safe for concurrent use. In
// particular, *os.Files must be locked before use.
func Lock(ws WriteSyncer) WriteSyncer {
	if _, ok := ws.(*lockedWriteSyncer); ok {
		// no need to layer on another lock
		return ws
	}
	return &lockedWriteSyncer{ws: ws}
}

func (s *lockedWriteSyncer) Write(bs []byte) (int, error) {
	s.Lock()
	n, err := s.ws.Write(bs)
	s.Unlock()
	return n, err
}

func (s *lockedWriteSyncer) Sync() error {
	s.Lock()
	err := s.ws.Sync()
	s.Unlock()
	return err
}

type bufferWriterSyncer struct {
	sync.Mutex

	stop         chan struct{}
	bufferWriter *bufio.Writer
	ticker       *time.Ticker
}

const (
	// _defaultBufferSize specifies the default size used by Buffer.
	_defaultBufferSize = 256 * 1024 // 256 kB

	// _defaultFlushInterval specifies the default flush interval for
	// Buffer.
	_defaultFlushInterval = 30 * time.Second
)

// Buffer wraps a WriteSyncer to buffer its output. The returned WriteSyncer
// flushes its output as the buffer fills up, or at the provided interval,
// whichever comes first.
//
// Call the returned function to finish using the WriteSyncer and flush
// remaining bytes.
//
//   func main() {
//     // ...
//     ws, closeWS := zapcore.Buffer(ws, 0, 0)
//     defer closeWS()
//     // ...
//   }
//
// The buffer size defaults to 256 kB if set to zero.
// The flush interval defaults to 30 seconds if set to zero.
func Buffer(ws WriteSyncer, bufferSize int, flushInterval time.Duration) (_ WriteSyncer, close func() error) {
	if bufferSize == 0 {
		bufferSize = _defaultBufferSize
	}

	if flushInterval == 0 {
		flushInterval = _defaultFlushInterval
	}

	bws := &bufferWriterSyncer{
		stop:         make(chan struct{}),
		bufferWriter: bufio.NewWriterSize(ws, bufferSize),
		ticker:       time.NewTicker(flushInterval),
	}

	go bws.flushLoop()

	return bws, bws.close
}

// Write writes log data into buffer syncer directly, multiple Write calls will be batched,
// and log data will be flushed to disk when the buffer is full or periodically.
func (s *bufferWriterSyncer) Write(bs []byte) (int, error) {
	s.Lock()
	defer s.Unlock()

	// To avoid partial writes from being flushed, we manually flush the existing buffer if:
	// * The current write doesn't fit into the buffer fully, and
	// * The buffer is not empty (since bufio will not split large writes when the buffer is empty)
	if len(bs) > s.bufferWriter.Available() && s.bufferWriter.Buffered() > 0 {
		if err := s.bufferWriter.Flush(); err != nil {
			return 0, err
		}
	}

	return s.bufferWriter.Write(bs)
}

// Sync flushes buffered log data into disk directly.
func (s *bufferWriterSyncer) Sync() error {
	s.Lock()
	defer s.Unlock()

	return s.bufferWriter.Flush()
}

// flushLoop flushes the buffer at the configured interval until Close is
// called.
func (s *bufferWriterSyncer) flushLoop() {
	for {
		select {
		case <-s.ticker.C:
			// we just simply ignore error here
			// because the underlying bufio writer stores any errors
			// and we return any error from Sync() as part of the close
			_ = s.Sync()
		case <-s.stop:
			return
		}
	}
}

// close closes the buffer, cleans up background goroutines, and flushes
// remaining, unwritten data.
func (s *bufferWriterSyncer) close() error {
	s.ticker.Stop()
	close(s.stop)
	return s.Sync()
}

type writerWrapper struct {
	io.Writer
}

func (w writerWrapper) Sync() error {
	return nil
}

type multiWriteSyncer []WriteSyncer

// NewMultiWriteSyncer creates a WriteSyncer that duplicates its writes
// and sync calls, much like io.MultiWriter.
func NewMultiWriteSyncer(ws ...WriteSyncer) WriteSyncer {
	if len(ws) == 1 {
		return ws[0]
	}
	// Copy to protect against https://github.com/golang/go/issues/7809
	return multiWriteSyncer(append([]WriteSyncer(nil), ws...))
}

// See https://golang.org/src/io/multi.go
// When not all underlying syncers write the same number of bytes,
// the smallest number is returned even though Write() is called on
// all of them.
func (ws multiWriteSyncer) Write(p []byte) (int, error) {
	var writeErr error
	nWritten := 0
	for _, w := range ws {
		n, err := w.Write(p)
		writeErr = multierr.Append(writeErr, err)
		if nWritten == 0 && n != 0 {
			nWritten = n
		} else if n < nWritten {
			nWritten = n
		}
	}
	return nWritten, writeErr
}

func (ws multiWriteSyncer) Sync() error {
	var err error
	for _, w := range ws {
		err = multierr.Append(err, w.Sync())
	}
	return err
}

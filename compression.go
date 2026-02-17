// Copyright 2017 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package websocket implements the WebSocket protocol defined in RFC 6455.
// This file contains the implementation of WebSocket compression using the
// DEFLATE algorithm as specified in RFC 7692.
package websocket

import (
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/klauspost/compress/flate"
)

// Compression level constants for WebSocket compression
const (
	// minCompressionLevel is the minimum compression level supported
	// flate.HuffmanOnly is not defined in Go < 1.6
	minCompressionLevel = -2

	// maxCompressionLevel is the maximum compression level supported
	maxCompressionLevel = flate.BestCompression

	// defaultCompressionLevel is the default compression level used if not specified
	defaultCompressionLevel = 1
)

// Pools for reusing flate readers and writers to improve performance
var (
	// flateWriterPools contains a pool for each compression level
	flateWriterPools [maxCompressionLevel - minCompressionLevel + 1]sync.Pool

	// flateReaderPool is a pool for reusing flate readers
	flateReaderPool = sync.Pool{New: func() interface{} {
		return flate.NewReader(nil)
	}}
)

// decompressNoContextTakeover creates a decompressor that implements the
// "per-message DEFLATE" WebSocket extension as described in RFC 7692.
// The context takeover is disabled, which means that each message is compressed
// independently without using the compression state from previous messages.
func decompressNoContextTakeover(r io.Reader) io.ReadCloser {
	// The tail bytes are necessary for the decompression to work correctly:
	// - First 4 bytes (\x00\x00\xff\xff) are added as specified in RFC 7692
	// - Second 5 bytes (\x01\x00\x00\xff\xff) add a final block to prevent
	//   unexpected EOF errors from the flate reader
	const tail = "\x00\x00\xff\xff\x01\x00\x00\xff\xff"

	// Try to reuse a reader from the pool
	fr, _ := flateReaderPool.Get().(io.ReadCloser)

	// Create a multi-reader that combines the input reader with the tail
	mr := io.MultiReader(r, strings.NewReader(tail))

	// Try to reset the reader to reuse it
	if err := fr.(flate.Resetter).Reset(mr, nil); err != nil {
		// Reset never fails, but handle error in case that changes in future versions
		fr = flate.NewReader(mr)
	}

	// Wrap the reader to handle proper cleanup when closed
	return &flateReadWrapper{fr}
}

// isValidCompressionLevel checks if the given compression level is within
// the valid range defined by minCompressionLevel and maxCompressionLevel.
func isValidCompressionLevel(level int) bool {
	return minCompressionLevel <= level && level <= maxCompressionLevel
}

// compressNoContextTakeover creates a compressor that implements the
// "per-message DEFLATE" WebSocket extension as described in RFC 7692.
// The context takeover is disabled, which means that each message is compressed
// independently without using the compression state from previous messages.
func compressNoContextTakeover(w io.WriteCloser, level int) io.WriteCloser {
	// Get the pool for the specified compression level
	p := &flateWriterPools[level-minCompressionLevel]

	// Create a truncWriter that will discard the last 4 bytes
	tw := &truncWriter{w: w}

	// Try to get a writer from the pool
	fw, _ := p.Get().(*flate.Writer)

	// Create a new writer if none was available in the pool
	if fw == nil {
		fw, _ = flate.NewWriter(tw, level)
	} else {
		// Reset the writer to reuse it
		fw.Reset(tw)
	}

	// Wrap the writer to handle proper cleanup when closed
	return &flateWriteWrapper{fw: fw, tw: tw, p: p}
}

// truncWriter is an io.Writer that writes all but the last four bytes of the
// stream to another io.Writer. This is necessary for WebSocket compression
// because the DEFLATE algorithm adds 4 bytes of trailer data that must be
// removed as specified in RFC 7692.
type truncWriter struct {
	w io.WriteCloser // The underlying writer
	n int            // Number of bytes currently in the buffer
	p [4]byte        // Buffer for the last 4 bytes
}

// Write implements the io.Writer interface for truncWriter.
// It buffers the last 4 bytes of the stream and writes everything else
// to the underlying writer.
func (w *truncWriter) Write(p []byte) (int, error) {
	n := 0

	// First, fill the buffer if it's not full yet
	if w.n < len(w.p) {
		// Copy as many bytes as possible to the buffer
		n = copy(w.p[w.n:], p)
		p = p[n:]
		w.n += n

		// If we've consumed all input, we're done
		if len(p) == 0 {
			return n, nil
		}
	}

	// Calculate how many bytes to keep in the buffer after this write
	m := len(p)
	if m > len(w.p) {
		m = len(w.p)
	}

	// Write the bytes from the buffer that will be replaced
	if nn, err := w.w.Write(w.p[:m]); err != nil {
		return n + nn, err
	}

	// Shift the remaining bytes in the buffer
	copy(w.p[:], w.p[m:])

	// Copy the last m bytes from p to the end of the buffer
	copy(w.p[len(w.p)-m:], p[len(p)-m:])

	// Write all but the last m bytes from p
	nn, err := w.w.Write(p[:len(p)-m])
	return n + nn, err
}

// flateWriteWrapper is a wrapper around a flate.Writer that handles
// proper cleanup when closed. It implements the io.WriteCloser interface.
type flateWriteWrapper struct {
	fw *flate.Writer // The flate writer for compression
	tw *truncWriter  // The truncWriter that discards the last 4 bytes
	p  *sync.Pool    // The pool to return the flate writer to when closed
}

// Write implements the io.Writer interface for flateWriteWrapper.
// It writes the data to the underlying flate writer for compression.
func (w *flateWriteWrapper) Write(p []byte) (int, error) {
	if w.fw == nil {
		return 0, errWriteClosed
	}
	return w.fw.Write(p)
}

// Close implements the io.Closer interface for flateWriteWrapper.
// It flushes the flate writer, returns it to the pool, and closes the underlying writer.
func (w *flateWriteWrapper) Close() error {
	if w.fw == nil {
		return errWriteClosed
	}

	// Flush any remaining data in the flate writer
	err1 := w.fw.Flush()

	// Return the flate writer to the pool for reuse
	w.p.Put(w.fw)
	w.fw = nil

	// Verify that the last 4 bytes are as expected (0x00, 0x00, 0xff, 0xff)
	// This is required by the WebSocket compression specification
	if w.tw.p != [4]byte{0, 0, 0xff, 0xff} {
		return errors.New("websocket: internal error, unexpected bytes at end of flate stream")
	}

	// Close the underlying writer
	err2 := w.tw.w.Close()

	// Return the first error encountered, if any
	if err1 != nil {
		return err1
	}
	return err2
}

// flateReadWrapper is a wrapper around a flate.Reader that handles
// proper cleanup when closed. It implements the io.ReadCloser interface.
type flateReadWrapper struct {
	fr io.ReadCloser // The flate reader for decompression
}

// Read implements the io.Reader interface for flateReadWrapper.
// It reads decompressed data from the underlying flate reader.
func (r *flateReadWrapper) Read(p []byte) (int, error) {
	if r.fr == nil {
		return 0, io.ErrClosedPipe
	}

	n, err := r.fr.Read(p)

	if err == io.EOF {
		// Preemptively place the reader back in the pool. This helps with
		// scenarios where the application does not call NextReader() soon after
		// this final read.
		r.Close()
	}

	return n, err
}

// Close implements the io.Closer interface for flateReadWrapper.
// It closes the underlying flate reader and returns it to the pool.
func (r *flateReadWrapper) Close() error {
	if r.fr == nil {
		return io.ErrClosedPipe
	}

	// Close the flate reader
	err := r.fr.Close()

	// Return the flate reader to the pool for reuse
	flateReaderPool.Put(r.fr)
	r.fr = nil

	return err
}

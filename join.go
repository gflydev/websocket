// Copyright 2019 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"fmt"
	"io"
	"strings"
)

// JoinMessages concatenates received WebSocket messages to create a single io.Reader.
// It reads messages sequentially from the provided WebSocket connection.
//
// Parameters:
//   - c: The WebSocket connection to read messages from
//   - term: A string to append after each message (can be empty for no separator)
//
// The returned reader does not support concurrent calls to the Read method.
// Each message is read completely before moving to the next message.
// When a message is fully read, the next call to Read will fetch a new message.
func JoinMessages(c *Conn, term string) io.Reader {
	if c == nil {
		// Return a reader that will immediately return an error
		return &errorReader{err: fmt.Errorf("websocket: nil connection provided to JoinMessages")}
	}
	return &joinReader{
		conn:      c,
		separator: term,
	}
}

// errorReader is a simple io.Reader that always returns the same error
type errorReader struct {
	err error
}

func (r *errorReader) Read(p []byte) (int, error) {
	return 0, r.err
}

// joinReader implements io.Reader by concatenating multiple WebSocket messages
type joinReader struct {
	conn      *Conn     // WebSocket connection to read from
	separator string    // String to append after each message
	reader    io.Reader // Current message reader
}

// Read implements the io.Reader interface
func (jr *joinReader) Read(p []byte) (int, error) {
	// If we don't have a current reader, get the next message
	if jr.reader == nil {
		var err error
		_, jr.reader, err = jr.conn.NextReader()
		if err != nil {
			return 0, fmt.Errorf("websocket: failed to get next message: %w", err)
		}

		// If a separator is specified, append it to the message
		if jr.separator != "" {
			jr.reader = io.MultiReader(jr.reader, strings.NewReader(jr.separator))
		}
	}

	// Read from the current message
	bytesRead, err := jr.reader.Read(p)

	// If we've reached the end of the current message,
	// clear the reader so we'll get a new message on the next Read call
	if err == io.EOF {
		jr.reader = nil
		err = nil // Convert EOF to nil to indicate more data might be available
	}

	return bytesRead, err
}

// Copyright 2016 The Gorilla WebSocket Authors. All rights reserved.  Use of
// this source code is governed by a BSD-style license that can be found in the
// LICENSE file.

//go:build !appengine

// Package websocket implements the WebSocket protocol defined in RFC 6455.
package websocket

import "unsafe"

// wordSize represents the size of a machine word (4 or 8 bytes depending on the platform).
// This is used for optimizing the masking operation by processing multiple bytes at once.
const wordSize = int(unsafe.Sizeof(uintptr(0)))

// maskBytes applies the WebSocket masking operation to a byte slice.
//
// WebSocket protocol requires all frames sent from client to server to be masked with
// a 4-byte key. This function efficiently applies the XOR masking operation to the
// provided byte slice using the given key.
//
// Parameters:
//   - key: The 4-byte masking key
//   - pos: The starting position in the masking key (0-3)
//   - b: The byte slice to mask (will be modified in-place)
//
// Returns:
//   - The new position in the masking key (0-3) for continued masking operations
//
// The implementation uses several optimizations:
//  1. For small buffers, it masks one byte at a time
//  2. For larger buffers, it aligns to word boundaries and masks one word at a time
//  3. For any remaining bytes, it masks one byte at a time
func maskBytes(key [4]byte, pos int, b []byte) int {
	// For small buffers, use the simple byte-by-byte approach
	if len(b) < 2*wordSize {
		return maskSmallBuffer(key, pos, b)
	}

	// Process the buffer in three steps:
	// 1. Align to word boundary
	pos, b = alignToWordBoundary(key, pos, b)

	// 2. Process aligned words (the bulk of the data)
	pos, b = maskAlignedWords(key, pos, b)

	// 3. Process remaining bytes
	return maskRemainingBytes(key, pos, b)
}

// maskSmallBuffer applies masking to small buffers byte by byte.
// This is more efficient for small buffers than the word-by-word approach.
func maskSmallBuffer(key [4]byte, pos int, b []byte) int {
	for i := range b {
		b[i] ^= key[pos&3]
		pos++
	}
	return pos & 3
}

// alignToWordBoundary masks bytes until the buffer is aligned to a word boundary.
// This prepares the buffer for more efficient word-by-word processing.
func alignToWordBoundary(key [4]byte, pos int, b []byte) (int, []byte) {
	if n := int(uintptr(unsafe.Pointer(&b[0]))) % wordSize; n != 0 {
		n = wordSize - n
		for i := range b[:n] {
			b[i] ^= key[pos&3]
			pos++
		}
		return pos, b[n:]
	}
	return pos, b
}

// maskAlignedWords processes the buffer one word at a time for maximum efficiency.
// This is the core optimization that significantly improves masking performance.
func maskAlignedWords(key [4]byte, pos int, b []byte) (int, []byte) {
	// Create a word-sized key by repeating the masking key pattern
	var k [wordSize]byte
	for i := range k {
		k[i] = key[(pos+i)&3]
	}

	// Convert the byte array to a word value for efficient XOR operations
	kw := *(*uintptr)(unsafe.Pointer(&k))

	// Calculate how many complete words we can process
	n := (len(b) / wordSize) * wordSize

	// Process one word at a time using unsafe pointer arithmetic
	for i := 0; i < n; i += wordSize {
		// XOR the word in the buffer with our word-sized key
		*(*uintptr)(unsafe.Pointer(uintptr(unsafe.Pointer(&b[0])) + uintptr(i))) ^= kw
	}

	// Update position and return the remaining unprocessed bytes
	pos = (pos + n) & 3
	return pos, b[n:]
}

// maskRemainingBytes processes any remaining bytes that couldn't be processed as complete words.
func maskRemainingBytes(key [4]byte, pos int, b []byte) int {
	for i := range b {
		b[i] ^= key[pos&3]
		pos++
	}
	return pos & 3
}

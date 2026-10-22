// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gzip

import (
	"compress/flate"
	"fmt"
	"hash/crc32"
	"io"
	"sync"
)

// memberNXMinSize is the smallest input worth offloading to the NX
// accelerator; below it the job setup cost outweighs the benefit.
const memberNXMinSize = 32 << 10

// memberWriters caches software writers for the fallback path, indexed by
// level-HuffmanOnly.
var memberWriters [BestCompression - HuffmanOnly + 1]sync.Pool

// appendWriter is an [io.Writer] that appends to a byte slice.
type appendWriter struct{ b []byte }

func (w *appendWriter) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}

// AppendMember appends src to dst as one complete gzip member (RFC 1952,
// section 2.3) and returns the extended slice. Independent members can be
// concatenated: a reader decodes them as a single stream, which makes
// AppendMember suitable for compressing large inputs chunk by chunk, possibly
// in parallel.
//
// On a POWER10 system with an NX-GZIP accelerator (see [flate.IsNXAvailable]),
// AppendMember compresses on the accelerator when src is at least 32 KB and
// small enough for a single hardware job (1 MiB on PowerVM). In every other
// case, including when the accelerator declines the job (for example,
// incompressible data), it falls back to software compression at the given
// level, which is as for [NewWriterLevel].
//
// The accelerator uses fixed Huffman codes, so its output is typically
// 10-40% larger than software output at the default level, and it does not
// depend on level. Callers that need the same bytes on every machine must not
// use AppendMember. [NewWriter] and [NewWriterLevel] never use the accelerator.
func AppendMember(dst, src []byte, level int) ([]byte, error) {
	if level < HuffmanOnly || level > BestCompression {
		return dst, fmt.Errorf("gzip: invalid compression level: %d", level)
	}
	if len(src) >= memberNXMinSize && flate.IsNXAvailable() {
		if out, ok := appendMemberNX(dst, src); ok {
			return out, nil
		}
	}
	return appendMemberSoftware(dst, src, level)
}

// appendMemberNX tries to compress src on the accelerator. It reports false,
// leaving dst's contents unchanged, if the accelerator did not produce a
// stream.
func appendMemberNX(dst, src []byte) ([]byte, bool) {
	const header, trailer = 10, 8
	// Fixed Huffman coding can expand incompressible data by about 12.5%;
	// flate.NXCompress documents the extra space it needs for the terminator.
	scratch := len(src) + len(src)/4 + 4096

	start := len(dst)
	if cap(dst)-start < header+scratch+trailer {
		grown := make([]byte, start, start+header+scratch+trailer)
		copy(grown, dst)
		dst = grown
	}
	buf := dst[start : start+header+scratch+trailer]

	buf[0], buf[1], buf[2] = gzipID1, gzipID2, gzipDeflate
	clear(buf[3:header])
	buf[9] = 255 // OS unknown, as in Writer.Header

	n, err := flate.NXCompress(src, buf[header:header+scratch])
	if err != nil {
		return dst[:start], false
	}
	t := buf[header+n : header+n+trailer]
	le.PutUint32(t[0:4], crc32.ChecksumIEEE(src))
	le.PutUint32(t[4:8], uint32(len(src)))
	return dst[:start+header+n+trailer], true
}

func appendMemberSoftware(dst, src []byte, level int) ([]byte, error) {
	aw := &appendWriter{b: dst}
	pool := &memberWriters[level-HuffmanOnly]
	z, _ := pool.Get().(*Writer)
	if z == nil {
		var err error
		if z, err = NewWriterLevel(aw, level); err != nil {
			return dst, err
		}
	} else {
		z.Reset(aw)
	}
	defer func() {
		z.Reset(io.Discard) // do not retain dst while pooled
		pool.Put(z)
	}()

	// Write, even for empty src, emits the header; Close then emits the
	// trailer.
	if _, err := z.Write(src); err != nil {
		return dst, err
	}
	if err := z.Close(); err != nil {
		return dst, err
	}
	return aw.b, nil
}

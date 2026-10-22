// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package flate

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"testing"
)

func TestNXAvailable(t *testing.T) {
	// Verify that querying NX status does not panic on any platform.
	t.Logf("NX hardware acceleration available: %v", IsNXAvailable())
}

func TestNXCompressRoundTrip(t *testing.T) {
	if !IsNXAvailable() {
		t.Skip("NX hardware acceleration not available")
	}

	text := func(n int) []byte {
		words := []string{"the ", "quick ", "brown ", "fox ", "jumps ", "etcd ", "kubernetes "}
		r := rand.New(rand.NewSource(1))
		b := make([]byte, 0, n+16)
		for len(b) < n {
			b = append(b, words[r.Intn(len(words))]...)
		}
		return b[:n]
	}
	random := func(n int) []byte {
		b := make([]byte, n)
		rand.New(rand.NewSource(2)).Read(b)
		return b
	}

	for _, size := range []int{1, 100, 4095, 32 << 10, 100<<10 + 3, 1 << 20} {
		for name, gen := range map[string]func(int) []byte{"text": text, "random": random} {
			src := gen(size)
			dst := make([]byte, size+size/4+4096)
			n, err := NXCompress(src, dst)
			if err != nil {
				// Falling back to software is legitimate for incompressible
				// data; it must never be reported as success.
				t.Logf("size=%d %s: NXCompress: %v", size, name, err)
				continue
			}
			// Append a sentinel: the decoder must consume exactly the
			// stream, so the output can be framed (e.g. by a gzip trailer).
			const sentinel = "\xaa\xbb\xcc\xdd"
			br := bytes.NewReader(append(append([]byte(nil), dst[:n]...), sentinel...))
			got, err := io.ReadAll(NewReader(br))
			if err != nil {
				t.Fatalf("size=%d %s: decompress: %v", size, name, err)
			}
			if !bytes.Equal(got, src) {
				t.Fatalf("size=%d %s: round trip mismatch", size, name)
			}
			if br.Len() != len(sentinel) {
				t.Fatalf("size=%d %s: decoder left %d trailing bytes, want %d (stream not terminated by its final block)",
					size, name, br.Len(), len(sentinel))
			}
		}
	}
}

func TestNXCompressUnavailableError(t *testing.T) {
	if IsNXAvailable() {
		t.Skip("NX hardware acceleration is available")
	}
	_, err := NXCompress(make([]byte, 64), make([]byte, 256))
	if !errors.Is(err, errNXUnavailable) {
		t.Fatalf("NXCompress with NX unavailable: got %v, want errNXUnavailable", err)
	}
}

func TestNXCompressRejectsEmpty(t *testing.T) {
	if _, err := NXCompress(nil, make([]byte, 16)); err == nil {
		t.Fatal("NXCompress(nil, dst) succeeded")
	}
	if _, err := NXCompress(make([]byte, 16), nil); err == nil {
		t.Fatal("NXCompress(src, nil) succeeded")
	}
}

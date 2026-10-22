// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package flate

import (
	"bytes"
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
			got, err := io.ReadAll(NewReader(bytes.NewReader(dst[:n])))
			if err != nil {
				t.Fatalf("size=%d %s: decompress: %v", size, name, err)
			}
			if !bytes.Equal(got, src) {
				t.Fatalf("size=%d %s: round trip mismatch", size, name)
			}
		}
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

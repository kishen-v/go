// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gzip

import (
	"bytes"
	"compress/flate"
	"io"
	"math/rand"
	"testing"
)

func memberTestData(kind string, n int) []byte {
	b := make([]byte, n)
	switch kind {
	case "random":
		rand.New(rand.NewSource(1)).Read(b)
	case "text":
		words := []string{"alpha ", "beta ", "gamma ", "kubernetes ", "etcd ", "\n"}
		r := rand.New(rand.NewSource(2))
		b = b[:0]
		for len(b) < n {
			b = append(b, words[r.Intn(len(words))]...)
		}
		b = b[:n]
	}
	return b
}

func TestAppendMember(t *testing.T) {
	t.Logf("NX available: %v", flate.IsNXAvailable())
	for _, kind := range []string{"zero", "text", "random"} {
		for _, n := range []int{0, 1, 1000, 32<<10 - 1, 32 << 10, 100<<10 + 7, 1 << 20, 3<<20 + 5} {
			src := memberTestData(kind, n)
			prefix := []byte("prefix")
			got, err := AppendMember(append([]byte(nil), prefix...), src, DefaultCompression)
			if err != nil {
				t.Fatalf("%s/%d: %v", kind, n, err)
			}
			if !bytes.HasPrefix(got, prefix) {
				t.Fatalf("%s/%d: existing dst contents were modified", kind, n)
			}
			zr, err := NewReader(bytes.NewReader(got[len(prefix):]))
			if err != nil {
				t.Fatalf("%s/%d: NewReader: %v", kind, n, err)
			}
			out, err := io.ReadAll(zr)
			if err != nil {
				t.Fatalf("%s/%d: decompress: %v", kind, n, err)
			}
			if !bytes.Equal(out, src) {
				t.Fatalf("%s/%d: round trip mismatch", kind, n)
			}
		}
	}
}

// Members must concatenate into one valid gzip stream.
func TestAppendMemberConcat(t *testing.T) {
	var want, z []byte
	for i, kind := range []string{"text", "random", "zero", "text"} {
		src := memberTestData(kind, 200<<10+i)
		want = append(want, src...)
		var err error
		if z, err = AppendMember(z, src, BestSpeed); err != nil {
			t.Fatal(err)
		}
	}
	zr, err := NewReader(bytes.NewReader(z))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("concatenated members did not round trip")
	}
}

func TestAppendMemberBadLevel(t *testing.T) {
	for _, level := range []int{-3, 10} {
		if _, err := AppendMember(nil, []byte("x"), level); err == nil {
			t.Errorf("level %d: no error", level)
		}
	}
}

func TestAppendMemberAllLevels(t *testing.T) {
	src := memberTestData("text", 64<<10)
	for level := HuffmanOnly; level <= BestCompression; level++ {
		z, err := AppendMember(nil, src, level)
		if err != nil {
			t.Fatalf("level %d: %v", level, err)
		}
		zr, err := NewReader(bytes.NewReader(z))
		if err != nil {
			t.Fatalf("level %d: %v", level, err)
		}
		if got, err := io.ReadAll(zr); err != nil || !bytes.Equal(got, src) {
			t.Fatalf("level %d: round trip failed (%v)", level, err)
		}
	}
}

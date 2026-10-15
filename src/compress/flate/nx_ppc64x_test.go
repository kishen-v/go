// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build (ppc64 || ppc64le) && linux

package flate

import (
	"testing"
	"unsafe"
)

func TestNXStructSizesPPC64(t *testing.T) {
	if sz := unsafe.Sizeof(nxCRB{}); sz != 128 {
		t.Fatalf("expected nxCRB size to be 128 bytes, got %d", sz)
	}

	if sz := unsafe.Sizeof(nxCSB{}); sz != 128 {
		t.Fatalf("expected nxCSB size to be 128 bytes, got %d", sz)
	}

	if sz := unsafe.Sizeof(nxDDE{}); sz != 16 {
		t.Fatalf("expected nxDDE size to be 16 bytes, got %d", sz)
	}

	if sz := unsafe.Sizeof(vasTxWinOpenAttr{}); sz != 64 {
		t.Fatalf("expected vasTxWinOpenAttr size to be 64 bytes, got %d", sz)
	}
}

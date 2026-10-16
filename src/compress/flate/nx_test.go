// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package flate

import (
	"testing"
)

func TestNXAvailable(t *testing.T) {
	// Verify that querying NX status does not panic on any platform.
	avail := isNXAvailable()
	t.Logf("NX hardware acceleration available: %v", avail)

	if avail {
		src := []byte("Hello Power10 NX hardware acceleration! 1234567890 1234567890 1234567890 1234567890")
		dst := make([]byte, len(src)+1024)
		n, err := nxCompress(src, dst)
		t.Logf("nxCompress result: n=%d, err=%v", n, err)
	}
}

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
}

// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !((ppc64 || ppc64le) && linux)

package flate

const nxMinPayloadSize = 32 * 1024

func isNXAvailable() bool {
	return false
}

func (w *Writer) writeNX(data []byte) (int, error) {
	return w.d.write(data)
}

// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(ppc64le && linux)

package flate

import "errors"

const nxMinPayloadSize = 32 * 1024

func isNXAvailable() bool {
	return false
}

// IsNXAvailable returns false on platforms without POWER10 NX support.
func IsNXAvailable() bool {
	return false
}

// NXCompress returns an error on platforms without POWER10 NX support.
func NXCompress(src, dst []byte) (int, error) {
	return 0, errNXUnavailable
}

var errNXUnavailable = errors.New("nx: not supported on this platform")

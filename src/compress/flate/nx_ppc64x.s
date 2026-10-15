// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build (ppc64 || ppc64le) && linux

#include "textflag.h"

// func nxCopyPaste(pasteAddr uintptr, crb *nxCRB) uint32
// Dispatches a 128-byte CRB to the NX coprocessor MMIO paste window.
//
// Arguments:
//   pasteAddr +0(FP) - VAS MMIO paste window address (uintptr)
//   crb       +8(FP) - Pointer to 128-byte aligned CRB (*nxCRB)
// Return:
//   ret      +16(FP) - CR0 condition register result (uint32)
TEXT ·nxCopyPaste(SB), NOSPLIT|NOFRAME, $0-24
	MOVD	pasteAddr+0(FP), R4
	MOVD	crb+8(FP), R5
	MOVD	$0, R3

	// Copy 128 bytes from CRB at (R3+R5) into CPU copy buffer
	COPY	R3, R5

	// Paste the buffer to MMIO address at (R3+R4)
	PASTECC	R3, R4

	// Read Condition Register to capture paste. status (CR0 EQ indicates success)
	MOVW	CR, R6
	MOVW	R6, ret+16(FP)
	RET

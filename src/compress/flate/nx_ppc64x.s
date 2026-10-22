// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build ppc64le && linux

#include "textflag.h"

// func nxTouch(p *byte, write bool)
// Loads *p and, if write is set, stores the same value back, so that the page
// is resident (and writable) before NX is given its address.
TEXT ·nxTouch(SB), NOSPLIT|NOFRAME, $0-9
	MOVD	p+0(FP), R3
	MOVBZ	(R3), R4
	MOVBZ	write+8(FP), R5
	CMP	R5, $0
	BEQ	done
	MOVB	R4, (R3)
done:
	RET

// func nxCopyPaste(pasteAddr uintptr, crb *nxCRB) uint32
// Dispatches a 128-byte CRB to the NX coprocessor MMIO paste window.
//
// Arguments:
//   pasteAddr +0(FP) - VAS MMIO paste window address (uintptr)
//   crb       +8(FP) - Pointer to 128-byte aligned CRB (*nxCRB)
// Return:
//   ret      +16(FP) - CR0 condition register result (uint32)
TEXT ·nxCopyPaste(SB), NOSPLIT|NOFRAME, $0-20
	MOVD	pasteAddr+0(FP), R4
	MOVD	crb+8(FP), R5
	MOVD	$0, R3

	// Power ISA memory barrier before copy
	SYNC

	// Copy 128 bytes from CRB at (R3+R5) into CPU copy buffer
	COPY	R3, R5

	// Power ISA memory barrier before paste
	SYNC

	// Paste the buffer to MMIO address at (R3+R4)
	PASTECC	R3, R4

	// Power ISA memory barrier after paste
	SYNC

	// Read Condition Register to capture paste. status (CR0 EQ indicates success)
	MOVW	CR, R6
	MOVW	R6, ret+16(FP)
	RET

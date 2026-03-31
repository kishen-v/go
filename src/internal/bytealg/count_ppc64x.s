Git`// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build ppc64le || ppc64

#include "go_asm.h"
#include "textflag.h"

TEXT ·Count<ABIInternal>(SB),NOSPLIT|NOFRAME,$0-40
	// R3 = byte array pointer
	// R4 = length
	// R6 = byte to count
	MTVRD	R6, V1		// move compare byte
	MOVD	R6, R5
	VSPLTB	$7, V1, V1	// replicate byte across V1
	BR	countbytebody<>(SB)

TEXT ·CountString<ABIInternal>(SB), NOSPLIT|NOFRAME, $0-32
	// R3 = byte array pointer
	// R4 = length
	// R5 = byte to count
	MTVRD	R5, V1		// move compare byte
	VSPLTB	$7, V1, V1	// replicate byte across V1
	BR	countbytebody<>(SB)

// R3: addr of string
// R4: len of string
// R5: byte to count
// V1: byte to count, splatted.
// On exit:
// R3: return value
TEXT countbytebody<>(SB), NOSPLIT|NOFRAME, $0-0
	MOVD	$0, R18 // byte count

#ifndef GOPPC64_power10
	RLDIMI	$8, R5, $48, R5
	RLDIMI	$16, R5, $32, R5
	RLDIMI	$32, R5, $0, R5	// fill reg with the byte to count
#endif

	CMPU	R4, $32		// Check if it's a small string (<32 bytes)
	BLT	tail		// Jump to the small string case

	CMP	R4, $128, CR1	// Check if we can use 128-byte loop (full cache line)
	BLT	CR1, loop32_setup

	// Setup for 128-byte loop (full cache line on POWER8/9/10)
	MOVD	$16, R11
	MOVD	$32, R12
	MOVD	$48, R14
	MOVD	$64, R15
	MOVD	$80, R16
	MOVD	$96, R17
	MOVD	$112, R19
	SRD	$7, R4, R20	// R20 = len / 128
	MOVD	R20, CTR
	XXLXOR	V4, V4, V4
	XXLXOR	V5, V5, V5
	XXLXOR	V6, V6, V6
	XXLXOR	V7, V7, V7

	PCALIGN	$16
loop128:
	// Process full 128-byte cache line with 8 vector loads
	LXVD2X	(R0)(R3), V0	// R3+0: Load bytes 0-15 (R0 is always 0)
	LXVD2X	(R11)(R3), V2	// R3+16: Load bytes 16-31
	LXVD2X	(R12)(R3), V8	// R3+32: Load bytes 32-47
	LXVD2X	(R14)(R3), V9	// R3+48: Load bytes 48-63
	VCMPEQUB V0, V1, V0
	VCMPEQUB V2, V1, V2
	VCMPEQUB V8, V1, V8
	VCMPEQUB V9, V1, V9
	VPOPCNTD V0, V0
	VPOPCNTD V2, V2
	VPOPCNTD V8, V8
	VPOPCNTD V9, V9
	VADDUDM	V0, V4, V4
	VADDUDM	V2, V5, V5
	VADDUDM	V8, V6, V6
	VADDUDM	V9, V7, V7

	LXVD2X	(R15)(R3), V0	// R3+64: Load bytes 64-79
	LXVD2X	(R16)(R3), V2	// R3+80: Load bytes 80-95
	LXVD2X	(R17)(R3), V8	// R3+96: Load bytes 96-111
	LXVD2X	(R19)(R3), V9	// R3+112: Load bytes 112-127
	VCMPEQUB V0, V1, V0
	VCMPEQUB V2, V1, V2
	VCMPEQUB V8, V1, V8
	VCMPEQUB V9, V1, V9
	VPOPCNTD V0, V0
	VPOPCNTD V2, V2
	VPOPCNTD V8, V8
	VPOPCNTD V9, V9
	VADDUDM	V0, V4, V4
	VADDUDM	V2, V5, V5
	VADDUDM	V8, V6, V6
	VADDUDM	V9, V7, V7

	ADD	$128, R3
	BDNZ	loop128

	// Combine all four accumulators
	VADDUDM	V4, V5, V4
	VADDUDM	V6, V7, V6
	VADDUDM	V4, V6, V5
	MFVSRD	V5, R18
	VSLDOI	$8, V5, V5, V5
	MFVSRD	V5, R21
	ADD	R21, R18, R18
	ANDCC	$127, R4, R4	// Remaining bytes after 128-byte chunks
	BEQ	tail_0
	CMP	R4, $32
	BLT	tail
	// Fall through to handle remaining 32-127 bytes

loop32_setup:
	SRD	$5, R4, R20
	MOVD	R20, CTR
	MOVD	$16, R21
	XXLXOR	V4, V4, V4
	XXLXOR	V5, V5, V5

	PCALIGN	$16
cmploop:
	LXVD2X	(R0)(R3), V0	// Count 32B per loop with two vector accumulators.
	LXVD2X	(R21)(R3), V2
	VCMPEQUB V2, V1, V2
	VCMPEQUB V0, V1, V0
	VPOPCNTD V2, V2		// A match is 0xFF or 0. Count the bits into doubleword buckets.
	VPOPCNTD V0, V0
	VADDUDM	V0, V4, V4	// Accumulate the popcounts. They are 8x the count.
	VADDUDM	V2, V5, V5	// The count will be fixed up afterwards.
	ADD	$32, R3
	BDNZ	cmploop

	VADDUDM	V4, V5, V5
	MFVSRD	V5, R21
	VSLDOI	$8, V5, V5, V5
	MFVSRD	V5, R22
	ADD	R22, R21, R21
	ADD	R21, R18, R18
	ANDCC	$31, R4, R4
	// Skip the tail processing if no bytes remaining.
	BEQ	tail_0

#ifdef GOPPC64_power10
	SRD	$3, R18, R18	// Fix the vector loop count before counting the tail on P10.

tail:	// Count the last 0 - 31 bytes.
	CMP	R4, $16
	BLE	small_tail_p10
	LXV	0(R3), V0
	VCMPEQUB V0, V1, V0
	VCNTMBB	V0, $1, R14	// Sum the value of bit 0 of each byte of the compare into R14.
	SRD	$56, R14, R14	// The result of VCNTMBB is shifted. Unshift it.
	ADD	R14, R18, R18
	ADD	$16, R3, R3
	ANDCC	$15, R4, R4

small_tail_p10:
	SLD	$56, R4, R6
	LXVLL	R3, R6, V0
	VCMPEQUB V0, V1, V0
	VCLRRB	V0, R4, V0	// If <16B being compared, clear matches of the 16-R4 bytes.
	VCNTMBB	V0, $1, R14	// Sum the value of bit 0 of each byte of the compare into R14.
	SRD	$56, R14, R14	// The result of VCNTMBB is shifted. Unshift it.
	ADD	R14, R18, R3
	RET

#else
tail:	// Count the last 0 - 31 bytes.
	CMP	R4, $16
	BLT	tail_8
	MOVD	(R3), R12
	MOVD	8(R3), R14
	CMPB	R12, R5, R12
	CMPB	R14, R5, R14
	POPCNTD	R12, R12
	POPCNTD	R14, R14
	ADD	R12, R18, R18
	ADD	R14, R18, R18
	ADD	$16, R3, R3
	ADD	$-16, R4, R4

tail_8:	// Count the remaining 0 - 15 bytes.
	CMP	R4, $8
	BLT	tail_4
	MOVD	(R3), R12
	CMPB	R12, R5, R12
	POPCNTD	R12, R12
	ADD	R12, R18, R18
	ADD	$8, R3, R3
	ADD	$-8, R4, R4

tail_4:	// Count the remaining 0 - 7 bytes.
	CMP	R4, $4
	BLT	tail_2
	MOVWZ	(R3), R12
	CMPB	R12, R5, R12
	SLD	$32, R12, R12	// Remove non-participating matches.
	POPCNTD	R12, R12
	ADD	R12, R18, R18
	ADD	$4, R3, R3
	ADD	$-4, R4, R4

tail_2:	// Count the remaining 0 - 3 bytes.
	CMP	R4, $2
	BLT	tail_1
	MOVHZ	(R3), R12
	CMPB	R12, R5, R12
	SLD	$48, R12, R12	// Remove non-participating matches.
	POPCNTD	R12, R12
	ADD	R12, R18, R18
	ADD	$2, R3, R3
	ADD	$-2, R4, R4

tail_1:	// Count the remaining 0 - 1 bytes.
	CMP	R4, $1
	BLT	tail_0
	MOVBZ	(R3), R12
	CMPB	R12, R5, R12
	ANDCC	$0x8, R12, R12
	ADD	R12, R18, R18
#endif

tail_0:	// No remaining tail to count.
	SRD	$3, R18, R3	// Fixup count, it is off by 8x.
	RET

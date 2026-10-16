// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build (ppc64 || ppc64le) && linux

package flate

import (
	"sync"
	"syscall"
	"unsafe"
)

const (
	// Device path for user-space VAS NX-GZIP access
	nxDevicePath = "/dev/crypto/nx-gzip"

	// VAS ioctl request for opening a transmit window:
	// _IOW('v', 0x20, struct vas_tx_win_open_attr) = 0x80407620
	vasTxWinOpenIoctl = 0x80407620

	// NX coprocessor types and flags
	nxCopTypeGZIP = 0x1F // GZIP coprocessor type for NX

	// GZIP/ZLIB Sub-function and sub-format flags
	nxFormatRawDeflate = 0 // RFC 1951 Raw Deflate
	nxFormatZLIB       = 1 // RFC 1950 ZLIB
	nxFormatGZIP       = 2 // RFC 1952 GZIP

	// Completion status codes in CSB
	nxCSBSuccess        = 0x00
	nxCSBPartial        = 0x01
	nxCSBInputExhausted = 0x02

	// Alignment constants
	nxAlignSize = 128
)

// vasTxWinOpenAttr represents the Linux kernel struct vas_tx_win_open_attr (64 bytes).
type vasTxWinOpenAttr struct {
	version   uint32
	vasID     int16
	reserved1 uint16
	flags     uint64
	reserved2 [6]uint64
}

// nxDDE represents a Data Descriptor Element (DDE) for NX DMA scatter-gather.
type nxDDE struct {
	flags   uint8
	count   uint8
	res1    uint16
	length  uint32
	address uint64
}

// nxCSB represents the Coprocessor Status Block (128 bytes, 128-byte aligned).
type nxCSB struct {
	flags       uint8
	csbCC       uint8 // Completion code
	res1        uint16
	processedIn uint32
	prodOut     uint32
	checksum    uint32 // Adler-32 or CRC-32
	res2        [112]byte
}

// nxCRB represents the Coprocessor Request Block (128 bytes, 128-byte aligned).
type nxCRB struct {
	// Header (16 bytes)
	copType  uint8
	priority uint8
	flags    uint16
	res1     uint32
	csbAddr  uint64 // Pointer to nxCSB (virtual address)

	// GZIP/Deflate specific parameters (40 bytes)
	subFunc   uint8
	format    uint8
	res2      uint16
	maxOutLen uint32
	inDDE     nxDDE
	outDDE    nxDDE

	// Padding to ensure exactly 128 bytes total size (128 - 56 = 72)
	pad [72]byte
}

// nxState tracks the initialized VAS paste window for the process.
type nxState struct {
	available bool
	fd        int
	pasteAddr uintptr
	mmapLen   int
}

var (
	globalNX     nxState
	globalNXOnce sync.Once
)

// nxInit initializes the VAS NX-GZIP hardware window if available on POWER10.
func nxInit() {
	fd, err := syscall.Open(nxDevicePath, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return
	}

	var attr vasTxWinOpenAttr
	attr.version = 1
	attr.vasID = -1

	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(vasTxWinOpenIoctl),
		uintptr(unsafe.Pointer(&attr)),
	)
	if errno != 0 {
		syscall.Close(fd)
		return
	}

	// MMIO map the 64 KB paste window returned for this VAS window fd
	pageSize := syscall.Getpagesize()
	if pageSize < 64*1024 {
		pageSize = 64 * 1024
	}

	data, err := syscall.Mmap(fd, 0, pageSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		syscall.Close(fd)
		return
	}

	globalNX.available = true
	globalNX.fd = fd
	globalNX.pasteAddr = uintptr(unsafe.Pointer(&data[0]))
	globalNX.mmapLen = pageSize
}

// isNXAvailable returns whether POWER10 NX hardware compression is ready.
func isNXAvailable() bool {
	globalNXOnce.Do(nxInit)
	return globalNX.available
}

// nxCopyPaste executes the Power ISA copy and paste. instructions to dispatch
// the CRB to the NX coprocessor MMIO paste window.
//
//go:noescape
func nxCopyPaste(pasteAddr uintptr, crb *nxCRB) uint32

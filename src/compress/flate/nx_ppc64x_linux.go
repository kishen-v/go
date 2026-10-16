// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build (ppc64 || ppc64le) && linux

package flate

import (
	"errors"
	"math/bits"
	"os"
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

	// Minimum payload threshold for hardware offload (32 KB)
	nxMinPayloadSize = 32 * 1024

	// Alignment constants
	nxAlignSize = 256
)

var (
	errNXNotAccepted = errors.New("nx: paste rejected by hardware")
	nxMu             sync.Mutex
)

// vasTxWinOpenAttr represents the Linux kernel struct vas_tx_win_open_attr (64 bytes).
type vasTxWinOpenAttr struct {
	version   uint32
	vasID     int16
	reserved1 uint16
	flags     uint64
	reserved2 [6]uint64
}

// nxDDE represents a Data Descriptor Element (DDE) for NX DMA scatter-gather (16 bytes).
type nxDDE struct {
	flags   uint16
	count   uint8
	index   uint8
	length  uint32
	address uint64
}

// nxCSB represents the Coprocessor Status Block (128 bytes, 128-byte aligned).
type nxCSB struct {
	flags   uint8
	cs      uint8
	cc      uint8 // Completion code: 0 = success
	ce      uint8
	count   uint32
	address uint64
	res     [112]byte
}

// nxCRB represents the Coprocessor Request Block (128 bytes, 256-byte aligned).
type nxCRB struct {
	ccw     uint32
	flags   uint32
	csbAddr uint64

	sourceDDE nxDDE
	targetDDE nxDDE

	ccbValue uint64
	ccbAddr  uint64

	reserved [64]byte
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
	// Disable during toolchain bootstrap or when explicitly disabled via environment
	if os.Getenv("NX_DISABLE") == "1" {
		return
	}

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
	globalNX.pasteAddr = uintptr(unsafe.Pointer(&data[0])) + 0x400
	globalNX.mmapLen = pageSize
}

// isNXAvailable returns whether POWER10 NX hardware compression is ready.
func isNXAvailable() bool {
	globalNXOnce.Do(nxInit)
	return globalNX.available
}

// Touch memory pages to ensure virtual memory is resident and avoid NX page faults.
func nxTouchPages(buf []byte, write bool) {
	if len(buf) == 0 {
		return
	}
	pageSize := 64 * 1024
	for i := 0; i < len(buf); i += pageSize {
		if write {
			buf[i] = buf[i] + 0
		} else {
			_ = buf[i]
		}
	}
	last := len(buf) - 1
	if write {
		buf[last] = buf[last] + 0
	} else {
		_ = buf[last]
	}
}

// Allocate aligned memory for CRB / CSB structures.
func allocAligned(size, align int) []byte {
	buf := make([]byte, size+align)
	offset := align - int(uintptr(unsafe.Pointer(&buf[0]))%uintptr(align))
	if offset == align {
		offset = 0
	}
	return buf[offset : offset+size]
}

//go:noescape
func nxCopyPaste(pasteAddr uintptr, crb unsafe.Pointer) uint32

// nxCompress executes Deflate compression on src into dst using Power10 NX hardware.
func nxCompress(src, dst []byte) (int, error) {
	if !globalNX.available || globalNX.pasteAddr == 0 {
		return 0, errNXNotAccepted
	}

	nxMu.Lock()
	defer nxMu.Unlock()

	cmdBuf := allocAligned(2048, 2048)
	cmdPtr := uintptr(unsafe.Pointer(&cmdBuf[0]))

	// GZIP_FC_COMPRESS_RESUME_FHT = 0x08
	*(*uint32)(unsafe.Pointer(cmdPtr + 0)) = bits.ReverseBytes32(0x00000008)

	// CSB is located at offset 240
	csbAddr := uint64(cmdPtr + 240)
	*(*uint64)(unsafe.Pointer(cmdPtr + 8)) = bits.ReverseBytes64(csbAddr)

	// Source DDE: length at offset 20, address at offset 24
	srcPtr := uint64(uintptr(unsafe.Pointer(&src[0])))
	*(*uint32)(unsafe.Pointer(cmdPtr + 20)) = bits.ReverseBytes32(uint32(len(src)))
	*(*uint64)(unsafe.Pointer(cmdPtr + 24)) = bits.ReverseBytes64(srcPtr)

	// Target DDE: length at offset 36, address at offset 40
	dstPtr := uint64(uintptr(unsafe.Pointer(&dst[0])))
	*(*uint32)(unsafe.Pointer(cmdPtr + 36)) = bits.ReverseBytes32(uint32(len(dst)))
	*(*uint64)(unsafe.Pointer(cmdPtr + 40)) = bits.ReverseBytes64(dstPtr)

	// Touch memory pages to ensure virtual memory is resident
	nxTouchPages(src, false)
	nxTouchPages(dst, true)

	// Retry paste loop up to 5000 attempts
	var ret uint32
	var ok bool
	for r := 0; r < 5000; r++ {
		ret = nxCopyPaste(globalNX.pasteAddr, unsafe.Pointer(cmdPtr))
		cr0 := (ret >> 28) & 0xF
		if cr0 == 2 || cr0 == 3 {
			ok = true
			break
		}
	}
	if !ok {
		return 0, errNXNotAccepted
	}

	// Poll CSB completion flag at offset 240
	csbFlags := (*uint8)(unsafe.Pointer(cmdPtr + 240))
	for i := 0; *csbFlags&0x80 == 0; i++ {
		if i > 10000000 {
			return 0, errNXNotAccepted
		}
	}

	csbCC := *(*uint8)(unsafe.Pointer(cmdPtr + 242))
	if csbCC != 0 {
		return 0, errNXNotAccepted
	}

	prod := bits.ReverseBytes32(*(*uint32)(unsafe.Pointer(cmdPtr + 244)))
	if prod > 0 {
		// Set BFINAL bit (bit 0 of the first Deflate block) per RFC 1951
		dst[0] |= 0x01
	}
	return int(prod), nil
}

// writeNX writes compressed data using the NX hardware offload path.
func (w *Writer) writeNX(data []byte) (int, error) {
	dst := make([]byte, len(data)+1024)
	n, err := nxCompress(data, dst)
	if err != nil {
		// Fallback cleanly to software path on error
		return w.d.write(data)
	}
	_, err = w.d.w.writer.Write(dst[:n])
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

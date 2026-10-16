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
	globalNX.pasteAddr = uintptr(unsafe.Pointer(&data[0]))
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
func nxCopyPaste(pasteAddr uintptr, crb *nxCRB) uint32

// nxCompress executes Deflate compression on src into dst using Power10 NX hardware.
func nxCompress(src, dst []byte) (int, error) {
	if !globalNX.available || globalNX.pasteAddr == 0 {
		return 0, errNXNotAccepted
	}

	nxMu.Lock()
	defer nxMu.Unlock()

	crbBuf := allocAligned(int(unsafe.Sizeof(nxCRB{})), nxAlignSize)
	crb := (*nxCRB)(unsafe.Pointer(&crbBuf[0]))
	*crb = nxCRB{}

	csbBuf := allocAligned(int(unsafe.Sizeof(nxCSB{})), 128)
	csb := (*nxCSB)(unsafe.Pointer(&csbBuf[0]))
	*csb = nxCSB{}

	csbPtr := uint64(uintptr(unsafe.Pointer(csb)))

	// CCW for GZIP/Deflate: FC = GZIP_FC_COMPRESS_FHT (0x02), CT = 0x1F
	crb.ccw = bits.ReverseBytes32(0x001F0002)
	crb.csbAddr = bits.ReverseBytes64(csbPtr)

	// Setup Source DDE
	srcPtr := uint64(uintptr(unsafe.Pointer(&src[0])))
	crb.sourceDDE.length = bits.ReverseBytes32(uint32(len(src)))
	crb.sourceDDE.address = bits.ReverseBytes64(srcPtr)

	// Setup Target DDE
	dstPtr := uint64(uintptr(unsafe.Pointer(&dst[0])))
	crb.targetDDE.length = bits.ReverseBytes32(uint32(len(dst)))
	crb.targetDDE.address = bits.ReverseBytes64(dstPtr)

	// Touch memory pages to avoid DMA page faults
	nxTouchPages(src, false)
	nxTouchPages(dst, true)

	// Dispatch to hardware MMIO window
	ret := nxCopyPaste(globalNX.pasteAddr, crb)

	// In Power ISA, CR0 is the upper 4 bits. CR0.EQ indicates paste acceptance.
	if (ret>>28)&0x2 == 0 {
		println("nx: paste rejected, CR0=", ret>>28, "raw CR=", ret)
		return 0, errNXNotAccepted
	}

	// Poll CSB completion flag (bit 0 of flags byte indicates hardware completion)
	for i := 0; csb.flags&0x80 == 0; i++ {
		if i > 5000000 {
			return 0, errNXNotAccepted
		}
	}

	if csb.cc != 0 {
		return 0, errNXNotAccepted
	}

	prod := bits.ReverseBytes32(csb.count)
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

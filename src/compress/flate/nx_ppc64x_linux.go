// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build ppc64le && linux

package flate

// This file drives the POWER10 NX-GZIP accelerator through a VAS paste window.
// It follows the reference implementation in https://github.com/libnxz/power-gzip
// (lib/gzip_vas.c, lib/nx_zlib.c and lib/nx_deflate.c). The CRB/CPB layout below
// was verified against inc_nx/nxu.h: the 2048-byte command block holds the
// 256-byte CRB (CSB at +240) followed by the CPB at +256.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
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

	// Command block (nx_gzip_crb_cpb_t) layout; all fields are big-endian.
	nxCmdSize       = 2048
	nxOffFC         = 0   // crb.gzip_fc
	nxOffCSBAddr    = 8   // crb.csb_address
	nxOffSrcDDE     = 16  // crb.source_dde
	nxOffTgtDDE     = 32  // crb.target_dde
	nxOffCSB        = 240 // crb.csb
	nxOffCSBCC      = nxOffCSB + 2
	nxOffCSBCE      = nxOffCSB + 3
	nxOffCSBTPBC    = nxOffCSB + 4
	nxOffCPBTEBC    = 256 + 392 // cpb.out_tebc (bits 77:79 of qw24)
	nxOffCPBSPBCRaw = 256 + 400 // cpb.out_spbc_comp (compress, no lzcounts)
	nxOffDDEBC      = 4         // dde.ddebc, relative to the DDE
	nxOffDDEAD      = 8         // dde.ddead, relative to the DDE

	// GZIP_FC_COMPRESS_RESUME_FHT
	nxFCCompressResumeFHT = 0x08

	// CSB completion codes (ERR_NX_*)
	nxCCOK          = 0
	nxCCDataLength  = 3
	nxCCTargetSpace = 13
	nxCCTPBCGTSPBC  = 64 // ERR_NX_TPBC_GT_SPBC: output would exceed input
	nxCCATFault     = 250

	// CSB completion extension bits (after >>5, as get_csb_ce_ms3b)
	nxCEPartial   = 0x4
	nxCETerminate = 0x2

	// Maximum bytes described by one DDE (nx_config.strm_def_bufsz comment: 1GB)
	nxMaxDDELen = 1 << 30

	// Sysfs capability files
	nxSysfsGzipCaps = "/sys/devices/vio/ibm,compression-v1/nx_gzip_caps/"
	nxSysfsVasCaps  = "/sys/devices/virtual/misc/vas/vas0/gzip/default_capabilities/"

	// Default per-job limits when sysfs is unavailable (nx_zlib.c)
	nxDefaultJobBareMetal = 64 * 1024 * 1024
	nxDefaultJobPowerVM   = 1024 * 1024
)

// Timeouts, taken from nx_zlib.c defaults.
const (
	nxPasteTimeoutPowerVM  = 60 * time.Second  // timeout_paste_success
	nxPasteTimeoutBare     = 300 * time.Second // timeout_paste_success
	nxCSBTimeout           = 60 * time.Second  // timeout_wait_for_csb_v
	nxPageFaultTimeout     = 300 * time.Second // timeout_pgfaults
	nxSuspendCheckAfter    = time.Second / 32  // SUSPENDED_TIME_THRESHOLD
	nxSleepAfter           = 215 * time.Microsecond
	nxMaxSleep             = time.Millisecond
	nxWindowReopenBaseWait = time.Second / 32
)

var (
	errNXUnavailable = errors.New("nx: hardware acceleration is unavailable or disabled")
	errNXNotAccepted = errors.New("nx: paste rejected by hardware")
	errNXEmpty       = errors.New("nx: empty source or destination buffer")
	errNXTooLarge    = errors.New("nx: source exceeds the hardware job size limit")
	errNXTargetSpace = errors.New("nx: destination buffer too small")
	errNXExpanded    = errors.New("nx: output is larger than input")
	errNXPartial     = errors.New("nx: hardware did not consume all of the input")
	errNXPageFault   = errors.New("nx: too many page faults")
	errNXTimeout     = errors.New("nx: timed out waiting for the coprocessor status block")
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
	available    bool
	powerVM      bool
	fd           int
	mem          []byte
	pasteAddr    uintptr
	jobLimit     int
	totalCredits int // nr_total_credits when the window was opened
}

var (
	globalNX     nxState
	globalNXOnce sync.Once

	// nxCmd is the single, 2048-byte aligned command block. It is heap
	// allocated once and never freed, so the coprocessor can never write the
	// CSB into reclaimed memory. It is protected by nxMu.
	nxCmd []byte

	// nxSink forces src/dst to escape to the heap (so they cannot live on a
	// movable goroutine stack while the coprocessor holds their addresses).
	nxSink unsafe.Pointer

	// nxLeaked retains buffers handed to a job that never completed.
	nxLeaked [][]byte
)

// nxReadInt reads a single integer from a sysfs file (nx_read_sysfs_entry).
func nxReadInt(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	return v, true
}

// nxIsPowerVM mirrors nx_enumerate_engines: PowerVM partitions expose an
// "ibm,powervm*" node in the device tree, or /proc/powerpc/lparcfg when the
// device tree is hidden (containers).
func nxIsPowerVM() bool {
	entries, err := os.ReadDir("/proc/device-tree")
	if err != nil {
		_, err := os.Stat("/proc/powerpc/lparcfg")
		return err == nil
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "ibm,powervm") {
			return true
		}
	}
	return false
}

// nxQueryJobLimit mirrors nx_query_job_limits.
func nxQueryJobLimit(powerVM bool) int {
	if v, ok := nxReadInt(nxSysfsGzipCaps + "req_max_processed_len"); ok && v > 0 {
		return v
	}
	if powerVM {
		return nxDefaultJobPowerVM
	}
	return nxDefaultJobBareMetal
}

// nxOpenWindow opens a VAS transmit window and maps its paste area
// (open_device_nodes in gzip_vas.c).
func nxOpenWindow() error {
	fd, err := syscall.Open(nxDevicePath, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
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
		return errno
	}

	pageSize := syscall.Getpagesize()
	if pageSize < 64*1024 {
		pageSize = 64 * 1024
	}

	data, err := syscall.Mmap(fd, 0, pageSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		syscall.Close(fd)
		return err
	}

	globalNX.fd = fd
	globalNX.mem = data
	globalNX.pasteAddr = uintptr(unsafe.Pointer(&data[0])) + 0x400
	return nil
}

// nxCloseWindow releases the current window (nx_function_end).
func nxCloseWindow() {
	if globalNX.mem != nil {
		syscall.Munmap(globalNX.mem)
		globalNX.mem = nil
	}
	if globalNX.fd != 0 {
		syscall.Close(globalNX.fd)
		globalNX.fd = 0
	}
	globalNX.pasteAddr = 0
}

// nxInit initializes the VAS NX-GZIP hardware window if available on POWER10.
func nxInit() {
	// Disable during toolchain bootstrap or when explicitly disabled via environment
	if os.Getenv("NX_DISABLE") == "1" {
		return
	}

	globalNX.powerVM = nxIsPowerVM()
	globalNX.jobLimit = nxQueryJobLimit(globalNX.powerVM)

	if err := nxOpenWindow(); err != nil {
		return
	}
	if v, ok := nxReadInt(nxSysfsVasCaps + "nr_total_credits"); ok {
		globalNX.totalCredits = v
	}

	nxCmd = allocAligned(nxCmdSize, nxCmdSize)
	globalNX.available = true
}

// isNXAvailable returns whether POWER10 NX hardware compression is ready.
func isNXAvailable() bool {
	globalNXOnce.Do(nxInit)
	return globalNX.available
}

// IsNXAvailable returns whether POWER10 NX hardware acceleration is available.
func IsNXAvailable() bool {
	return isNXAvailable()
}

// NXCompress compresses src into dst using Power10 NX hardware acceleration
// and returns the number of bytes written to dst.
//
// The result is a complete raw deflate stream: it is terminated with an empty
// final stored block, as nx_stream_end does in libnxz. src must be non-empty
// and no larger than the hardware job limit; dst must be large enough for the
// output (a fixed-Huffman job can expand incompressible data by about 12.5%,
// plus 6 bytes for the terminator). Callers should fall back to software
// compression on any error.
func NXCompress(src, dst []byte) (int, error) {
	n, tebc, err := nxCompress(src, dst)
	if err != nil {
		return 0, err
	}
	// The engine marks its block BFINAL. Clear it, as set_bfinal(s, false, 0)
	// does in libnxz, since the terminating stored block appended below is the
	// final one; otherwise a decoder stops before the terminator and leaves
	// its bytes unread.
	dst[0] &^= 1
	return nxAppendFinalBlock(dst, n, tebc)
}

// nxAppendFinalBlock closes a deflate stream whose last byte holds only tebc
// valid bits, by appending a BFINAL stored block of length 0 (append_sync_flush
// with final=1 in nx_deflate.c). It returns the new stream length.
func nxAppendFinalBlock(dst []byte, n int, tebc uint) (int, error) {
	var lo byte
	start := n
	if tebc > 0 {
		start = n - 1
		lo = dst[start] & (1<<tebc - 1) // keep only the valid bits
	}
	lo |= 1 << tebc // BFINAL=1; BTYPE=00 follows as zero bits

	shift := tebc + 3 // BFINAL and BTYPE written
	if shift <= 8 {
		shift = 8
	} else {
		shift = 16
	}
	flush := uint64(lo) | uint64(0xFFFF0000)<<shift // LEN=0, NLEN=0xFFFF
	total := int(shift+32) / 8

	if start+total > len(dst) {
		return 0, errNXTargetSpace
	}
	for i := 0; i < total; i++ {
		dst[start+i] = byte(flush)
		flush >>= 8
	}
	return start + total, nil
}

// nxTouch makes the page holding *p resident: it loads the byte and, if write
// is set, stores it back unchanged (nx_touch_pages in libnxz). It is written in
// assembly because the compiler would eliminate an equivalent Go load/store.
//
//go:noescape
func nxTouch(p *byte, write bool)

// nxTouchPages touches one byte per page of buf, plus the last byte, so that
// the NX engine does not take translation faults (nx_touch_pages).
func nxTouchPages(buf []byte, write bool) {
	if len(buf) == 0 {
		return
	}
	pageSize := os.Getpagesize()
	for i := 0; i < len(buf); i += pageSize {
		nxTouch(&buf[i], write)
	}
	nxTouch(&buf[len(buf)-1], write)
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

// nxWindowSuspended reports whether the VAS window may have been suspended by
// a DLPAR operation or partition migration (PowerVM only, nxu_run_job).
func nxWindowSuspended() bool {
	total, ok1 := nxReadInt(nxSysfsVasCaps + "nr_total_credits")
	used, ok2 := nxReadInt(nxSysfsVasCaps + "nr_used_credits")
	if !ok1 || !ok2 {
		return false
	}
	return used > total-1 || globalNX.totalCredits > total
}

// nxReopenWindow replaces a suspended window, backing off 31ms, 62ms and 125ms
// as nxu_run_job does. It disables NX if no window can be obtained in time.
func nxReopenWindow(deadline time.Time) error {
	nxCloseWindow()
	for n := 0; ; n++ {
		if err := nxOpenWindow(); err == nil {
			break
		}
		wait := nxWindowReopenBaseWait << (n % 3)
		if time.Now().Add(wait).After(deadline) {
			globalNX.available = false
			return errNXNotAccepted
		}
		time.Sleep(wait)
	}
	if v, ok := nxReadInt(nxSysfsVasCaps + "nr_total_credits"); ok {
		globalNX.totalCredits = v
	}
	return nil
}

// nxPaste submits nxCmd, retrying while the NX queue is full (nxu_run_job).
// Paste failures can mean a full queue, a busy paste buffer, or on PowerVM a
// suspended window. It must be called with nxMu held.
func nxPaste() error {
	timeout := nxPasteTimeoutBare
	if globalNX.powerVM {
		timeout = nxPasteTimeoutPowerVM
	}
	start := time.Now()
	lastCheck := start

	for {
		if globalNX.pasteAddr == 0 {
			return errNXNotAccepted
		}
		ret := nxCopyPaste(globalNX.pasteAddr, unsafe.Pointer(&nxCmd[0]))
		if cr0 := (ret >> 28) & 0xF; cr0 == 2 || cr0 == 3 {
			return nil
		}

		now := time.Now()
		elapsed := now.Sub(start)
		if elapsed > timeout {
			return errNXNotAccepted
		}
		if globalNX.powerVM && elapsed > nxSuspendCheckAfter && now.Sub(lastCheck) > nxSuspendCheckAfter {
			lastCheck = now
			if nxWindowSuspended() {
				if err := nxReopenWindow(start.Add(timeout)); err != nil {
					return err
				}
				start, lastCheck = time.Now(), time.Now()
				continue
			}
		}
		if elapsed > nxSleepAfter {
			time.Sleep(min(elapsed/4, nxMaxSleep))
		}
	}
}

// nxWaitCSB waits for the coprocessor to mark the CSB valid. The atomic load
// has acquire semantics, so the CSB/CPB fields read afterwards are not stale.
func nxWaitCSB() bool {
	valid := (*uint32)(unsafe.Pointer(&nxCmd[nxOffCSB]))
	start := time.Now()
	for i := 1; ; i++ {
		if atomic.LoadUint32(valid)&0x80 != 0 { // csb.v is the MSB of byte 0
			return true
		}
		if i&0xFFF == 0 {
			elapsed := time.Since(start)
			if elapsed > nxCSBTimeout {
				return false
			}
			if elapsed > nxSleepAfter {
				time.Sleep(min(elapsed/4, nxMaxSleep))
			}
		}
	}
}

// nxCompress runs one RESUME_FHT compression job over all of src. It returns
// the number of output bytes and tebc, the number of valid bits (0 means all 8)
// in the last output byte. The output is not terminated.
func nxCompress(src, dst []byte) (int, uint, error) {
	if len(src) == 0 || len(dst) == 0 {
		return 0, 0, errNXEmpty
	}
	if !isNXAvailable() {
		return 0, 0, errNXUnavailable
	}
	if len(src) > globalNX.jobLimit || uint64(len(src)) > math.MaxUint32 {
		return 0, 0, errNXTooLarge
	}
	if len(dst) > nxMaxDDELen {
		dst = dst[:nxMaxDDELen]
	}

	nxMu.Lock()
	defer nxMu.Unlock()
	if !globalNX.available {
		return 0, 0, errNXUnavailable
	}

	// The coprocessor holds raw addresses of src and dst: make sure they are
	// heap allocated (not on a movable stack) and stay live until we are done.
	nxSink = unsafe.Pointer(&src[0])
	nxSink = unsafe.Pointer(&dst[0])
	nxSink = nil
	defer runtime.KeepAlive(src)
	defer runtime.KeepAlive(dst)

	nxTouchPages(src, false)
	nxTouchPages(dst, true)

	deadline := time.Now().Add(nxPageFaultTimeout)
	for retries := 0; ; retries++ {
		// Fresh CRB/CSB/CPB for every submission (nx_submit_job clears the CSB
		// and the output byte counts).
		clear(nxCmd)
		binary.BigEndian.PutUint32(nxCmd[nxOffFC:], nxFCCompressResumeFHT)
		binary.BigEndian.PutUint64(nxCmd[nxOffCSBAddr:], uint64(uintptr(unsafe.Pointer(&nxCmd[nxOffCSB]))))
		binary.BigEndian.PutUint32(nxCmd[nxOffSrcDDE+nxOffDDEBC:], uint32(len(src)))
		binary.BigEndian.PutUint64(nxCmd[nxOffSrcDDE+nxOffDDEAD:], uint64(uintptr(unsafe.Pointer(&src[0]))))
		binary.BigEndian.PutUint32(nxCmd[nxOffTgtDDE+nxOffDDEBC:], uint32(len(dst)))
		binary.BigEndian.PutUint64(nxCmd[nxOffTgtDDE+nxOffDDEAD:], uint64(uintptr(unsafe.Pointer(&dst[0]))))

		if err := nxPaste(); err != nil {
			return 0, 0, err
		}
		if !nxWaitCSB() {
			// The job may still be running and may yet write dst and the CSB.
			// Never hand those buffers back to the runtime, and stop using NX.
			nxLeaked = append(nxLeaked, src, dst)
			globalNX.available = false
			return 0, 0, errNXTimeout
		}

		cc := nxCmd[nxOffCSBCC]
		switch cc {
		case nxCCOK:
			spbc := binary.BigEndian.Uint32(nxCmd[nxOffCPBSPBCRaw:])
			tpbc := binary.BigEndian.Uint32(nxCmd[nxOffCSBTPBC:])
			if int(spbc) != len(src) {
				return 0, 0, errNXPartial
			}
			// The engine does not subtract history when checking for
			// expansion (nx_compress_block); libnxz falls back to a stored
			// block, we report it so the caller can do the same.
			if tpbc > spbc {
				return 0, 0, errNXExpanded
			}
			tebc := uint(binary.BigEndian.Uint32(nxCmd[nxOffCPBTEBC:])>>16) & 7
			return int(tpbc), tebc, nil

		case nxCCATFault:
			// Translation fault: touch the pages again and retry.
			if time.Now().After(deadline) {
				return 0, 0, errNXPageFault
			}
			if retries > 0 {
				time.Sleep(min(time.Duration(retries)*time.Millisecond, nxMaxSleep))
			}
			nxTouchPages(src, false)
			nxTouchPages(dst, true)

		case nxCCTargetSpace:
			return 0, 0, errNXTargetSpace

		case nxCCTPBCGTSPBC:
			// libnxz retries with a WRAP job to emit a stored block; we report
			// it so that the caller can fall back.
			return 0, 0, errNXExpanded

		case nxCCDataLength:
			ce := nxCmd[nxOffCSBCE] >> 5
			if ce&nxCETerminate == 0 && ce&nxCEPartial != 0 {
				// Job suspended at the DMA limit; only part of src was consumed.
				return 0, 0, errNXPartial
			}
			return 0, 0, fmt.Errorf("nx: history length error (cc=%d ce=%#x)", cc, ce)

		default:
			return 0, 0, fmt.Errorf("nx: job failed (cc=%d ce=%#x)", cc, nxCmd[nxOffCSBCE]>>5)
		}
	}
}

package main

import (
	"os"
	"sync"
	"syscall"
	"unsafe"
)

// Off-heap storage for items. Items live in anonymous mmap'd memory so the Go
// GC never scans or copies them and RSS stays close to the live data size.
//
// Small items are carved from size classes: each shard bump-allocates from
// blocks handed out by a global arena and keeps its own per-class free lists.
// Items larger than maxSmall get a dedicated mapping.

const (
	maxSmall   = 64 << 10
	clsBig     = 255
	arenaChunk = 64 << 20
	blockSize  = 64 << 10
)

var (
	classSize   []int
	classLookup [maxSmall/16 + 1]uint8
	pageSize    = os.Getpagesize()
)

func init() {
	for s := 32; s <= 256; s += 16 {
		classSize = append(classSize, s)
	}
	s := 256
	for s < maxSmall {
		s += s / 8
		s = (s + 15) &^ 15
		if s > maxSmall {
			s = maxSmall
		}
		classSize = append(classSize, s)
	}
	ci := 0
	for i := range classLookup {
		n := i * 16
		for classSize[ci] < n {
			ci++
		}
		classLookup[i] = uint8(ci)
	}
}

func classOf(n int) int { return int(classLookup[(n+15)>>4]) }

func mmap(n int) []byte {
	b, err := syscall.Mmap(-1, 0, n, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		panic("mmap: " + err.Error())
	}
	return b
}

func roundPage(n int) int { return (n + pageSize - 1) &^ (pageSize - 1) }

var arena struct {
	mu       sync.Mutex
	chunks   [][]byte
	cur, end uintptr
}

func arenaBlock(n uintptr) uintptr {
	arena.mu.Lock()
	if arena.cur+n > arena.end {
		size := max(arenaChunk, int(n))
		b := mmap(size)
		arena.chunks = append(arena.chunks, b)
		arena.cur = uintptr(unsafe.Pointer(&b[0]))
		arena.end = arena.cur + uintptr(size)
	}
	p := arena.cur
	arena.cur += n
	arena.mu.Unlock()
	return p
}

// arenaReset unmaps all small-item memory. Caller must hold every shard lock.
func arenaReset() {
	arena.mu.Lock()
	for _, b := range arena.chunks {
		syscall.Munmap(b)
	}
	arena.chunks = nil
	arena.cur, arena.end = 0, 0
	arena.mu.Unlock()
}

type bump struct{ cur, end uintptr }

func (s *shard) allocRaw(total int) (uintptr, uint8) {
	if total > maxSmall {
		b := mmap(roundPage(total))
		return uintptr(unsafe.Pointer(&b[0])), clsBig
	}
	cls := classOf(total)
	if p := s.free[cls]; p != 0 {
		s.free[cls] = *(*uintptr)(unsafe.Pointer(p))
		return p, uint8(cls)
	}
	sz := uintptr(classSize[cls])
	b := &s.bumps[cls]
	if b.cur+sz > b.end {
		bs := uintptr(max(blockSize, classSize[cls]*8))
		b.cur = arenaBlock(bs)
		b.end = b.cur + bs
	}
	p := b.cur
	b.cur += sz
	return p, uint8(cls)
}

func (s *shard) freeItem(it item) {
	cls := it.cls()
	if cls == clsBig {
		syscall.Munmap(unsafe.Slice((*byte)(it.p()), roundPage(hdrSize+it.klen()+it.vlen())))
		return
	}
	*(*uintptr)(it.p()) = s.free[cls]
	s.free[cls] = uintptr(it)
}

// capacity is the number of value bytes the item can hold without reallocation.
func (it item) capacity() int {
	if it.cls() == clsBig {
		return it.vlen()
	}
	return classSize[it.cls()] - hdrSize - it.klen()
}

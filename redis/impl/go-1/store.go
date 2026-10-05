package main

import (
	"bytes"
	"hash/maphash"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

const (
	nShards   = 256
	shardBits = 8
	hdrSize   = 24
	tomb      = uint64(1)
	ptrMask   = uint64(1)<<48 - 1

	typStr  = 0
	typList = 1
	typHash = 2
)

// item is the address of an off-heap record:
//
//	0  exp   int64  (unix ms, 0 = none)
//	8  lru   uint32
//	12 klen  uint32
//	16 vlen  uint32 (containers: 4, value holds the object index)
//	20 typ   uint8
//	21 cls   uint8
//	24 key bytes, value bytes
type item uintptr

func (it item) p() unsafe.Pointer { return unsafe.Pointer(uintptr(it)) }
func (it item) exp() int64        { return *(*int64)(it.p()) }
func (it item) setExpRaw(v int64) { *(*int64)(it.p()) = v }
func (it item) lru() uint32       { return *(*uint32)(unsafe.Add(it.p(), 8)) }
func (it item) setLRU(v uint32)   { *(*uint32)(unsafe.Add(it.p(), 8)) = v }
func (it item) klen() int         { return int(*(*uint32)(unsafe.Add(it.p(), 12))) }
func (it item) vlen() int         { return int(*(*uint32)(unsafe.Add(it.p(), 16))) }
func (it item) setVlen(n int)     { *(*uint32)(unsafe.Add(it.p(), 16)) = uint32(n) }
func (it item) typ() uint8        { return *(*uint8)(unsafe.Add(it.p(), 20)) }
func (it item) cls() uint8        { return *(*uint8)(unsafe.Add(it.p(), 21)) }
func (it item) key() []byte       { return unsafe.Slice((*byte)(unsafe.Add(it.p(), hdrSize)), it.klen()) }
func (it item) val() []byte       { return it.valN(it.vlen()) }
func (it item) valN(n int) []byte {
	return unsafe.Slice((*byte)(unsafe.Add(it.p(), hdrSize+it.klen())), n)
}
func (it item) objIdx() uint32     { return *(*uint32)(unsafe.Add(it.p(), hdrSize+it.klen())) }
func (it item) setObjIdx(i uint32) { *(*uint32)(unsafe.Add(it.p(), hdrSize+it.klen())) = i }
func (it item) setHdr(kl, vl int, t, c uint8) {
	*(*uint32)(unsafe.Add(it.p(), 12)) = uint32(kl)
	*(*uint32)(unsafe.Add(it.p(), 16)) = uint32(vl)
	*(*uint8)(unsafe.Add(it.p(), 20)) = t
	*(*uint8)(unsafe.Add(it.p(), 21)) = c
}

type listObj struct {
	buf  []string
	head int
	n    int
	cost int64
}

// hashObj stores small hashes as a flat field/value slice and switches to a
// map once it grows past hashMaxFlat fields.
type hashObj struct {
	kv   []string
	m    map[string]string
	cost int64
}

const hashMaxFlat = 16

func (h *hashObj) get(f []byte) (string, bool) {
	if h.m != nil {
		v, ok := h.m[string(f)]
		return v, ok
	}
	for i := 0; i < len(h.kv); i += 2 {
		if h.kv[i] == string(f) {
			return h.kv[i+1], true
		}
	}
	return "", false
}

// set stores f=v and returns the previous value, if any.
func (h *hashObj) set(f []byte, v string) (string, bool) {
	if h.m != nil {
		old, ok := h.m[string(f)]
		h.m[string(f)] = v
		return old, ok
	}
	for i := 0; i < len(h.kv); i += 2 {
		if h.kv[i] == string(f) {
			old := h.kv[i+1]
			h.kv[i+1] = v
			return old, true
		}
	}
	if len(h.kv) >= hashMaxFlat*2 {
		h.m = make(map[string]string, len(h.kv))
		for i := 0; i < len(h.kv); i += 2 {
			h.m[h.kv[i]] = h.kv[i+1]
		}
		h.kv = nil
		h.m[string(f)] = v
		return "", false
	}
	h.kv = append(h.kv, string(f), v)
	return "", false
}

func (h *hashObj) del(f []byte) (string, bool) {
	if h.m != nil {
		old, ok := h.m[string(f)]
		if ok {
			delete(h.m, string(f))
		}
		return old, ok
	}
	for i := 0; i < len(h.kv); i += 2 {
		if h.kv[i] == string(f) {
			old := h.kv[i+1]
			n := len(h.kv)
			h.kv[i], h.kv[i+1] = h.kv[n-2], h.kv[n-1]
			h.kv[n-2], h.kv[n-1] = "", ""
			h.kv = h.kv[:n-2]
			return old, true
		}
	}
	return "", false
}

func (h *hashObj) size() int {
	if h.m != nil {
		return len(h.m)
	}
	return len(h.kv) / 2
}

type shard struct {
	mu       sync.Mutex
	slots    []uint64
	count    int
	tombs    int
	used     int64
	nexp     int
	expCur   int
	free     [80]uintptr
	bumps    [80]bump
	objs     []any
	freeObjs []uint32
	_        [64]byte
}

var (
	shards     [nShards]shard
	seed       = maphash.MakeSeed()
	maxMemory  int64
	globalUsed atomic.Int64
	startMs    = time.Now().UnixMilli()
)

func init() {
	for i := range shards {
		shards[i].slots = make([]uint64, 8)
	}
}

func hashKey(k []byte) uint64   { return maphash.Bytes(seed, k) }
func shardOf(h uint64) *shard   { return &shards[h&(nShards-1)] }
func lruClock(now int64) uint32 { return uint32(now - startMs) }
func (s *shard) addUsed(d int64) {
	s.used += d
	if maxMemory > 0 {
		globalUsed.Add(d)
	}
}

func (s *shard) cost(it item) int64 {
	switch it.typ() {
	case typStr:
		return int64(64 + it.klen() + it.vlen())
	case typList:
		return int64(64+it.klen()) + s.objs[it.objIdx()].(*listObj).cost
	default:
		return int64(64+it.klen()) + s.objs[it.objIdx()].(*hashObj).cost
	}
}

// find returns the slot of key (or -1) and the first free slot on the probe path.
func (s *shard) find(key []byte, h uint64) (idx, ins int) {
	mask := len(s.slots) - 1
	i := int(h>>shardBits) & mask
	tag := h >> 48
	ins = -1
	for {
		v := s.slots[i]
		if v == 0 {
			if ins < 0 {
				ins = i
			}
			return -1, ins
		}
		if v == tomb {
			if ins < 0 {
				ins = i
			}
		} else if v>>48 == tag {
			if bytes.Equal(item(v&ptrMask).key(), key) {
				return i, ins
			}
		}
		i = (i + 1) & mask
	}
}

// get looks a key up, lazily expiring it and touching its LRU clock.
func (s *shard) get(key []byte, h uint64, now int64) (idx int, it item, ins int) {
	idx, ins = s.find(key, h)
	if idx < 0 {
		return -1, 0, ins
	}
	it = item(s.slots[idx] & ptrMask)
	if e := it.exp(); e != 0 && e <= now {
		s.del(idx, it)
		if ins < 0 {
			ins = idx
		}
		return -1, 0, ins
	}
	if maxMemory > 0 {
		it.setLRU(lruClock(now))
	}
	return idx, it, ins
}

// grow must be called before a lookup that may insert.
func (s *shard) grow() {
	if (s.count+s.tombs+1)*4 > len(s.slots)*3 {
		n := 8
		for n*3 < (s.count+1)*4*2 {
			n *= 2
		}
		s.rehash(n)
	}
}

func (s *shard) rehash(n int) {
	old := s.slots
	s.slots = make([]uint64, n)
	mask := n - 1
	for _, v := range old {
		if v > tomb {
			h := hashKey(item(v & ptrMask).key())
			i := int(h>>shardBits) & mask
			for s.slots[i] != 0 {
				i = (i + 1) & mask
			}
			s.slots[i] = v
		}
	}
	s.tombs = 0
	s.expCur = 0
}

func (s *shard) newItem(key []byte, vlen int, typ uint8, now int64) item {
	p, cls := s.allocRaw(hdrSize + len(key) + vlen)
	it := item(p)
	it.setExpRaw(0)
	it.setLRU(lruClock(now))
	it.setHdr(len(key), vlen, typ, cls)
	copy(it.key(), key)
	return it
}

func (s *shard) insert(ins int, h uint64, it item) {
	if s.slots[ins] == tomb {
		s.tombs--
	}
	s.slots[ins] = uint64(it) | (h>>48)<<48
	s.count++
	if it.exp() != 0 {
		s.nexp++
	}
	s.addUsed(s.cost(it))
}

func (s *shard) setExp(it item, e int64) {
	old := it.exp()
	if old == 0 && e != 0 {
		s.nexp++
	} else if old != 0 && e == 0 {
		s.nexp--
	}
	it.setExpRaw(e)
}

func (s *shard) releaseObj(it item) {
	if it.typ() != typStr {
		i := it.objIdx()
		s.objs[i] = nil
		s.freeObjs = append(s.freeObjs, i)
	}
}

func (s *shard) newObj(o any) uint32 {
	if n := len(s.freeObjs); n > 0 {
		i := s.freeObjs[n-1]
		s.freeObjs = s.freeObjs[:n-1]
		s.objs[i] = o
		return i
	}
	s.objs = append(s.objs, o)
	return uint32(len(s.objs) - 1)
}

func (s *shard) del(idx int, it item) {
	s.addUsed(-s.cost(it))
	if it.exp() != 0 {
		s.nexp--
	}
	s.releaseObj(it)
	s.freeItem(it)
	mask := len(s.slots) - 1
	if s.slots[(idx+1)&mask] == 0 {
		s.slots[idx] = 0
	} else {
		s.slots[idx] = tomb
		s.tombs++
	}
	s.count--
}

// replace swaps the item in slot idx for nw (same key). Accounting of the old
// item's cost and expiry is transferred; old is freed.
func (s *shard) replace(idx int, old, nw item) {
	s.addUsed(s.cost(nw) - s.cost(old))
	if old.exp() != 0 {
		s.nexp--
	}
	if nw.exp() != 0 {
		s.nexp++
	}
	s.releaseObj(old)
	s.freeItem(old)
	s.slots[idx] = s.slots[idx]&^ptrMask | uint64(nw)
}

// setStr stores a string value under key. idx/it/ins come from get.
func (s *shard) setStr(idx int, it item, ins int, h uint64, key, val []byte, exp int64, now int64) {
	if it != 0 {
		if it.typ() == typStr && len(val) <= it.capacity() && it.cls() != clsBig {
			d := len(val) - it.vlen()
			it.setVlen(len(val))
			copy(it.val(), val)
			s.setExp(it, exp)
			s.addUsed(int64(d))
			return
		}
		nw := s.newItem(key, len(val), typStr, now)
		copy(nw.val(), val)
		nw.setExpRaw(exp)
		s.replace(idx, it, nw)
		return
	}
	nw := s.newItem(key, len(val), typStr, now)
	copy(nw.val(), val)
	nw.setExpRaw(exp)
	s.insert(ins, h, nw)
}

func (s *shard) newContainer(ins int, h uint64, key []byte, typ uint8, o any, now int64) item {
	it := s.newItem(key, 4, typ, now)
	it.setObjIdx(s.newObj(o))
	s.insert(ins, h, it)
	return it
}

func (s *shard) flush() {
	for _, v := range s.slots {
		if v > tomb {
			it := item(v & ptrMask)
			if it.cls() == clsBig {
				s.freeItem(it)
			}
		}
	}
	s.addUsed(-s.used)
	s.slots = make([]uint64, 8)
	s.count, s.tombs, s.nexp, s.expCur = 0, 0, 0, 0
	s.free = [80]uintptr{}
	s.bumps = [80]bump{}
	s.objs = nil
	s.freeObjs = nil
}

// ---------------------------------------------------------------- expiry

func expiryLoop() {
	t := time.NewTicker(100 * time.Millisecond)
	for range t.C {
		for i := range shards {
			s := &shards[i]
			s.mu.Lock()
			if s.nexp == 0 {
				if s.count*8 < len(s.slots) && len(s.slots) > 64 {
					s.grow0()
				}
				s.mu.Unlock()
				continue
			}
			budget := len(s.slots)/8 + 64
			for budget > 0 {
				now := time.Now().UnixMilli()
				for n := 0; n < 1024 && budget > 0; n++ {
					budget--
					if s.expCur >= len(s.slots) {
						s.expCur = 0
					}
					v := s.slots[s.expCur]
					if v > tomb {
						it := item(v & ptrMask)
						if e := it.exp(); e != 0 && e <= now {
							s.del(s.expCur, it)
						}
					}
					s.expCur++
				}
				if s.count*8 < len(s.slots) && len(s.slots) > 64 {
					s.grow0()
				}
				s.mu.Unlock()
				s.mu.Lock()
			}
			s.mu.Unlock()
		}
	}
}

// grow0 shrinks the table to fit the current count.
func (s *shard) grow0() {
	n := 8
	for n*3 < (s.count+1)*4*2 {
		n *= 2
	}
	s.rehash(n)
}

// ---------------------------------------------------------------- eviction

func evict(protect [][]byte) {
	for globalUsed.Load() > maxMemory {
		if !evictOne(protect) {
			return
		}
	}
}

func isProtected(k []byte, protect [][]byte) bool {
	for _, p := range protect {
		if bytes.Equal(k, p) {
			return true
		}
	}
	return false
}

func evictOne(protect [][]byte) bool {
	for tries := 0; tries < nShards*4; tries++ {
		s := &shards[rand.IntN(nShards)]
		s.mu.Lock()
		if s.count == 0 {
			s.mu.Unlock()
			continue
		}
		n := len(s.slots)
		mask := n - 1
		i := rand.IntN(n)
		best := -1
		var bestLRU uint32
		var bestIt item
		sampled := 0
		for scanned := 0; scanned < n && sampled < 16; scanned++ {
			v := s.slots[i]
			if v > tomb {
				it := item(v & ptrMask)
				if !isProtected(it.key(), protect) {
					sampled++
					if l := it.lru(); best < 0 || l < bestLRU {
						best, bestLRU, bestIt = i, l, it
					}
				}
			}
			i = (i + 1) & mask
		}
		if best >= 0 {
			s.del(best, bestIt)
			s.mu.Unlock()
			return true
		}
		s.mu.Unlock()
	}
	return false
}

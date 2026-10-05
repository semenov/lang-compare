package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/klauspost/compress/zstd"
)

const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

func mmapFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() == 0 {
		return []byte{}, nil
	}
	return syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
}

func asU32(b []byte) []uint32 {
	if len(b) < 4 {
		return nil
	}
	return unsafe.Slice((*uint32)(unsafe.Pointer(&b[0])), len(b)/4)
}

func asU64(b []byte) []uint64 {
	if len(b) < 8 {
		return nil
	}
	return unsafe.Slice((*uint64)(unsafe.Pointer(&b[0])), len(b)/8)
}

type dict struct {
	count   int
	entries []byte // 24-byte entries: termOff u32, df u32, docOff u64, posOff u64; plus sentinel
	blob    []byte
	docs    []byte
	pos     []byte
}

type termEntry struct {
	df     uint32
	docOff uint64
	posOff uint64
	d      *dict
}

func (d *dict) term(i int) []byte {
	a := binary.LittleEndian.Uint32(d.entries[24*i:])
	b := binary.LittleEndian.Uint32(d.entries[24*i+24:])
	return d.blob[a:b]
}

func (d *dict) lookup(t []byte) (termEntry, bool) {
	lo, hi := 0, d.count
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if bytes.Compare(d.term(m), t) < 0 {
			lo = m + 1
		} else {
			hi = m
		}
	}
	if lo >= d.count || !bytes.Equal(d.term(lo), t) {
		return termEntry{}, false
	}
	e := d.entries[24*lo:]
	return termEntry{
		df:     binary.LittleEndian.Uint32(e[4:]),
		docOff: binary.LittleEndian.Uint64(e[8:]),
		posOff: binary.LittleEndian.Uint64(e[16:]),
		d:      d,
	}, true
}

type index struct {
	n        uint64
	avgdl    float64
	dicts    [numBuckets]*dict
	ids      []uint32
	dls      []uint32
	titles   []byte
	titleOff []uint64
	store    []byte
	storeIdx []byte // (firstDoc u32, off u64) entries, 12 bytes each
	nBlocks  int
	idmap    []uint32 // pairs (id, docnum)
	dec      *zstd.Decoder
	normC1   float64
	normC2   float64
}

func openIndex(dir string) (*index, error) {
	ix := &index{}
	mb, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	var meta indexMeta
	if err := json.Unmarshal(mb, &meta); err != nil {
		return nil, err
	}
	if meta.Buckets != numBuckets {
		return nil, fmt.Errorf("bucket count mismatch")
	}
	ix.n = meta.N
	if meta.N > 0 {
		ix.avgdl = float64(meta.TotalTokens) / float64(meta.N)
	}
	m := func(name string) []byte {
		if err != nil {
			return nil
		}
		var b []byte
		b, err = mmapFile(filepath.Join(dir, name))
		return b
	}
	for b := 0; b < numBuckets; b++ {
		raw := m(fmt.Sprintf("dict%d.bin", b))
		d := &dict{docs: m(fmt.Sprintf("docs%d.bin", b)), pos: m(fmt.Sprintf("pos%d.bin", b))}
		if err != nil {
			return nil, err
		}
		d.entries = raw
		d.blob = m(fmt.Sprintf("terms%d.bin", b))
		if err != nil {
			return nil, err
		}
		d.count = len(raw)/24 - 1
		ix.dicts[b] = d
	}
	ix.ids = asU32(m("ids.bin"))
	ix.dls = asU32(m("dl.bin"))
	ix.titles = m("titles.bin")
	ix.titleOff = asU64(m("titleoff.bin"))
	ix.store = m("store.bin")
	ix.storeIdx = m("storeidx.bin")
	ix.idmap = asU32(m("idmap.bin"))
	if err != nil {
		return nil, err
	}
	ix.nBlocks = len(ix.storeIdx)/12 - 1
	ix.dec, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	if err != nil {
		return nil, err
	}
	if ix.avgdl > 0 {
		ix.normC1 = bm25K1 * (1 - bm25B)
		ix.normC2 = bm25K1 * bm25B / ix.avgdl
	}
	return ix, nil
}

func (ix *index) lookup(t string) (termEntry, bool) {
	tb := unsafe.Slice(unsafe.StringData(t), len(t))
	return ix.dicts[termBucket(tb)].lookup(tb)
}

func (ix *index) title(doc uint32) []byte {
	return ix.titles[ix.titleOff[doc]:ix.titleOff[doc+1]]
}

func (ix *index) docByID(id uint32) (uint32, bool) {
	n := len(ix.idmap) / 2
	i := sort.Search(n, func(i int) bool { return ix.idmap[2*i] >= id })
	if i < n && ix.idmap[2*i] == id {
		return ix.idmap[2*i+1], true
	}
	return 0, false
}

func (ix *index) docText(doc uint32) ([]byte, error) {
	si := ix.storeIdx
	b := sort.Search(ix.nBlocks, func(i int) bool { return binary.LittleEndian.Uint32(si[12*(i+1):]) > doc })
	first := binary.LittleEndian.Uint32(si[12*b:])
	off := binary.LittleEndian.Uint64(si[12*b+4:])
	end := binary.LittleEndian.Uint64(si[12*(b+1)+4:])
	raw, err := ix.dec.DecodeAll(ix.store[off:end], nil)
	if err != nil {
		return nil, err
	}
	p := 0
	for d := first; ; d++ {
		var l uint64
		l, p = uvarint(raw, p)
		if d == doc {
			return raw[p : p+int(l)], nil
		}
		p += int(l)
	}
}

// ---- postings cursor ----

type cursor struct {
	df      uint32
	nblocks int
	skip    []byte
	blocks  []byte
	pos     []byte
	posOff0 uint64
	blk     int
	n       int
	i       int
	doc     uint32
	docs    [blockSize]uint32
	tfs     [blockSize]uint32
	pj      int
	pp      int
}

func (c *cursor) init(e termEntry) {
	c.df = e.df
	c.nblocks = int((e.df + blockSize - 1) / blockSize)
	data := e.d.docs[e.docOff:]
	if c.nblocks > 1 {
		c.skip = data[:16*c.nblocks]
		data = data[16*c.nblocks:]
	} else {
		c.skip = nil
	}
	c.blocks = data
	c.pos = e.d.pos
	c.posOff0 = e.posOff
	c.loadBlock(0)
	c.i = 0
	c.doc = c.docs[0]
}

func (c *cursor) lastDoc(b int) uint32 {
	if c.skip == nil {
		return endDoc - 1
	}
	return binary.LittleEndian.Uint32(c.skip[16*b:])
}

func (c *cursor) loadBlock(b int) {
	c.blk = b
	var off int
	var prev uint32
	if c.skip != nil {
		off = int(binary.LittleEndian.Uint32(c.skip[16*b+4:]))
		c.pp = int(binary.LittleEndian.Uint64(c.skip[16*b+8:]))
		if b > 0 {
			prev = binary.LittleEndian.Uint32(c.skip[16*(b-1):])
		}
	} else {
		c.pp = int(c.posOff0)
	}
	n := blockSize
	if b == c.nblocks-1 {
		n = int(c.df) - blockSize*(c.nblocks-1)
	}
	c.n = n
	data := c.blocks
	p := off
	for i := 0; i < n; i++ {
		var v uint64
		v, p = uvarint(data, p)
		prev += uint32(v)
		c.docs[i] = prev
	}
	for i := 0; i < n; i++ {
		var v uint64
		v, p = uvarint(data, p)
		c.tfs[i] = uint32(v)
	}
	c.pj = 0
}

func (c *cursor) next() {
	c.i++
	if c.i >= c.n {
		if c.blk+1 >= c.nblocks {
			c.doc = endDoc
			c.i = c.n
			return
		}
		c.loadBlock(c.blk + 1)
		c.i = 0
	}
	c.doc = c.docs[c.i]
}

// advance moves to the first doc >= target.
func (c *cursor) advance(target uint32) {
	if c.doc >= target {
		return
	}
	if target > c.lastDoc(c.blk) {
		lo, hi := c.blk+1, c.nblocks
		for lo < hi {
			m := (lo + hi) / 2
			if c.lastDoc(m) >= target {
				hi = m
			} else {
				lo = m + 1
			}
		}
		if lo >= c.nblocks {
			c.doc = endDoc
			c.blk = c.nblocks - 1
			c.i = c.n
			return
		}
		c.loadBlock(lo)
		c.i = 0
	}
	i := c.i
	for i < c.n && c.docs[i] < target {
		i++
	}
	if i >= c.n {
		c.i = c.n
		c.doc = endDoc
		return
	}
	c.i = i
	c.doc = c.docs[i]
}

func (c *cursor) tf() uint32 { return c.tfs[c.i] }

// posStart returns the offset of the current doc's positions in c.pos.
func (c *cursor) posStart() int {
	if c.pj < c.i {
		var cnt int
		for j := c.pj; j < c.i; j++ {
			cnt += int(c.tfs[j])
		}
		c.pp = skipVarints(c.pos, c.pp, cnt)
		c.pj = c.i
	}
	return c.pp
}

// ---- HTTP ----

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h["Content-Type"] = []string{"application/json"}
	h["Content-Length"] = []string{strconv.Itoa(len(body))}
	w.WriteHeader(status)
	w.Write(body)
}

func errJSON(w http.ResponseWriter, status int, msg string) {
	b, _ := json.Marshal(map[string]string{"error": msg})
	writeJSON(w, status, b)
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 0, 4096); return &b }}

func runServe(dir string) error {
	ix, err := openIndex(dir)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		qv := r.URL.Query()
		qs, ok := qv["q"]
		if !ok || len(qs) == 0 {
			errJSON(w, 400, "missing q")
			return
		}
		k := 10
		if ks, ok := qv["k"]; ok && len(ks) > 0 {
			v, err := strconv.Atoi(ks[0])
			if err != nil || v < 1 || v > 1000 {
				errJSON(w, 400, "invalid k")
				return
			}
			k = v
		}
		total, hits := ix.search(qs[0], k)
		bp := bufPool.Get().(*[]byte)
		b := (*bp)[:0]
		b = append(b, `{"total":`...)
		b = strconv.AppendInt(b, int64(total), 10)
		b = append(b, `,"hits":[`...)
		for i, h := range hits {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, `{"id":`...)
			b = strconv.AppendUint(b, uint64(h.id), 10)
			b = append(b, `,"title":"`...)
			b = append(b, ix.title(h.doc)...)
			b = append(b, `","score":`...)
			b = strconv.AppendFloat(b, h.score, 'g', -1, 64)
			b = append(b, '}')
		}
		b = append(b, "]}"...)
		writeJSON(w, 200, b)
		*bp = b
		bufPool.Put(bp)
	})
	mux.HandleFunc("/doc/", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseUint(strings.TrimPrefix(r.URL.Path, "/doc/"), 10, 32)
		if err != nil {
			errJSON(w, 404, "not found")
			return
		}
		doc, ok := ix.docByID(uint32(id))
		if !ok {
			errJSON(w, 404, "not found")
			return
		}
		text, err := ix.docText(doc)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		b := make([]byte, 0, len(text)+256)
		b = append(b, `{"id":`...)
		b = strconv.AppendUint(b, id, 10)
		b = append(b, `,"title":"`...)
		b = append(b, ix.title(doc)...)
		b = append(b, `","text":"`...)
		b = append(b, text...)
		b = append(b, `"}`...)
		writeJSON(w, 200, b)
	})
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	ln, err := net.Listen("tcp", "0.0.0.0:"+port)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux}
	return srv.Serve(ln)
}

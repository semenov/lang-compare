package main

import (
	"bufio"
	"bytes"
	"container/heap"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const (
	chunkSize      = 8 << 20  // raw corpus bytes per chunk (= one run)
	storeBlockSize = 64 << 10 // uncompressed doc store block
)

type chunk struct {
	idx  int
	base uint32 // first docnum
	n    int    // docs in chunk
	data []byte
}

type storeBlock struct {
	firstDoc uint32
	data     []byte
}

type chunkResult struct {
	idx       int
	base      uint32
	ids       []uint32
	dls       []uint32
	titleBlob []byte
	titleEnds []uint32 // end offsets within titleBlob
	blocks    []storeBlock
	tokens    uint64
}

type runMeta struct {
	file int
	off  [numBuckets]int64
	len  [numBuckets]int64
}

type indexMeta struct {
	N           uint64 `json:"n"`
	TotalTokens uint64 `json:"total_tokens"`
	Buckets     int    `json:"buckets"`
}

func countDocs(data []byte) int {
	n := 0
	for len(data) > 0 {
		j := bytes.IndexByte(data, '\n')
		var line []byte
		if j < 0 {
			line, data = data, nil
		} else {
			line, data = data[:j], data[j+1:]
		}
		if !isBlank(line) {
			n++
		}
	}
	return n
}

func runIndex(corpus, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp(dir, "tmp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	f, err := os.Open(corpus)
	if err != nil {
		return err
	}
	defer f.Close()

	// Trade a little CPU for a lower peak heap while indexing.
	debug.SetGCPercent(60)
	nw := runtime.NumCPU()
	chunks := make(chan chunk, 2)
	results := make(chan chunkResult, nw)
	free := make(chan []byte, nw+2)
	for i := 0; i < nw+2; i++ {
		free <- nil
	}

	var readErr error
	go func() {
		defer close(chunks)
		var carry []byte
		var base uint32
		idx := 0
		eof := false
		for !eof {
			buf := <-free
			target := chunkSize
			if len(carry) >= chunkSize/2 {
				target = 2*len(carry) + chunkSize
			}
			if cap(buf) < target {
				buf = make([]byte, 0, target)
			}
			buf = append(buf[:0], carry...)
			for len(buf) < target && !eof {
				n, err := f.Read(buf[len(buf):cap(buf)])
				buf = buf[:len(buf)+n]
				if err == io.EOF {
					eof = true
				} else if err != nil {
					readErr = err
					return
				}
			}
			var data []byte
			if eof {
				data, carry = buf, nil
			} else {
				j := bytes.LastIndexByte(buf, '\n')
				if j < 0 {
					// line longer than buffer: grow
					carry = append([]byte(nil), buf...)
					free <- buf[:0]
					continue
				}
				data = buf[:j+1]
				carry = append(carry[:0:0], buf[j+1:]...)
			}
			n := countDocs(data)
			chunks <- chunk{idx: idx, base: base, n: n, data: data}
			idx++
			base += uint32(n)
		}
	}()

	runFiles := make([]*os.File, nw)
	for i := range runFiles {
		rf, err := os.Create(filepath.Join(tmpDir, fmt.Sprintf("run%d", i)))
		if err != nil {
			return err
		}
		runFiles[i] = rf
	}
	var runsMu sync.Mutex
	runs := map[int]runMeta{}
	var wg sync.WaitGroup
	var workErr error
	var errMu sync.Mutex
	for w := 0; w < nw; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			wk := newWorker(w, runFiles[w])
			for c := range chunks {
				res, rm, err := wk.process(c)
				free <- c.data[:0]
				if err != nil {
					errMu.Lock()
					workErr = err
					errMu.Unlock()
					continue
				}
				runsMu.Lock()
				runs[c.idx] = rm
				runsMu.Unlock()
				results <- res
			}
		}(w)
	}
	go func() { wg.Wait(); close(results) }()

	meta, ids, err := writeDocData(dir, results)
	if err != nil {
		return err
	}
	if readErr != nil {
		return readErr
	}
	if workErr != nil {
		return workErr
	}
	for _, rf := range runFiles {
		if err := rf.Sync(); err != nil {
			return err
		}
	}
	if err := writeIDMap(dir, ids); err != nil {
		return err
	}
	runList := make([]runMeta, len(runs))
	for i := range runList {
		rm, ok := runs[i]
		if !ok {
			return fmt.Errorf("missing run %d", i)
		}
		runList[i] = rm
	}
	if err := mergeRuns(dir, runFiles, runList); err != nil {
		return err
	}
	for _, rf := range runFiles {
		rf.Close()
	}
	mb, _ := json.Marshal(meta)
	return os.WriteFile(filepath.Join(dir, "meta.json"), mb, 0o644)
}

// ---- worker: parse, tokenize, build in-memory run ----

type worker struct {
	id  int
	out *os.File
	off int64
	enc *zstd.Encoder

	terms map[string]uint32
	strs  []string
	post  [][]byte
	last  []uint32

	stamp []uint32
	cnt   []uint32
	fill  []uint32

	tids     []uint32
	distinct []uint32
	posFlat  []uint32
	scratch  []byte
	tok      tokenizer
	wbuf     *bufio.Writer
}

func newWorker(id int, out *os.File) *worker {
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(storeBlockSize*2), zstd.WithLowerEncoderMem(true))
	return &worker{id: id, out: out, enc: enc, terms: make(map[string]uint32), wbuf: bufio.NewWriterSize(out, 1<<20)}
}

func (w *worker) tokenizeInto(text []byte) {
	w.tok.reset(text)
	for {
		tok, ok := w.tok.next()
		if !ok {
			return
		}
		tid, ok := w.terms[string(tok)]
		if !ok {
			tid = uint32(len(w.strs))
			s := string(tok)
			w.terms[s] = tid
			w.strs = append(w.strs, s)
			if len(w.post) < cap(w.post) {
				// reuse the buffer left over from a previous run
				w.post = w.post[:len(w.post)+1]
				w.post[tid] = w.post[tid][:0]
			} else {
				w.post = append(w.post, nil)
			}
			w.last = append(w.last, 0)
			if int(tid) >= len(w.stamp) {
				w.stamp = append(w.stamp, 0)
				w.cnt = append(w.cnt, 0)
				w.fill = append(w.fill, 0)
			}
		}
		w.tids = append(w.tids, tid)
	}
}

func (w *worker) addDoc(doc uint32) {
	stampv := doc + 1
	w.distinct = w.distinct[:0]
	for _, t := range w.tids {
		if w.stamp[t] != stampv {
			w.stamp[t] = stampv
			w.cnt[t] = 0
			w.distinct = append(w.distinct, t)
		}
		w.cnt[t]++
	}
	var sum uint32
	for _, t := range w.distinct {
		w.fill[t] = sum
		sum += w.cnt[t]
	}
	if cap(w.posFlat) < len(w.tids) {
		w.posFlat = make([]uint32, len(w.tids)*2)
	}
	pf := w.posFlat[:len(w.tids)]
	for p, t := range w.tids {
		pf[w.fill[t]] = uint32(p)
		w.fill[t]++
	}
	for _, t := range w.distinct {
		c := w.cnt[t]
		start := w.fill[t] - c
		b := w.post[t]
		b = putUvarint(b, uint64(doc-w.last[t]))
		w.last[t] = doc
		b = putUvarint(b, uint64(c))
		prev := uint32(0)
		for _, p := range pf[start : start+c] {
			b = putUvarint(b, uint64(p-prev))
			prev = p
		}
		w.post[t] = b
	}
}

func (w *worker) process(c chunk) (chunkResult, runMeta, error) {
	res := chunkResult{idx: c.idx, base: c.base}
	res.ids = make([]uint32, 0, c.n)
	res.dls = make([]uint32, 0, c.n)
	res.titleEnds = make([]uint32, 0, c.n)
	var blk []byte
	blkFirst := c.base
	flushBlock := func() {
		if len(blk) == 0 {
			return
		}
		comp := w.enc.EncodeAll(blk, make([]byte, 0, len(blk)/2))
		res.blocks = append(res.blocks, storeBlock{firstDoc: blkFirst, data: comp})
		blk = blk[:0]
	}
	doc := c.base
	data := c.data
	for len(data) > 0 {
		j := bytes.IndexByte(data, '\n')
		var line []byte
		if j < 0 {
			line, data = data, nil
		} else {
			line, data = data[:j], data[j+1:]
		}
		if isBlank(line) {
			continue
		}
		d, ok := parseLine(line)
		if !ok {
			return res, runMeta{}, fmt.Errorf("invalid JSON line for doc #%d", doc)
		}
		w.tids = w.tids[:0]
		if d.titleEsc {
			w.scratch = unescape(w.scratch[:0], d.title)
			w.tokenizeInto(w.scratch)
		} else {
			w.tokenizeInto(d.title)
		}
		if d.textEsc {
			w.scratch = unescape(w.scratch[:0], d.text)
			w.tokenizeInto(w.scratch)
		} else {
			w.tokenizeInto(d.text)
		}
		w.addDoc(doc)
		res.ids = append(res.ids, d.id)
		res.dls = append(res.dls, uint32(len(w.tids)))
		res.tokens += uint64(len(w.tids))
		res.titleBlob = append(res.titleBlob, d.title...)
		res.titleEnds = append(res.titleEnds, uint32(len(res.titleBlob)))
		if len(blk) == 0 {
			blkFirst = doc
		}
		blk = putUvarint(blk, uint64(len(d.text)))
		blk = append(blk, d.text...)
		if len(blk) >= storeBlockSize {
			flushBlock()
		}
		doc++
	}
	flushBlock()
	rm, err := w.flushRun()
	return res, rm, err
}

func (w *worker) flushRun() (runMeta, error) {
	rm := runMeta{file: w.id}
	var buckets [numBuckets][]uint32
	for tid, s := range w.strs {
		b := termBucket([]byte(s))
		buckets[b] = append(buckets[b], uint32(tid))
	}
	var hdr []byte
	for b := 0; b < numBuckets; b++ {
		tl := buckets[b]
		slices.SortFunc(tl, func(x, y uint32) int {
			sx, sy := w.strs[x], w.strs[y]
			if sx < sy {
				return -1
			} else if sx > sy {
				return 1
			}
			return 0
		})
		rm.off[b] = w.off
		for _, t := range tl {
			p := w.post[t]
			hdr = putUvarint(hdr[:0], uint64(len(w.strs[t])))
			hdr = append(hdr, w.strs[t]...)
			hdr = putUvarint(hdr, uint64(len(p)))
			w.wbuf.Write(hdr)
			w.wbuf.Write(p)
			w.off += int64(len(hdr) + len(p))
		}
		rm.len[b] = w.off - rm.off[b]
	}
	if err := w.wbuf.Flush(); err != nil {
		return rm, err
	}
	clear(w.terms)
	w.strs = w.strs[:0]
	w.post = w.post[:0]
	w.last = w.last[:0]
	return rm, nil
}

// ---- ordered writer for per-doc data ----

func writeDocData(dir string, results chan chunkResult) (indexMeta, []uint32, error) {
	var meta indexMeta
	meta.Buckets = numBuckets
	create := func(name string) (*os.File, *bufio.Writer) {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			panic(err)
		}
		return f, bufio.NewWriterSize(f, 1<<20)
	}
	idsF, idsW := create("ids.bin")
	dlF, dlW := create("dl.bin")
	tF, tW := create("titles.bin")
	toF, toW := create("titleoff.bin")
	sF, sW := create("store.bin")
	siF, siW := create("storeidx.bin")
	var allIDs []uint32
	var titleOff, storeOff uint64
	var u8 [8]byte
	var u4 [4]byte
	binary.LittleEndian.PutUint64(u8[:], 0)
	toW.Write(u8[:])
	pending := map[int]chunkResult{}
	next := 0
	emit := func(r chunkResult) {
		for i, id := range r.ids {
			binary.LittleEndian.PutUint32(u4[:], id)
			idsW.Write(u4[:])
			binary.LittleEndian.PutUint32(u4[:], r.dls[i])
			dlW.Write(u4[:])
			binary.LittleEndian.PutUint64(u8[:], titleOff+uint64(r.titleEnds[i]))
			toW.Write(u8[:])
		}
		allIDs = append(allIDs, r.ids...)
		tW.Write(r.titleBlob)
		titleOff += uint64(len(r.titleBlob))
		for _, b := range r.blocks {
			binary.LittleEndian.PutUint32(u4[:], b.firstDoc)
			siW.Write(u4[:])
			binary.LittleEndian.PutUint64(u8[:], storeOff)
			siW.Write(u8[:])
			sW.Write(b.data)
			storeOff += uint64(len(b.data))
		}
		meta.N += uint64(len(r.ids))
		meta.TotalTokens += r.tokens
	}
	for r := range results {
		pending[r.idx] = r
		for {
			p, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			emit(p)
			next++
		}
	}
	// terminal store index entry
	binary.LittleEndian.PutUint32(u4[:], uint32(meta.N))
	siW.Write(u4[:])
	binary.LittleEndian.PutUint64(u8[:], storeOff)
	siW.Write(u8[:])
	for _, p := range []struct {
		f *os.File
		w *bufio.Writer
	}{{idsF, idsW}, {dlF, dlW}, {tF, tW}, {toF, toW}, {sF, sW}, {siF, siW}} {
		if err := p.w.Flush(); err != nil {
			return meta, nil, err
		}
		if err := p.f.Close(); err != nil {
			return meta, nil, err
		}
	}
	return meta, allIDs, nil
}

func writeIDMap(dir string, ids []uint32) error {
	pairs := make([]uint64, len(ids))
	for d, id := range ids {
		pairs[d] = uint64(id)<<32 | uint64(d)
	}
	slices.Sort(pairs)
	buf := make([]byte, 8*len(pairs))
	for i, p := range pairs {
		binary.LittleEndian.PutUint32(buf[8*i:], uint32(p>>32))
		binary.LittleEndian.PutUint32(buf[8*i+4:], uint32(p))
	}
	return os.WriteFile(filepath.Join(dir, "idmap.bin"), buf, 0o644)
}

// ---- merge ----

type runCursor struct {
	r    *bufio.Reader
	run  int
	term []byte
	data []byte
}

func (c *runCursor) advance() (bool, error) {
	tl, err := binary.ReadUvarint(c.r)
	if err == io.EOF {
		return false, nil
	} else if err != nil {
		return false, err
	}
	c.term = slices.Grow(c.term[:0], int(tl))[:tl]
	if _, err := io.ReadFull(c.r, c.term); err != nil {
		return false, err
	}
	dl, err := binary.ReadUvarint(c.r)
	if err != nil {
		return false, err
	}
	c.data = slices.Grow(c.data[:0], int(dl))[:dl]
	if _, err := io.ReadFull(c.r, c.data); err != nil {
		return false, err
	}
	return true, nil
}

type cursorHeap []*runCursor

func (h cursorHeap) Len() int { return len(h) }
func (h cursorHeap) Less(i, j int) bool {
	c := bytes.Compare(h[i].term, h[j].term)
	if c != 0 {
		return c < 0
	}
	return h[i].run < h[j].run
}
func (h cursorHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *cursorHeap) Push(x any)   { *h = append(*h, x.(*runCursor)) }
func (h *cursorHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

func mergeRuns(dir string, files []*os.File, runs []runMeta) error {
	par := runtime.NumCPU()
	if par > numBuckets {
		par = numBuckets
	}
	bufSize := (128 << 20) / (par * max(1, len(runs)))
	bufSize = min(max(bufSize, 4096), 1<<20)
	sem := make(chan struct{}, par)
	errs := make([]error, numBuckets)
	var wg sync.WaitGroup
	for b := 0; b < numBuckets; b++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(b int) {
			defer wg.Done()
			defer func() { <-sem }()
			errs[b] = mergeBucket(dir, b, files, runs, bufSize)
		}(b)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

type countingWriter struct {
	w   *bufio.Writer
	off uint64
}

func (c *countingWriter) Write(p []byte) {
	c.w.Write(p)
	c.off += uint64(len(p))
}

func mergeBucket(dir string, b int, files []*os.File, runs []runMeta, bufSize int) error {
	h := cursorHeap{}
	for i, rm := range runs {
		if rm.len[b] == 0 {
			continue
		}
		c := &runCursor{r: bufio.NewReaderSize(io.NewSectionReader(files[rm.file], rm.off[b], rm.len[b]), bufSize), run: i}
		ok, err := c.advance()
		if err != nil {
			return err
		}
		if ok {
			h = append(h, c)
		}
	}
	heap.Init(&h)

	docF, err := os.Create(filepath.Join(dir, fmt.Sprintf("docs%d.bin", b)))
	if err != nil {
		return err
	}
	posF, err := os.Create(filepath.Join(dir, fmt.Sprintf("pos%d.bin", b)))
	if err != nil {
		return err
	}
	docW := &countingWriter{w: bufio.NewWriterSize(docF, 1<<20)}
	posW := &countingWriter{w: bufio.NewWriterSize(posF, 1<<20)}

	termF, err := os.Create(filepath.Join(dir, fmt.Sprintf("terms%d.bin", b)))
	if err != nil {
		return err
	}
	entF, err := os.Create(filepath.Join(dir, fmt.Sprintf("dict%d.bin", b)))
	if err != nil {
		return err
	}
	termW := &countingWriter{w: bufio.NewWriterSize(termF, 256<<10)}
	entW := &countingWriter{w: bufio.NewWriterSize(entF, 256<<10)}

	var group []*runCursor
	var blocksBuf, skipBuf []byte
	var bdocs, btfs [blockSize]uint32
	var ent [24]byte
	for h.Len() > 0 {
		group = group[:0]
		first := heap.Pop(&h).(*runCursor)
		group = append(group, first)
		for h.Len() > 0 && bytes.Equal(h[0].term, first.term) {
			group = append(group, heap.Pop(&h).(*runCursor))
		}
		// encode merged postings; runs in group are in increasing run order
		blocksBuf = blocksBuf[:0]
		skipBuf = skipBuf[:0]
		var df uint32
		n := 0
		var prevLast uint32
		var blockPos uint64
		firstPos := posW.off
		flush := func() {
			off := uint32(len(blocksBuf))
			prev := prevLast
			for i := 0; i < n; i++ {
				blocksBuf = putUvarint(blocksBuf, uint64(bdocs[i]-prev))
				prev = bdocs[i]
			}
			for i := 0; i < n; i++ {
				blocksBuf = putUvarint(blocksBuf, uint64(btfs[i]))
			}
			skipBuf = binary.LittleEndian.AppendUint32(skipBuf, bdocs[n-1])
			skipBuf = binary.LittleEndian.AppendUint32(skipBuf, off)
			skipBuf = binary.LittleEndian.AppendUint64(skipBuf, blockPos)
			prevLast = bdocs[n-1]
			n = 0
		}
		for _, c := range group {
			data := c.data
			var doc uint32
			i := 0
			for i < len(data) {
				var v uint64
				v, i = uvarint(data, i)
				doc += uint32(v)
				var tf uint64
				tf, i = uvarint(data, i)
				pstart := i
				i = skipVarints(data, i, int(tf))
				if n == 0 {
					blockPos = posW.off
				}
				posW.Write(data[pstart:i])
				bdocs[n] = doc
				btfs[n] = uint32(tf)
				n++
				df++
				if n == blockSize {
					flush()
				}
			}
		}
		if n > 0 {
			flush()
		}
		docOff := docW.off
		if len(skipBuf) > 16 {
			docW.Write(skipBuf)
		}
		docW.Write(blocksBuf)

		binary.LittleEndian.PutUint32(ent[0:], uint32(termW.off))
		binary.LittleEndian.PutUint32(ent[4:], df)
		binary.LittleEndian.PutUint64(ent[8:], docOff)
		binary.LittleEndian.PutUint64(ent[16:], firstPos)
		entW.Write(ent[:])
		termW.Write(first.term)

		for _, c := range group {
			ok, err := c.advance()
			if err != nil {
				return err
			}
			if ok {
				heap.Push(&h, c)
			}
		}
	}
	// sentinel entry: end of the last term
	clear(ent[:])
	binary.LittleEndian.PutUint32(ent[0:], uint32(termW.off))
	entW.Write(ent[:])
	for _, x := range []struct {
		f *os.File
		w *countingWriter
	}{{docF, docW}, {posF, posW}, {termF, termW}, {entF, entW}} {
		if err := x.w.w.Flush(); err != nil {
			return err
		}
		if err := x.f.Close(); err != nil {
			return err
		}
	}
	return nil
}

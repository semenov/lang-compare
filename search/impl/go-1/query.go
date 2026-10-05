package main

import (
	"math"
	"runtime"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// ---- query parsing ----

type clause struct {
	neg    bool
	tokens []string
}

func parseQuery(q string) []clause {
	var out []clause
	i := 0
	for {
		for i < len(q) {
			r, sz := utf8.DecodeRuneInString(q[i:])
			if !unicode.IsSpace(r) {
				break
			}
			i += sz
		}
		if i >= len(q) {
			return out
		}
		c := clause{}
		if q[i] == '-' {
			c.neg = true
			i++
		}
		var text string
		if i < len(q) && q[i] == '"' {
			i++
			j := strings.IndexByte(q[i:], '"')
			if j < 0 {
				text = q[i:]
				i = len(q)
			} else {
				text = q[i : i+j]
				i += j + 1
			}
		} else {
			j := i
			for j < len(q) {
				r, sz := utf8.DecodeRuneInString(q[j:])
				if unicode.IsSpace(r) {
					break
				}
				j += sz
			}
			text = q[i:j]
			i = j
		}
		c.tokens = tokenizeString(text)
		if len(c.tokens) > 0 {
			out = append(out, c)
		}
	}
}

// ---- top-k ----

type hit struct {
	score float64
	id    uint32
	doc   uint32
}

// worse reports whether a ranks below b.
func worse(a, b hit) bool {
	if a.score != b.score {
		return a.score < b.score
	}
	return a.id > b.id
}

type topK struct {
	k int
	h []hit
}

func (t *topK) push(x hit) {
	h := t.h
	if len(h) < t.k {
		h = append(h, x)
		i := len(h) - 1
		for i > 0 {
			p := (i - 1) / 2
			if !worse(h[i], h[p]) {
				break
			}
			h[i], h[p] = h[p], h[i]
			i = p
		}
		t.h = h
		return
	}
	if !worse(h[0], x) {
		return
	}
	h[0] = x
	i := 0
	n := len(h)
	for {
		l := 2*i + 1
		if l >= n {
			break
		}
		m := l
		if r := l + 1; r < n && worse(h[r], h[l]) {
			m = r
		}
		if !worse(h[m], h[i]) {
			break
		}
		h[i], h[m] = h[m], h[i]
		i = m
	}
}

// ---- phrase matching over streamed positions ----

type pstream struct {
	data []byte
	p    int
	left uint32
	cur  int64
}

func (s *pstream) next() bool {
	if s.left == 0 {
		return false
	}
	var v uint64
	v, s.p = uvarint(s.data, s.p)
	s.cur += int64(v)
	s.left--
	return true
}

// phraseMatch reports whether the cursors (all positioned on the same doc) have
// their terms at consecutive positions.
func phraseMatch(curs []*cursor, st []pstream) bool {
	for j, c := range curs {
		st[j] = pstream{data: c.pos, p: c.posStart(), left: c.tf()}
		st[j].next()
	}
	target := st[0].cur
	for {
		restart := false
		for j := range st {
			want := target + int64(j)
			s := &st[j]
			for s.cur < want {
				if !s.next() {
					return false
				}
			}
			if s.cur > want {
				target = s.cur - int64(j)
				restart = true
				break
			}
		}
		if !restart {
			return true
		}
	}
}

// ---- plan & execution ----

type planTerm struct {
	e   termEntry
	idf float64
}

type negPlan struct {
	uniq []termEntry
	seq  []int // phrase order, indices into uniq
}

type plan struct {
	pos     []planTerm
	phrases [][]int // indices into pos
	negs    []negPlan
}

func (ix *index) buildPlan(q string) (*plan, bool) {
	clauses := parseQuery(q)
	pl := &plan{}
	idx := map[string]int{}
	hasPos := false
	for _, c := range clauses {
		if c.neg {
			continue
		}
		hasPos = true
		var seq []int
		for _, t := range c.tokens {
			i, ok := idx[t]
			if !ok {
				e, found := ix.lookup(t)
				if !found {
					return nil, false
				}
				df := float64(e.df)
				i = len(pl.pos)
				idx[t] = i
				pl.pos = append(pl.pos, planTerm{e: e, idf: math.Log(1 + (float64(ix.n)-df+0.5)/(df+0.5))})
			}
			seq = append(seq, i)
		}
		if len(seq) > 1 {
			pl.phrases = append(pl.phrases, seq)
		}
	}
	if !hasPos {
		return nil, false
	}
	for _, c := range clauses {
		if !c.neg {
			continue
		}
		local := map[string]int{}
		var np negPlan
		ok := true
		for _, t := range c.tokens {
			i, seen := local[t]
			if !seen {
				e, found := ix.lookup(t)
				if !found {
					ok = false
					break
				}
				i = len(np.uniq)
				local[t] = i
				np.uniq = append(np.uniq, e)
			}
			np.seq = append(np.seq, i)
		}
		if ok {
			pl.negs = append(pl.negs, np)
		}
	}
	return pl, true
}

type negExec struct {
	uniq []*cursor
	seq  []*cursor
}

// exec evaluates the plan over docnums [lo, hi).
func (ix *index) exec(pl *plan, lo, hi uint32, k int) (int, []hit) {
	np := len(pl.pos)
	curs := make([]cursor, np)
	for i := range pl.pos {
		curs[i].init(pl.pos[i].e)
	}
	order := make([]*cursor, np)
	for i := range curs {
		order[i] = &curs[i]
	}
	slices.SortFunc(order, func(a, b *cursor) int { return int(a.df) - int(b.df) })
	maxLen := 1
	phr := make([][]*cursor, len(pl.phrases))
	for i, seq := range pl.phrases {
		for _, j := range seq {
			phr[i] = append(phr[i], &curs[j])
		}
		maxLen = max(maxLen, len(seq))
	}
	negs := make([]negExec, len(pl.negs))
	for i, n := range pl.negs {
		nc := make([]cursor, len(n.uniq))
		for j := range n.uniq {
			nc[j].init(n.uniq[j])
			negs[i].uniq = append(negs[i].uniq, &nc[j])
		}
		for _, j := range n.seq {
			negs[i].seq = append(negs[i].seq, &nc[j])
		}
		maxLen = max(maxLen, len(n.seq))
	}
	st := make([]pstream, maxLen)

	lead := order[0]
	rest := order[1:]
	top := topK{k: k, h: make([]hit, 0, min(k, int(lead.df)))}
	total := 0
	c1, c2 := ix.normC1, ix.normC2
	lead.advance(lo)
	d := lead.doc
outer:
	for d < hi {
		for _, c := range rest {
			c.advance(d)
			if c.doc != d {
				if c.doc == endDoc {
					break outer
				}
				lead.advance(c.doc)
				d = lead.doc
				continue outer
			}
		}
		match := true
		for _, ph := range phr {
			if !phraseMatch(ph, st[:len(ph)]) {
				match = false
				break
			}
		}
		if match {
			for i := range negs {
				nc := &negs[i]
				all := true
				for _, c := range nc.uniq {
					c.advance(d)
					if c.doc != d {
						all = false
						break
					}
				}
				if all && (len(nc.seq) == 1 || phraseMatch(nc.seq, st[:len(nc.seq)])) {
					match = false
					break
				}
			}
		}
		if match {
			total++
			norm := c1 + c2*float64(ix.dls[d])
			var s float64
			for i := range curs {
				tf := float64(curs[i].tf())
				s += pl.pos[i].idf * tf * (bm25K1 + 1) / (tf + norm)
			}
			top.push(hit{score: s, id: ix.ids[d], doc: d})
		}
		lead.next()
		d = lead.doc
	}
	return total, top.h
}

func (ix *index) search(q string, k int) (int, []hit) {
	pl, ok := ix.buildPlan(q)
	if !ok {
		return 0, nil
	}
	minDF := pl.pos[0].e.df
	for _, t := range pl.pos {
		minDF = min(minDF, t.e.df)
	}
	// Split large queries across cores by docnum range. Phrase checks are much
	// more expensive per candidate, so they parallelize earlier.
	unit := uint32(16384)
	if len(pl.phrases) > 0 || len(pl.negs) > 0 {
		unit = 2048
	}
	parts := min(runtime.GOMAXPROCS(0), int(minDF/unit), 8)
	var total int
	var hits []hit
	if parts <= 1 {
		total, hits = ix.exec(pl, 0, endDoc, k)
	} else {
		n := uint32(ix.n)
		totals := make([]int, parts)
		res := make([][]hit, parts)
		var wg sync.WaitGroup
		for p := 0; p < parts; p++ {
			lo := uint32(uint64(n) * uint64(p) / uint64(parts))
			hi := uint32(uint64(n) * uint64(p+1) / uint64(parts))
			wg.Add(1)
			go func(p int) {
				defer wg.Done()
				totals[p], res[p] = ix.exec(pl, lo, hi, k)
			}(p)
		}
		wg.Wait()
		top := topK{k: k}
		for p := 0; p < parts; p++ {
			total += totals[p]
			for _, h := range res[p] {
				top.push(h)
			}
		}
		hits = top.h
	}
	slices.SortFunc(hits, func(a, b hit) int {
		if worse(b, a) {
			return -1
		}
		if worse(a, b) {
			return 1
		}
		return 0
	})
	return total, hits
}

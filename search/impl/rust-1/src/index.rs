use crate::text::*;
use memmap2::Mmap;
use std::cmp::Ordering;
use std::collections::BinaryHeap;
use std::fs::File;
use std::path::Path;

const K1: f64 = 1.2;
const B: f64 = 0.75;

#[inline]
fn u32_at(d: &[u8], i: usize) -> u32 {
    u32::from_le_bytes(d[i..i + 4].try_into().unwrap())
}

pub struct Index {
    pub n: u64,
    avgdl: f64,
    dl: Mmap,
    ids: Mmap,
    docs: Mmap,
    idmap: Mmap,
    dicts: Vec<Mmap>,
    posts: Vec<Mmap>,
    stores: Vec<Mmap>,
    titles: Vec<Mmap>,
}

fn map(p: &Path) -> std::io::Result<Mmap> {
    let f = File::open(p)?;
    unsafe { Mmap::map(&f) }
}

#[derive(Clone, Copy)]
pub struct TermInfo {
    df: u32,
    doc_bytes: u32,
    off: u64,
    part: usize,
}

impl Index {
    pub fn open(dir: &Path) -> std::io::Result<Index> {
        let meta = std::fs::read(dir.join("meta.bin"))?;
        let n = u64::from_le_bytes(meta[0..8].try_into().unwrap());
        let total = u64::from_le_bytes(meta[8..16].try_into().unwrap());
        let nchunks = u64::from_le_bytes(meta[16..24].try_into().unwrap()) as usize;
        let avgdl = if n > 0 { total as f64 / n as f64 } else { 0.0 };
        let mut dicts = Vec::new();
        let mut posts = Vec::new();
        for p in 0..NPART {
            dicts.push(map(&dir.join(format!("dict_{p}.bin")))?);
            posts.push(map(&dir.join(format!("post_{p}.bin")))?);
        }
        let mut stores = Vec::new();
        let mut titles = Vec::new();
        for c in 0..nchunks {
            stores.push(map(&dir.join(format!("texts_{c}.bin")))?);
            titles.push(map(&dir.join(format!("titles_{c}.bin")))?);
        }
        let idx = Index {
            n,
            avgdl,
            dl: map(&dir.join("dl.bin"))?,
            ids: map(&dir.join("ids.bin"))?,
            docs: map(&dir.join("docs.bin"))?,
            idmap: map(&dir.join("idmap.bin"))?,
            dicts,
            posts,
            stores,
            titles,
        };
        #[cfg(unix)]
        {
            use memmap2::Advice;
            let _ = idx.dl.advise(Advice::WillNeed);
            let _ = idx.ids.advise(Advice::WillNeed);
            for d in &idx.dicts {
                let _ = d.advise(Advice::WillNeed);
            }
            for p in &idx.posts {
                let _ = p.advise(Advice::Random);
            }
        }
        Ok(idx)
    }

    pub fn lookup(&self, term: &[u8]) -> Option<TermInfo> {
        let part = partition(term);
        let d: &[u8] = &self.dicts[part];
        if d.len() < 8 {
            return None;
        }
        let nt = u64::from_le_bytes(d[0..8].try_into().unwrap()) as usize;
        let strs = 8 + nt * 24;
        let (mut lo, mut hi) = (0usize, nt);
        while lo < hi {
            let mid = (lo + hi) / 2;
            let r = 8 + mid * 24;
            let so = u32_at(d, r) as usize;
            let sl = u32_at(d, r + 4) as usize;
            let s = &d[strs + so..strs + so + sl];
            match s.cmp(term) {
                Ordering::Less => lo = mid + 1,
                Ordering::Greater => hi = mid,
                Ordering::Equal => {
                    return Some(TermInfo {
                        df: u32_at(d, r + 8),
                        doc_bytes: u32_at(d, r + 12),
                        off: u64::from_le_bytes(d[r + 16..r + 24].try_into().unwrap()),
                        part,
                    });
                }
            }
        }
        None
    }

    #[inline]
    fn ext_id(&self, ord: u32) -> u32 {
        u32_at(&self.ids, ord as usize * 4)
    }

    #[inline]
    fn rec(&self, ord: u32, f: usize) -> usize {
        u32_at(&self.docs, ord as usize * 28 + f * 4) as usize
    }

    pub fn title(&self, ord: u32) -> &str {
        let (c, off, tl) = (self.rec(ord, 0), self.rec(ord, 1), self.rec(ord, 2));
        let s = &self.titles[c][off..off + tl];
        unsafe { std::str::from_utf8_unchecked(s) }
    }

    /// Returns (title, text) for an external id.
    pub fn doc(&self, id: u32) -> Option<(&str, String)> {
        let m: &[u8] = &self.idmap;
        let n = m.len() / 8;
        let (mut lo, mut hi) = (0, n);
        while lo < hi {
            let mid = (lo + hi) / 2;
            let v = u32_at(m, mid * 8);
            match v.cmp(&id) {
                Ordering::Less => lo = mid + 1,
                Ordering::Greater => hi = mid,
                Ordering::Equal => {
                    let ord = u32_at(m, mid * 8 + 4);
                    let c = self.rec(ord, 0);
                    let (bo, bl, io, xl) = (self.rec(ord, 3), self.rec(ord, 4), self.rec(ord, 5), self.rec(ord, 6));
                    let st = &self.stores[c];
                    let cap = zstd::zstd_safe::get_frame_content_size(&st[bo..bo + bl]).ok()??;
                    let block = zstd::bulk::decompress(&st[bo..bo + bl], cap as usize).ok()?;
                    let text = String::from_utf8(block.get(io..io + xl)?.to_vec()).ok()?;
                    return Some((self.title(ord), text));
                }
            }
        }
        None
    }

    fn cursor(&self, t: &TermInfo) -> Cursor<'_> {
        let p: &[u8] = &self.posts[t.part];
        let nblocks = (t.df as usize).div_ceil(BLOCK);
        let off = t.off as usize;
        let skips = &p[off..off + nblocks * 12];
        let docs = &p[off + nblocks * 12..off + nblocks * 12 + t.doc_bytes as usize];
        let pos = &p[off + nblocks * 12 + t.doc_bytes as usize..];
        let mut c = Cursor {
            skips,
            docs,
            pos,
            df: t.df,
            nblocks,
            block: usize::MAX,
            bdocs: [0; BLOCK],
            btfs: [0; BLOCK],
            bpos: [0; BLOCK],
            bn: 0,
            i: 0,
            cur: 0,
        };
        c.load_block(0);
        c
    }
}

pub const END: u32 = u32::MAX;

struct Cursor<'a> {
    skips: &'a [u8],
    docs: &'a [u8],
    pos: &'a [u8],
    df: u32,
    nblocks: usize,
    block: usize,
    bdocs: [u32; BLOCK],
    btfs: [u32; BLOCK],
    bpos: [u32; BLOCK],
    bn: usize,
    i: usize,
    cur: u32,
}

impl<'a> Cursor<'a> {
    #[inline]
    fn last_of(&self, b: usize) -> u32 {
        u32_at(self.skips, b * 12 + 8)
    }

    fn load_block(&mut self, b: usize) {
        if b >= self.nblocks {
            self.cur = END;
            self.block = b;
            return;
        }
        self.block = b;
        let mut p = u32_at(self.skips, b * 12) as usize;
        let n = if b + 1 == self.nblocks { self.df as usize - b * BLOCK } else { BLOCK };
        let mut prev = if b == 0 { 0 } else { self.last_of(b - 1) };
        let mut po = u32_at(self.skips, b * 12 + 4);
        for j in 0..n {
            prev += get_varint(self.docs, &mut p);
            self.bdocs[j] = prev;
            self.btfs[j] = get_varint(self.docs, &mut p);
            self.bpos[j] = po;
            po += get_varint(self.docs, &mut p);
        }
        self.bn = n;
        self.i = 0;
        self.cur = self.bdocs[0];
    }

    #[inline]
    fn doc(&self) -> u32 {
        self.cur
    }

    #[inline]
    fn next(&mut self) {
        self.i += 1;
        if self.i < self.bn {
            self.cur = self.bdocs[self.i];
        } else {
            self.load_block(self.block + 1);
        }
    }

    /// Advance to the first doc >= target.
    fn advance(&mut self, target: u32) {
        if self.cur >= target {
            return;
        }
        if self.last_of(self.block) < target {
            // binary search for the first block with last >= target
            let (mut lo, mut hi) = (self.block + 1, self.nblocks);
            while lo < hi {
                let mid = (lo + hi) / 2;
                if self.last_of(mid) < target {
                    lo = mid + 1;
                } else {
                    hi = mid;
                }
            }
            self.load_block(lo);
            if self.cur == END {
                return;
            }
        }
        while self.bdocs[self.i] < target {
            self.i += 1;
        }
        self.cur = self.bdocs[self.i];
    }

    #[inline]
    fn tf(&self) -> u32 {
        self.btfs[self.i]
    }

    #[inline]
    fn pos_iter(&self) -> PosIter<'a> {
        PosIter { data: self.pos, p: self.bpos[self.i] as usize, left: self.btfs[self.i], cur: 0 }
    }
}

struct PosIter<'a> {
    data: &'a [u8],
    p: usize,
    left: u32,
    cur: u32,
}

impl PosIter<'_> {
    /// Advances to the first position >= target; returns false if exhausted.
    #[inline]
    fn seek(&mut self, target: u32) -> bool {
        while self.left > 0 {
            self.cur += get_varint(self.data, &mut self.p);
            self.left -= 1;
            if self.cur >= target {
                return true;
            }
        }
        false
    }
}

struct Clause {
    neg: bool,
    tokens: Vec<Vec<u8>>,
}

fn parse_query(q: &str) -> Vec<Clause> {
    let chars: Vec<char> = q.chars().collect();
    let mut i = 0;
    let mut out = Vec::new();
    loop {
        while i < chars.len() && chars[i].is_whitespace() {
            i += 1;
        }
        if i >= chars.len() {
            break;
        }
        let mut neg = false;
        if chars[i] == '-' {
            neg = true;
            i += 1;
        }
        let start;
        let end;
        if i < chars.len() && chars[i] == '"' {
            i += 1;
            start = i;
            while i < chars.len() && chars[i] != '"' {
                i += 1;
            }
            end = i;
            if i < chars.len() {
                i += 1;
            }
        } else {
            start = i;
            while i < chars.len() && !chars[i].is_whitespace() {
                i += 1;
            }
            end = i;
        }
        let s: String = chars[start..end].iter().collect();
        let tokens = tokens(&s);
        if !tokens.is_empty() {
            out.push(Clause { neg, tokens });
        }
    }
    out
}

#[derive(Clone, Copy)]
pub struct Hit {
    pub score: f64,
    pub id: u32,
    pub ord: u32,
}

// "Greater" = worse, so a BinaryHeap keeps the worst hit on top.
impl PartialEq for Hit {
    fn eq(&self, o: &Self) -> bool {
        self.cmp(o) == Ordering::Equal
    }
}
impl Eq for Hit {}
impl PartialOrd for Hit {
    fn partial_cmp(&self, o: &Self) -> Option<Ordering> {
        Some(self.cmp(o))
    }
}
impl Ord for Hit {
    fn cmp(&self, o: &Self) -> Ordering {
        o.score.total_cmp(&self.score).then(self.id.cmp(&o.id))
    }
}

struct CClause {
    terms: Vec<usize>, // indices into cursors
}

fn phrase_match(cl: &CClause, curs: &[Cursor]) -> bool {
    if cl.terms.len() == 2 {
        let mut a = curs[cl.terms[0]].pos_iter();
        let mut b = curs[cl.terms[1]].pos_iter();
        if !a.seek(0) || !b.seek(1) {
            return false;
        }
        loop {
            let want = a.cur + 1;
            if b.cur < want && !b.seek(want) {
                return false;
            }
            if b.cur == want {
                return true;
            }
            if !a.seek(b.cur - 1) {
                return false;
            }
        }
    }
    let mut its: Vec<PosIter> = cl.terms.iter().map(|&t| curs[t].pos_iter()).collect();
    let m = its.len();
    // current value of each iterator is pos_j; we need pos_j - j equal for all j.
    for (j, it) in its.iter_mut().enumerate() {
        if !it.seek(j as u32) {
            return false;
        }
    }
    let mut cand = its[0].cur;
    loop {
        let mut agreed = true;
        for j in 0..m {
            let want = cand + j as u32;
            if its[j].cur < want && !its[j].seek(want) {
                return false;
            }
            let base = its[j].cur - j as u32;
            if base > cand {
                cand = base;
                agreed = false;
                break;
            }
        }
        if agreed {
            return true;
        }
    }
}

impl Index {
    pub fn search(&self, q: &str, k: usize) -> (u64, Vec<Hit>) {
        let clauses = parse_query(q);
        if !clauses.iter().any(|c| !c.neg) {
            return (0, Vec::new());
        }
        let mut terms: Vec<Vec<u8>> = Vec::new();
        let mut infos: Vec<Option<TermInfo>> = Vec::new();
        let tid = |t: &Vec<u8>, terms: &mut Vec<Vec<u8>>, infos: &mut Vec<Option<TermInfo>>| -> usize {
            if let Some(i) = terms.iter().position(|x| x == t) {
                return i;
            }
            terms.push(t.clone());
            infos.push(self.lookup(t));
            terms.len() - 1
        };
        let mut pos_clauses = Vec::new();
        let mut neg_clauses = Vec::new();
        let mut positive_terms: Vec<usize> = Vec::new();
        for c in &clauses {
            let ts: Vec<usize> = c.tokens.iter().map(|t| tid(t, &mut terms, &mut infos)).collect();
            if c.neg {
                if ts.iter().all(|&t| infos[t].is_some()) {
                    neg_clauses.push(CClause { terms: ts });
                }
            } else {
                if ts.iter().any(|&t| infos[t].is_none()) {
                    return (0, Vec::new());
                }
                for &t in &ts {
                    if !positive_terms.contains(&t) {
                        positive_terms.push(t);
                    }
                }
                if ts.len() > 1 {
                    pos_clauses.push(CClause { terms: ts });
                }
            }
        }
        // cursors only for terms actually needed
        let mut curs: Vec<Cursor> = Vec::new();
        let mut cmap = vec![usize::MAX; terms.len()];
        for (t, info) in infos.iter().enumerate() {
            let used = positive_terms.contains(&t) || neg_clauses.iter().any(|c| c.terms.contains(&t));
            if used {
                cmap[t] = curs.len();
                curs.push(self.cursor(info.as_ref().unwrap()));
            }
        }
        for c in pos_clauses.iter_mut().chain(neg_clauses.iter_mut()) {
            for t in c.terms.iter_mut() {
                *t = cmap[*t];
            }
        }
        let n = self.n as f64;
        let mut pt: Vec<(usize, f64)> = positive_terms
            .iter()
            .map(|&t| {
                let df = infos[t].unwrap().df as f64;
                (cmap[t], (1.0 + (n - df + 0.5) / (df + 0.5)).ln())
            })
            .collect();
        // Fixed summation order (by token) for deterministic scores; iteration order by df.
        let score_terms = pt.clone();
        pt.sort_by_key(|&(c, _)| curs[c].df);
        let order: Vec<usize> = pt.iter().map(|&(c, _)| c).collect();

        let mut heap: BinaryHeap<Hit> = BinaryHeap::with_capacity(k + 1);
        let mut total: u64 = 0;
        let lead = order[0];
        let dl: &[u8] = &self.dl;
        let avgdl = self.avgdl;
        loop {
            let mut target = curs[lead].doc();
            if target == END {
                break;
            }
            // leapfrog until all positive cursors agree
            let mut agreed = false;
            'lf: loop {
                for &c in &order {
                    curs[c].advance(target);
                    let d = curs[c].doc();
                    if d == END {
                        break 'lf;
                    }
                    if d > target {
                        target = d;
                        continue 'lf;
                    }
                }
                agreed = true;
                break;
            }
            if !agreed {
                break;
            }
            let d = target;
            let mut ok = pos_clauses.iter().all(|c| phrase_match(c, &curs));
            if ok {
                for c in &neg_clauses {
                    let mut all = true;
                    for &t in &c.terms {
                        curs[t].advance(d);
                        if curs[t].doc() != d {
                            all = false;
                            break;
                        }
                    }
                    if all && (c.terms.len() == 1 || phrase_match(c, &curs)) {
                        ok = false;
                        break;
                    }
                }
            }
            if ok {
                total += 1;
                let norm = K1 * (1.0 - B + B * u32_at(dl, d as usize * 4) as f64 / avgdl);
                let mut score = 0.0;
                for &(c, idf) in &score_terms {
                    let tf = curs[c].tf() as f64;
                    score += idf * tf * (K1 + 1.0) / (tf + norm);
                }
                let h = Hit { score, id: self.ext_id(d), ord: d };
                if heap.len() < k {
                    heap.push(h);
                } else if h < *heap.peek().unwrap() {
                    *heap.peek_mut().unwrap() = h;
                }
            }
            curs[lead].next();
        }
        let mut hits = heap.into_vec();
        hits.sort();
        (total, hits)
    }
}

use crate::text::*;
use std::os::unix::fs::FileExt;
use rayon::prelude::*;
use rustc_hash::FxHashMap;
use serde::Deserialize;
use std::borrow::Cow;
use std::cmp::Reverse;
use std::collections::BinaryHeap;
use std::fs::{self, File};
use std::io::{BufWriter, Write};
use std::path::{Path, PathBuf};

#[derive(Deserialize)]
struct Doc<'a> {
    id: u64,
    #[serde(borrow)]
    title: Cow<'a, str>,
    #[serde(borrow)]
    text: Cow<'a, str>,
}

#[derive(Default)]
struct TermAcc {
    docs: Vec<u8>,
    pos: Vec<u8>,
    last: u32,
    df: u32,
}

const STORE_BLOCK: usize = 64 << 10;
const ZLEVEL: i32 = 3;

struct ChunkResult {
    ids: Vec<u32>,
    dls: Vec<u32>,
    /// off, title_len, text_len, text_clen
    /// title_off, title_len, block_off, block_clen, in_block_off, text_len
    recs: Vec<[u32; 6]>,
}

fn seg_path(dir: &Path, c: usize) -> PathBuf {
    dir.join(format!("seg_{c}.tmp"))
}

fn process_chunk(c: usize, data: &[u8], dir: &Path) -> std::io::Result<ChunkResult> {
    let mut map: FxHashMap<Box<[u8]>, u32> = FxHashMap::default();
    let mut terms: Vec<TermAcc> = Vec::new();
    let mut res = ChunkResult { ids: Vec::new(), dls: Vec::new(), recs: Vec::new() };
    let mut titles = BufWriter::with_capacity(1 << 18, File::create(dir.join(format!("titles_{c}.bin")))?);
    let mut titles_off: u64 = 0;
    let mut store = BufWriter::with_capacity(1 << 20, File::create(dir.join(format!("texts_{c}.bin")))?);
    let mut store_off: u64 = 0;
    let mut comp = zstd::bulk::Compressor::new(ZLEVEL)?;
    let mut block: Vec<u8> = Vec::with_capacity(STORE_BLOCK * 2);
    let mut block_first = 0usize;
    let mut flush_block = |block: &mut Vec<u8>, first: &mut usize, recs: &mut Vec<[u32; 6]>| -> std::io::Result<()> {
        if *first == recs.len() {
            return Ok(());
        }
        let cb = comp.compress(block)?;
        for r in &mut recs[*first..] {
            r[2] = store_off as u32;
            r[3] = cb.len() as u32;
        }
        store.write_all(&cb)?;
        store_off += cb.len() as u64;
        block.clear();
        *first = recs.len();
        Ok(())
    };
    let mut toks: Vec<(u32, u32)> = Vec::new();
    let mut buf = Vec::new();

    for line in data.split(|&b| b == b'\n') {
        if line.iter().all(|b| b.is_ascii_whitespace()) {
            continue;
        }
        let doc: Doc = match serde_json::from_slice(line) {
            Ok(d) => d,
            Err(e) => {
                eprintln!("skipping bad line: {e}");
                continue;
            }
        };
        let local = res.ids.len() as u32;
        toks.clear();
        let mut p = 0u32;
        {
            let mut add = |t: &[u8]| {
                let tid = match map.get(t) {
                    Some(&id) => id,
                    None => {
                        let id = terms.len() as u32;
                        map.insert(t.into(), id);
                        terms.push(TermAcc::default());
                        id
                    }
                };
                toks.push((tid, p));
                p += 1;
            };
            tokenize(&doc.title, &mut buf, &mut add);
            tokenize(&doc.text, &mut buf, &mut add);
        }
        toks.sort_unstable();
        let mut i = 0;
        while i < toks.len() {
            let tid = toks[i].0;
            let mut j = i;
            while j < toks.len() && toks[j].0 == tid {
                j += 1;
            }
            let acc = &mut terms[tid as usize];
            let delta = if acc.df == 0 { local } else { local - acc.last };
            put_varint(&mut acc.docs, delta);
            put_varint(&mut acc.docs, (j - i) as u32);
            acc.last = local;
            acc.df += 1;
            let mut prev = 0;
            for &(_, pp) in &toks[i..j] {
                put_varint(&mut acc.pos, pp - prev);
                prev = pp;
            }
            i = j;
        }
        res.ids.push(doc.id as u32);
        res.dls.push(p);
        res.recs.push([titles_off as u32, doc.title.len() as u32, 0, 0, block.len() as u32, doc.text.len() as u32]);
        titles.write_all(doc.title.as_bytes())?;
        titles_off += doc.title.len() as u64;
        block.extend_from_slice(doc.text.as_bytes());
        if block.len() >= STORE_BLOCK {
            flush_block(&mut block, &mut block_first, &mut res.recs)?;
        }
    }
    flush_block(&mut block, &mut block_first, &mut res.recs)?;
    drop(flush_block);
    store.flush()?;
    titles.flush()?;

    // Write segment: terms grouped by partition, sorted within.
    let mut order: Vec<(usize, &[u8], u32)> = map.iter().map(|(k, &v)| (partition(k), &k[..], v)).collect();
    order.sort_unstable_by(|a, b| (a.0, a.1).cmp(&(b.0, b.1)));
    let mut f = BufWriter::with_capacity(1 << 20, File::create(seg_path(dir, c))?);
    let hdr = ((NPART + 1) * 8) as u64;
    f.write_all(&vec![0u8; hdr as usize])?;
    let mut offs = vec![0u64; NPART + 1];
    let mut cur: u64 = 0;
    let mut oi = 0;
    let mut tmp = Vec::with_capacity(16);
    for p in 0..NPART {
        offs[p] = cur;
        while oi < order.len() && order[oi].0 == p {
            let (_, t, tid) = order[oi];
            let acc = &terms[tid as usize];
            tmp.clear();
            put_varint(&mut tmp, t.len() as u32);
            tmp.extend_from_slice(t);
            put_varint(&mut tmp, acc.df);
            put_varint(&mut tmp, acc.docs.len() as u32);
            f.write_all(&tmp)?;
            f.write_all(&acc.docs)?;
            cur += (tmp.len() + acc.docs.len()) as u64;
            tmp.clear();
            put_varint(&mut tmp, acc.pos.len() as u32);
            f.write_all(&tmp)?;
            f.write_all(&acc.pos)?;
            cur += (tmp.len() + acc.pos.len()) as u64;
            oi += 1;
        }
    }
    offs[NPART] = cur;
    let f = f.into_inner().map_err(|e| e.into_error())?;
    let h: Vec<u8> = offs.iter().flat_map(|o| o.to_le_bytes()).collect();
    f.write_all_at(&h, 0)?;
    Ok(res)
}

struct SegCursor<'a> {
    data: &'a [u8],
    pos: usize,
}

struct Entry<'a> {
    df: u32,
    docs: &'a [u8],
    pos: &'a [u8],
}

impl<'a> SegCursor<'a> {
    fn next_term(&mut self) -> Option<(&'a [u8], Entry<'a>)> {
        if self.pos >= self.data.len() {
            return None;
        }
        let d = self.data;
        let tl = get_varint(d, &mut self.pos) as usize;
        let term = &d[self.pos..self.pos + tl];
        self.pos += tl;
        let df = get_varint(d, &mut self.pos);
        let dl = get_varint(d, &mut self.pos) as usize;
        let docs = &d[self.pos..self.pos + dl];
        self.pos += dl;
        let pl = get_varint(d, &mut self.pos) as usize;
        let pos = &d[self.pos..self.pos + pl];
        self.pos += pl;
        Some((term, Entry { df, docs, pos }))
    }
}

fn merge_partition(p: usize, segs: &[(File, Vec<u64>)], bases: &[u32], dir: &Path) -> std::io::Result<()> {
    let hdr = ((NPART + 1) * 8) as u64;
    let bufs: Vec<Vec<u8>> = segs
        .iter()
        .map(|(f, offs)| {
            let mut b = vec![0u8; (offs[p + 1] - offs[p]) as usize];
            f.read_exact_at(&mut b, hdr + offs[p])?;
            Ok(b)
        })
        .collect::<std::io::Result<_>>()?;
    let mut curs: Vec<SegCursor> = bufs.iter().map(|b| SegCursor { data: b, pos: 0 }).collect();
    let mut pending: Vec<Option<Entry>> = (0..segs.len()).map(|_| None).collect();
    let mut heap = BinaryHeap::new();
    for (s, c) in curs.iter_mut().enumerate() {
        if let Some((t, e)) = c.next_term() {
            pending[s] = Some(e);
            heap.push(Reverse((t, s)));
        }
    }
    let mut post = BufWriter::with_capacity(1 << 20, File::create(dir.join(format!("post_{p}.bin")))?);
    let mut post_off: u64 = 0;
    let mut recs: Vec<u8> = Vec::new();
    let mut strs: Vec<u8> = Vec::new();
    let mut nterms: u64 = 0;
    let mut group: Vec<(usize, Entry)> = Vec::new();
    let mut skips: Vec<u8> = Vec::new();
    let mut docbuf: Vec<u8> = Vec::new();
    let mut posbuf: Vec<u8> = Vec::new();

    while let Some(Reverse((term, s))) = heap.pop() {
        group.clear();
        group.push((s, pending[s].take().unwrap()));
        if let Some((t, e)) = curs[s].next_term() {
            pending[s] = Some(e);
            heap.push(Reverse((t, s)));
        }
        while let Some(Reverse((t2, s2))) = heap.peek() {
            if *t2 != term {
                break;
            }
            let s2 = *s2;
            heap.pop();
            group.push((s2, pending[s2].take().unwrap()));
            if let Some((t, e)) = curs[s2].next_term() {
                pending[s2] = Some(e);
                heap.push(Reverse((t, s2)));
            }
        }
        // group is in segment order (heap ties broken by segment index).
        let df: u32 = group.iter().map(|(_, e)| e.df).sum();
        skips.clear();
        docbuf.clear();
        posbuf.clear();
        let mut n = 0u32;
        let mut prev_doc = 0u32;
        let mut block_prev = 0u32;
        for (s, e) in group.iter() {
            let base = bases[*s];
            let mut dp = 0;
            let mut pp = 0;
            let mut local = 0u32;
            for i in 0..e.df {
                let delta = get_varint(e.docs, &mut dp);
                local = if i == 0 { delta } else { local + delta };
                let tf = get_varint(e.docs, &mut dp);
                let doc = base + local;
                if n as usize % BLOCK == 0 {
                    if n > 0 {
                        skips.extend_from_slice(&prev_doc.to_le_bytes());
                        block_prev = prev_doc;
                    }
                    skips.extend_from_slice(&(docbuf.len() as u32).to_le_bytes());
                    skips.extend_from_slice(&((posbuf.len() + pp) as u32).to_le_bytes());
                }
                put_varint(&mut docbuf, doc - if n as usize % BLOCK == 0 { block_prev } else { prev_doc });
                put_varint(&mut docbuf, tf);
                let p0 = pp;
                skip_varints(e.pos, &mut pp, tf);
                put_varint(&mut docbuf, (pp - p0) as u32);
                prev_doc = doc;
                n += 1;
            }
            posbuf.extend_from_slice(e.pos);
        }
        skips.extend_from_slice(&prev_doc.to_le_bytes());
        // skips layout per block: [doc_off, pos_off, last_doc] (12 bytes)
        // Written order above: doc_off, pos_off, then last_doc appended when block closes.
        let rec_off = post_off;
        post.write_all(&skips)?;
        post.write_all(&docbuf)?;
        post.write_all(&posbuf)?;
        post_off += (skips.len() + docbuf.len() + posbuf.len()) as u64;
        recs.extend_from_slice(&(strs.len() as u32).to_le_bytes());
        recs.extend_from_slice(&(term.len() as u32).to_le_bytes());
        recs.extend_from_slice(&df.to_le_bytes());
        recs.extend_from_slice(&(docbuf.len() as u32).to_le_bytes());
        recs.extend_from_slice(&rec_off.to_le_bytes());
        strs.extend_from_slice(term);
        nterms += 1;
    }
    post.flush()?;
    let mut d = BufWriter::new(File::create(dir.join(format!("dict_{p}.bin")))?);
    d.write_all(&nterms.to_le_bytes())?;
    d.write_all(&recs)?;
    d.write_all(&strs)?;
    d.flush()?;
    Ok(())
}

pub fn build(corpus: &Path, dir: &Path) -> std::io::Result<()> {
    fs::create_dir_all(dir)?;
    let file = File::open(corpus)?;
    let len = file.metadata()?.len();
    let target = (4u64 << 20).max(len / 2048);
    let mut chunks: Vec<(u64, u64)> = Vec::new();
    let mut start = 0u64;
    let mut probe = vec![0u8; 1 << 16];
    while start < len {
        let mut end = (start + target).min(len);
        // extend to just past the next newline
        while end < len {
            let n = probe.len().min((len - end) as usize);
            file.read_exact_at(&mut probe[..n], end)?;
            match memchr_nl(&probe[..n]) {
                Some(i) => {
                    end += i as u64 + 1;
                    break;
                }
                None => end += n as u64,
            }
        }
        chunks.push((start, end));
        start = end;
    }
    let results: Vec<ChunkResult> = chunks
        .par_iter()
        .enumerate()
        .map(|(c, &(a, b))| {
            let mut buf = vec![0u8; (b - a) as usize];
            file.read_exact_at(&mut buf, a)?;
            process_chunk(c, &buf, dir)
        })
        .collect::<std::io::Result<_>>()?;

    let mut bases = Vec::with_capacity(results.len());
    let mut n: u32 = 0;
    for r in &results {
        bases.push(n);
        n += r.ids.len() as u32;
    }

    let segs: Vec<(File, Vec<u64>)> = (0..results.len())
        .map(|c| {
            let f = File::open(seg_path(dir, c))?;
            let mut h = vec![0u8; (NPART + 1) * 8];
            f.read_exact_at(&mut h, 0)?;
            let offs = h.chunks(8).map(|x| u64::from_le_bytes(x.try_into().unwrap())).collect();
            Ok((f, offs))
        })
        .collect::<std::io::Result<_>>()?;
    let merge = || -> std::io::Result<()> {
        (0..NPART).into_par_iter().try_for_each(|p| merge_partition(p, &segs, &bases, dir))
    };

    let write_docs = || -> std::io::Result<()> {
        let mut total_tokens: u64 = 0;
        let mut ids = BufWriter::new(File::create(dir.join("ids.bin"))?);
        let mut dls = BufWriter::new(File::create(dir.join("dl.bin"))?);
        let mut docs = BufWriter::new(File::create(dir.join("docs.bin"))?);
        let mut idmap: Vec<(u32, u32)> = Vec::with_capacity(n as usize);
        for (c, r) in results.iter().enumerate() {
            for i in 0..r.ids.len() {
                ids.write_all(&r.ids[i].to_le_bytes())?;
                dls.write_all(&r.dls[i].to_le_bytes())?;
                total_tokens += r.dls[i] as u64;
                docs.write_all(&(c as u32).to_le_bytes())?;
                for v in r.recs[i] {
                    docs.write_all(&v.to_le_bytes())?;
                }
                idmap.push((r.ids[i], bases[c] + i as u32));
            }
        }
        idmap.sort_unstable();
        let mut im = BufWriter::new(File::create(dir.join("idmap.bin"))?);
        for (id, o) in idmap {
            im.write_all(&id.to_le_bytes())?;
            im.write_all(&o.to_le_bytes())?;
        }
        im.flush()?;
        ids.flush()?;
        dls.flush()?;
        docs.flush()?;
        let mut m = File::create(dir.join("meta.bin"))?;
        m.write_all(&(n as u64).to_le_bytes())?;
        m.write_all(&total_tokens.to_le_bytes())?;
        m.write_all(&(results.len() as u64).to_le_bytes())?;
        Ok(())
    };
    let (a, b) = rayon::join(merge, write_docs);
    a?;
    b?;
    drop(segs);
    for c in 0..results.len() {
        let _ = fs::remove_file(seg_path(dir, c));
    }
    Ok(())
}

fn memchr_nl(s: &[u8]) -> Option<usize> {
    s.iter().position(|&b| b == b'\n')
}


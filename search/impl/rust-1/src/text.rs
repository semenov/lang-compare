use unicode_general_category::{GeneralCategory as G, get_general_category};

#[inline]
fn is_word_char(c: char) -> bool {
    matches!(
        get_general_category(c),
        G::UppercaseLetter
            | G::LowercaseLetter
            | G::TitlecaseLetter
            | G::ModifierLetter
            | G::OtherLetter
            | G::DecimalNumber
            | G::LetterNumber
            | G::OtherNumber
    )
}

/// Unicode simple lowercase mapping.
#[inline]
fn simple_lower(c: char) -> char {
    if c == '\u{130}' {
        return 'i';
    }
    let mut it = c.to_lowercase();
    match (it.next(), it.next()) {
        (Some(l), None) => l,
        _ => c,
    }
}

/// Calls `f` for every token in `s` (lowercased UTF-8 bytes).
pub fn tokenize(s: &str, buf: &mut Vec<u8>, mut f: impl FnMut(&[u8])) {
    buf.clear();
    let bytes = s.as_bytes();
    let mut i = 0;
    while i < bytes.len() {
        let b = bytes[i];
        if b < 0x80 {
            if b.is_ascii_alphanumeric() {
                buf.push(b.to_ascii_lowercase());
            } else if !buf.is_empty() {
                f(buf);
                buf.clear();
            }
            i += 1;
        } else {
            let c = s[i..].chars().next().unwrap();
            i += c.len_utf8();
            if is_word_char(c) {
                let l = simple_lower(c);
                let mut tmp = [0u8; 4];
                buf.extend_from_slice(l.encode_utf8(&mut tmp).as_bytes());
            } else if !buf.is_empty() {
                f(buf);
                buf.clear();
            }
        }
    }
    if !buf.is_empty() {
        f(buf);
        buf.clear();
    }
}

pub fn tokens(s: &str) -> Vec<Vec<u8>> {
    let mut out = Vec::new();
    let mut buf = Vec::new();
    tokenize(s, &mut buf, |t| out.push(t.to_vec()));
    out
}

#[inline]
pub fn put_varint(out: &mut Vec<u8>, mut v: u32) {
    while v >= 0x80 {
        out.push((v as u8) | 0x80);
        v >>= 7;
    }
    out.push(v as u8);
}

#[inline]
pub fn get_varint(data: &[u8], pos: &mut usize) -> u32 {
    let mut b = data[*pos];
    *pos += 1;
    if b < 0x80 {
        return b as u32;
    }
    let mut v = (b & 0x7f) as u32;
    let mut shift = 7;
    loop {
        b = data[*pos];
        *pos += 1;
        v |= ((b & 0x7f) as u32) << shift;
        if b < 0x80 {
            return v;
        }
        shift += 7;
    }
}

/// Skips `n` varints starting at `pos`.
#[inline]
pub fn skip_varints(data: &[u8], pos: &mut usize, mut n: u32) {
    while n > 0 {
        if data[*pos] < 0x80 {
            n -= 1;
        }
        *pos += 1;
    }
}

pub const NPART: usize = 64;

#[inline]
pub fn partition(term: &[u8]) -> usize {
    let mut h: u32 = 0x811c9dc5;
    for &b in term {
        h ^= b as u32;
        h = h.wrapping_mul(0x01000193);
    }
    (h as usize) % NPART
}

pub const BLOCK: usize = 128;

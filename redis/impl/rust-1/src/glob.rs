//! Redis-style glob matching (stringmatchlen).

pub fn glob_match(p: &[u8], s: &[u8]) -> bool {
    let (mut pi, mut si) = (0usize, 0usize);
    while pi < p.len() {
        match p[pi] {
            b'*' => {
                while pi + 1 < p.len() && p[pi + 1] == b'*' {
                    pi += 1;
                }
                if pi + 1 == p.len() {
                    return true;
                }
                for k in si..=s.len() {
                    if glob_match(&p[pi + 1..], &s[k..]) {
                        return true;
                    }
                }
                return false;
            }
            b'?' => {
                if si >= s.len() {
                    return false;
                }
                si += 1;
            }
            b'[' => {
                if si >= s.len() {
                    return false;
                }
                pi += 1;
                let not = pi < p.len() && p[pi] == b'^';
                if not {
                    pi += 1;
                }
                let mut matched = false;
                loop {
                    if pi >= p.len() {
                        break;
                    }
                    if p[pi] == b'\\' && pi + 1 < p.len() {
                        pi += 1;
                        if p[pi] == s[si] {
                            matched = true;
                        }
                    } else if p[pi] == b']' {
                        break;
                    } else if pi + 2 < p.len() && p[pi + 1] == b'-' {
                        let (mut lo, mut hi) = (p[pi], p[pi + 2]);
                        if lo > hi {
                            std::mem::swap(&mut lo, &mut hi);
                        }
                        if s[si] >= lo && s[si] <= hi {
                            matched = true;
                        }
                        pi += 2;
                    } else if p[pi] == s[si] {
                        matched = true;
                    }
                    pi += 1;
                }
                if not {
                    matched = !matched;
                }
                if !matched {
                    return false;
                }
                si += 1;
            }
            b'\\' if pi + 1 < p.len() => {
                pi += 1;
                if si >= s.len() || p[pi] != s[si] {
                    return false;
                }
                si += 1;
            }
            c => {
                if si >= s.len() || c != s[si] {
                    return false;
                }
                si += 1;
            }
        }
        pi += 1;
    }
    si == s.len()
}

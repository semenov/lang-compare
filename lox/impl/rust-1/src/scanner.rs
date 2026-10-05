#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum TokenType {
    LeftParen,
    RightParen,
    LeftBrace,
    RightBrace,
    Comma,
    Dot,
    Minus,
    Plus,
    Semicolon,
    Slash,
    Star,
    Bang,
    BangEqual,
    Equal,
    EqualEqual,
    Greater,
    GreaterEqual,
    Less,
    LessEqual,
    Identifier,
    String,
    Number,
    And,
    Class,
    Else,
    False,
    For,
    Fun,
    If,
    Nil,
    Or,
    Print,
    Return,
    Super,
    This,
    True,
    Var,
    While,
    Error,
    Eof,
}

#[derive(Clone, Copy)]
pub struct Token<'a> {
    pub ty: TokenType,
    pub lexeme: &'a str,
    pub line: u32,
}

pub struct Scanner<'a> {
    src: &'a str,
    bytes: &'a [u8],
    start: usize,
    current: usize,
    line: u32,
}

impl<'a> Scanner<'a> {
    pub fn new(src: &'a str) -> Self {
        Scanner { src, bytes: src.as_bytes(), start: 0, current: 0, line: 1 }
    }

    fn is_at_end(&self) -> bool {
        self.current >= self.bytes.len()
    }

    fn advance(&mut self) -> u8 {
        let c = self.bytes[self.current];
        self.current += 1;
        c
    }

    fn peek(&self) -> u8 {
        if self.current < self.bytes.len() { self.bytes[self.current] } else { 0 }
    }

    fn peek_next(&self) -> u8 {
        if self.current + 1 < self.bytes.len() { self.bytes[self.current + 1] } else { 0 }
    }

    fn matches(&mut self, expected: u8) -> bool {
        if self.is_at_end() || self.bytes[self.current] != expected {
            return false;
        }
        self.current += 1;
        true
    }

    fn make(&self, ty: TokenType) -> Token<'a> {
        Token { ty, lexeme: &self.src[self.start..self.current], line: self.line }
    }

    fn error(&self, msg: &'static str) -> Token<'a> {
        Token { ty: TokenType::Error, lexeme: msg, line: self.line }
    }

    fn skip_whitespace(&mut self) {
        loop {
            match self.peek() {
                b' ' | b'\r' | b'\t' => {
                    self.current += 1;
                }
                b'\n' => {
                    self.line += 1;
                    self.current += 1;
                }
                b'/' => {
                    if self.peek_next() == b'/' {
                        while self.peek() != b'\n' && !self.is_at_end() {
                            self.current += 1;
                        }
                    } else {
                        return;
                    }
                }
                _ => return,
            }
        }
    }

    pub fn scan_token(&mut self) -> Token<'a> {
        self.skip_whitespace();
        self.start = self.current;
        if self.is_at_end() {
            return self.make(TokenType::Eof);
        }
        let c = self.advance();
        if is_alpha(c) {
            return self.identifier();
        }
        if c.is_ascii_digit() {
            return self.number();
        }
        use TokenType::*;
        match c {
            b'(' => self.make(LeftParen),
            b')' => self.make(RightParen),
            b'{' => self.make(LeftBrace),
            b'}' => self.make(RightBrace),
            b';' => self.make(Semicolon),
            b',' => self.make(Comma),
            b'.' => self.make(Dot),
            b'-' => self.make(Minus),
            b'+' => self.make(Plus),
            b'/' => self.make(Slash),
            b'*' => self.make(Star),
            b'!' => {
                let t = if self.matches(b'=') { BangEqual } else { Bang };
                self.make(t)
            }
            b'=' => {
                let t = if self.matches(b'=') { EqualEqual } else { Equal };
                self.make(t)
            }
            b'<' => {
                let t = if self.matches(b'=') { LessEqual } else { Less };
                self.make(t)
            }
            b'>' => {
                let t = if self.matches(b'=') { GreaterEqual } else { Greater };
                self.make(t)
            }
            b'"' => self.string(),
            _ => self.error("Unexpected character."),
        }
    }

    fn string(&mut self) -> Token<'a> {
        while self.peek() != b'"' && !self.is_at_end() {
            if self.peek() == b'\n' {
                self.line += 1;
            }
            self.current += 1;
        }
        if self.is_at_end() {
            return self.error("Unterminated string.");
        }
        self.current += 1;
        self.make(TokenType::String)
    }

    fn number(&mut self) -> Token<'a> {
        while self.peek().is_ascii_digit() {
            self.current += 1;
        }
        if self.peek() == b'.' && self.peek_next().is_ascii_digit() {
            self.current += 1;
            while self.peek().is_ascii_digit() {
                self.current += 1;
            }
        }
        self.make(TokenType::Number)
    }

    fn identifier(&mut self) -> Token<'a> {
        while is_alpha(self.peek()) || self.peek().is_ascii_digit() {
            self.current += 1;
        }
        use TokenType::*;
        let ty = match &self.src[self.start..self.current] {
            "and" => And,
            "class" => Class,
            "else" => Else,
            "false" => False,
            "for" => For,
            "fun" => Fun,
            "if" => If,
            "nil" => Nil,
            "or" => Or,
            "print" => Print,
            "return" => Return,
            "super" => Super,
            "this" => This,
            "true" => True,
            "var" => Var,
            "while" => While,
            _ => Identifier,
        };
        self.make(ty)
    }
}

fn is_alpha(c: u8) -> bool {
    c.is_ascii_alphabetic() || c == b'_'
}

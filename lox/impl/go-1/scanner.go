package main

type TokenType uint8

const (
	TLeftParen TokenType = iota
	TRightParen
	TLeftBrace
	TRightBrace
	TComma
	TDot
	TMinus
	TPlus
	TSemicolon
	TSlash
	TStar
	TBang
	TBangEqual
	TEqual
	TEqualEqual
	TGreater
	TGreaterEqual
	TLess
	TLessEqual
	TIdentifier
	TString
	TNumber
	TAnd
	TClass
	TElse
	TFalse
	TFor
	TFun
	TIf
	TNil
	TOr
	TPrint
	TReturn
	TSuper
	TThis
	TTrue
	TVar
	TWhile
	TError
	TEOF
	tokenCount
)

type Token struct {
	typ    TokenType
	lexeme string
	line   int
}

type Scanner struct {
	src     string
	start   int
	current int
	line    int
}

func newScanner(src string) *Scanner {
	return &Scanner{src: src, line: 1}
}

func (s *Scanner) atEnd() bool { return s.current >= len(s.src) }

func (s *Scanner) advance() byte {
	c := s.src[s.current]
	s.current++
	return c
}

func (s *Scanner) peek() byte {
	if s.atEnd() {
		return 0
	}
	return s.src[s.current]
}

func (s *Scanner) peekNext() byte {
	if s.current+1 >= len(s.src) {
		return 0
	}
	return s.src[s.current+1]
}

func (s *Scanner) match(c byte) bool {
	if s.atEnd() || s.src[s.current] != c {
		return false
	}
	s.current++
	return true
}

func (s *Scanner) make(t TokenType) Token {
	return Token{typ: t, lexeme: s.src[s.start:s.current], line: s.line}
}

func (s *Scanner) errorToken(msg string) Token {
	return Token{typ: TError, lexeme: msg, line: s.line}
}

func (s *Scanner) skipWhitespace() {
	for !s.atEnd() {
		switch s.peek() {
		case ' ', '\r', '\t':
			s.current++
		case '\n':
			s.line++
			s.current++
		case '/':
			if s.peekNext() == '/' {
				for !s.atEnd() && s.peek() != '\n' {
					s.current++
				}
			} else {
				return
			}
		default:
			return
		}
	}
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

var keywords = map[string]TokenType{
	"and": TAnd, "class": TClass, "else": TElse, "false": TFalse, "for": TFor,
	"fun": TFun, "if": TIf, "nil": TNil, "or": TOr, "print": TPrint,
	"return": TReturn, "super": TSuper, "this": TThis, "true": TTrue,
	"var": TVar, "while": TWhile,
}

func (s *Scanner) scanToken() Token {
	s.skipWhitespace()
	s.start = s.current
	if s.atEnd() {
		return s.make(TEOF)
	}
	c := s.advance()
	if isAlpha(c) {
		for isAlpha(s.peek()) || isDigit(s.peek()) {
			s.current++
		}
		if t, ok := keywords[s.src[s.start:s.current]]; ok {
			return s.make(t)
		}
		return s.make(TIdentifier)
	}
	if isDigit(c) {
		for isDigit(s.peek()) {
			s.current++
		}
		if s.peek() == '.' && isDigit(s.peekNext()) {
			s.current++
			for isDigit(s.peek()) {
				s.current++
			}
		}
		return s.make(TNumber)
	}
	switch c {
	case '(':
		return s.make(TLeftParen)
	case ')':
		return s.make(TRightParen)
	case '{':
		return s.make(TLeftBrace)
	case '}':
		return s.make(TRightBrace)
	case ';':
		return s.make(TSemicolon)
	case ',':
		return s.make(TComma)
	case '.':
		return s.make(TDot)
	case '-':
		return s.make(TMinus)
	case '+':
		return s.make(TPlus)
	case '/':
		return s.make(TSlash)
	case '*':
		return s.make(TStar)
	case '!':
		if s.match('=') {
			return s.make(TBangEqual)
		}
		return s.make(TBang)
	case '=':
		if s.match('=') {
			return s.make(TEqualEqual)
		}
		return s.make(TEqual)
	case '<':
		if s.match('=') {
			return s.make(TLessEqual)
		}
		return s.make(TLess)
	case '>':
		if s.match('=') {
			return s.make(TGreaterEqual)
		}
		return s.make(TGreater)
	case '"':
		for !s.atEnd() && s.peek() != '"' {
			if s.peek() == '\n' {
				s.line++
			}
			s.current++
		}
		if s.atEnd() {
			return s.errorToken("Unterminated string.")
		}
		s.current++
		return s.make(TString)
	}
	return s.errorToken("Unexpected character.")
}

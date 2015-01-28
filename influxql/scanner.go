package influxql

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Scanner represents a lexical scanner for InfluxQL.
type Scanner struct {
	r *reader
}

// NewScanner returns a new instance of Scanner.
func NewScanner(r io.Reader) *Scanner {
	return &Scanner{r: &reader{r: bufio.NewReader(r)}}
}

// Scan returns the next token and position from the underlying reader.
// Also returns the literal text read for strings, numbers, and duration tokens
// since these token types can have different literal representations.
func (s *Scanner) Scan() (tok Token, pos Pos, lit string) {
	// Read next code point.
	ch0, pos := s.r.read()

	// If we see whitespace then consume all contiguous whitespace.
	// If we see a letter then consume as an ident or reserved word.
	if isWhitespace(ch0) {
		return s.scanWhitespace()
	} else if isLetter(ch0) {
		s.r.unread()
		return s.scanIdent()
	} else if isDigit(ch0) {
		return s.scanNumber()
	}

	// Otherwise parse individual characters.
	switch ch0 {
	case eof:
		return EOF, pos, ""
	case '"':
		s.r.unread()
		return s.scanIdent()
	case '\'':
		return s.scanString()
	case '.':
		ch1, _ := s.r.read()
		s.r.unread()
		if isDigit(ch1) {
			return s.scanNumber()
		}
		return DOT, pos, ""
	case '+', '-':
		return s.scanNumber()
	case '*':
		return MUL, pos, ""
	case '/':
		return DIV, pos, ""
	case '=':
		return EQ, pos, ""
	case '>':
		if ch1, _ := s.r.read(); ch1 == '=' {
			return GTE, pos, ""
		}
		s.r.unread()
		return GT, pos, ""
	case '<':
		if ch1, _ := s.r.read(); ch1 == '=' {
			return LTE, pos, ""
		} else if ch1 == '>' {
			return NEQ, pos, ""
		}
		s.r.unread()
		return LT, pos, ""
	case '(':
		return LPAREN, pos, ""
	case ')':
		return RPAREN, pos, ""
	case ',':
		return COMMA, pos, ""
	case ';':
		return SEMICOLON, pos, ""
	}

	return ILLEGAL, pos, string(ch0)
}

// scanWhitespace consumes the current rune and all contiguous whitespace.
func (s *Scanner) scanWhitespace() (tok Token, pos Pos, lit string) {
	// Create a buffer and read the current character into it.
	var buf bytes.Buffer
	ch, pos := s.r.curr()
	_, _ = buf.WriteRune(ch)

	// Read every subsequent whitespace character into the buffer.
	// Non-whitespace characters and EOF will cause the loop to exit.
	for {
		ch, _ = s.r.read()
		if ch == eof {
			break
		} else if !isWhitespace(ch) {
			s.r.unread()
			break
		} else {
			_, _ = buf.WriteRune(ch)
		}
	}

	return WS, pos, buf.String()
}

// scanIdent a fully qualified identifier.
func (s *Scanner) scanIdent() (tok Token, pos Pos, lit string) {
	_, pos = s.r.read()
	s.r.unread()

	var buf bytes.Buffer
	for {
		ch, _ := s.r.read()
		if ch == eof {
			break
		} else if ch == '.' {
			buf.WriteRune(ch)
		} else if ch == '"' {
			if tok0, pos0, lit0 := s.scanString(); tok == BADSTRING || tok == BADESCAPE {
				return tok0, pos0, lit0
			} else {
				_ = buf.WriteByte('"')
				_, _ = buf.WriteString(lit0)
				_ = buf.WriteByte('"')
			}
		} else if isIdentChar(ch) {
			s.r.unread()
			buf.WriteString(ScanBareIdent(s.r))
		} else {
			s.r.unread()
			break
		}
	}
	lit = buf.String()

	// If the literal matches a keyword then return that keyword.
	if tok = Lookup(lit); tok != IDENT {
		return tok, pos, ""
	}

	return IDENT, pos, lit
}

// scanString consumes a contiguous string of non-quote characters.
// Quote characters can be consumed if they're first escaped with a backslash.
func (s *Scanner) scanString() (tok Token, pos Pos, lit string) {
	s.r.unread()
	_, pos = s.r.curr()

	var err error
	lit, err = ScanString(s.r)
	if err == errBadString {
		return BADSTRING, pos, lit
	} else if err == errBadEscape {
		_, pos = s.r.curr()
		return BADESCAPE, pos, lit
	}
	return STRING, pos, lit
}

// scanNumber consumes anything that looks like the start of a number.
// Numbers start with a digit, full stop, plus sign or minus sign.
// This function can return non-number tokens if a scan is a false positive.
// For example, a minus sign followed by a letter will just return a minus sign.
func (s *Scanner) scanNumber() (tok Token, pos Pos, lit string) {
	var buf bytes.Buffer

	// Check if the initial rune is a "+" or "-".
	ch, pos := s.r.curr()
	if ch == '+' || ch == '-' {
		// Peek at the next two runes.
		ch1, _ := s.r.read()
		ch2, _ := s.r.read()
		s.r.unread()
		s.r.unread()

		// This rune must be followed by a digit or a full stop and a digit.
		if isDigit(ch1) || (ch1 == '.' && isDigit(ch2)) {
			_, _ = buf.WriteRune(ch)
		} else if ch == '+' {
			return ADD, pos, ""
		} else if ch == '-' {
			return SUB, pos, ""
		}
	} else if ch == '.' {
		// Peek and see if the next rune is a digit.
		ch1, _ := s.r.read()
		s.r.unread()
		if !isDigit(ch1) {
			return ILLEGAL, pos, "."
		}

		// Unread the full stop so we can read it later.
		s.r.unread()
	} else {
		s.r.unread()
	}

	// Read as many digits as possible.
	_, _ = buf.WriteString(s.scanDigits())

	// If next code points are a full stop and digit then consume them.
	if ch0, _ := s.r.read(); ch0 == '.' {
		if ch1, _ := s.r.read(); isDigit(ch1) {
			_, _ = buf.WriteRune(ch0)
			_, _ = buf.WriteRune(ch1)
			_, _ = buf.WriteString(s.scanDigits())
		} else {
			s.r.unread()
			s.r.unread()
		}
	} else {
		s.r.unread()
	}

	// Attempt to read as a duration if it doesn't have a fractional part.
	if !strings.Contains(buf.String(), ".") {
		// If the next rune is a duration unit (u,µ,ms,s) then return a duration token
		if ch0, _ := s.r.read(); ch0 == 'u' || ch0 == 'µ' || ch0 == 's' || ch0 == 'h' || ch0 == 'd' || ch0 == 'w' {
			_, _ = buf.WriteRune(ch0)
			return DURATION_VAL, pos, buf.String()
		} else if ch0 == 'm' {
			_, _ = buf.WriteRune(ch0)
			if ch1, _ := s.r.read(); ch1 == 's' {
				_, _ = buf.WriteRune(ch1)
			} else {
				s.r.unread()
			}
			return DURATION_VAL, pos, buf.String()
		}
		s.r.unread()
	}
	return NUMBER, pos, buf.String()
}

// scanDigits consume a contiguous series of digits.
func (s *Scanner) scanDigits() string {
	var buf bytes.Buffer
	for {
		ch, _ := s.r.read()
		if !isDigit(ch) {
			s.r.unread()
			break
		}
		_, _ = buf.WriteRune(ch)
	}
	return buf.String()
}

// isWhitespace returns true if the rune is a space, tab, or newline.
func isWhitespace(ch rune) bool { return ch == ' ' || ch == '\t' || ch == '\n' }

// isLetter returns true if the rune is a letter.
func isLetter(ch rune) bool { return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') }

// isDigit returns true if the rune is a digit.
func isDigit(ch rune) bool { return (ch >= '0' && ch <= '9') }

// isIdentChar returns true if the rune that be used in a bare identifier.
func isIdentChar(ch rune) bool { return isLetter(ch) || isDigit(ch) || ch == '_' }

// bufScanner represents a wrapper for scanner to add a buffer.
// It provides a fixed-length circular buffer that can be unread.
type bufScanner struct {
	s   *Scanner
	i   int // buffer index
	n   int // buffer size
	buf [3]struct {
		tok Token
		pos Pos
		lit string
	}
}

// newBufScanner returns a new buffered scanner for a reader.
func newBufScanner(r io.Reader) *bufScanner {
	return &bufScanner{s: NewScanner(r)}
}

// Scan reads the next token from the scanner.
func (s *bufScanner) Scan() (tok Token, pos Pos, lit string) {
	// If we have unread tokens then read them off the buffer first.
	if s.n > 0 {
		s.n--
		return s.curr()
	}

	// Move buffer position forward and save the token.
	s.i = (s.i + 1) % len(s.buf)
	buf := &s.buf[s.i]
	buf.tok, buf.pos, buf.lit = s.s.Scan()

	return s.curr()
}

// Unscan pushes the previously token back onto the buffer.
func (s *bufScanner) Unscan() { s.n++ }

// curr returns the last read token.
func (s *bufScanner) curr() (tok Token, pos Pos, lit string) {
	buf := &s.buf[(s.i-s.n+len(s.buf))%len(s.buf)]
	return buf.tok, buf.pos, buf.lit
}

// reader represents a buffered rune reader used by the scanner.
// It provides a fixed-length circular buffer that can be unread.
type reader struct {
	r   io.RuneScanner
	i   int // buffer index
	n   int // buffer char count
	pos Pos // last read rune position
	buf [3]struct {
		ch  rune
		pos Pos
	}
	eof bool // true if reader has ever seen eof.
}

// ReadRune reads the next rune from the reader.
// This is a wrapper function to implement the io.RuneReader interface.
// Note that this function does not return size.
func (r *reader) ReadRune() (ch rune, size int, err error) {
	ch, _ = r.read()
	if ch == eof {
		err = io.EOF
	}
	return
}

// UnreadRune pushes the previously read rune back onto the buffer.
// This is a wrapper function to implement the io.RuneScanner interface.
func (r *reader) UnreadRune() error {
	r.unread()
	return nil
}

// read reads the next rune from the reader.
func (r *reader) read() (ch rune, pos Pos) {
	// If we have unread characters then read them off the buffer first.
	if r.n > 0 {
		r.n--
		return r.curr()
	}

	// Read next rune from underlying reader.
	// Any error (including io.EOF) should return as EOF.
	ch, _, err := r.r.ReadRune()
	if err != nil {
		ch = eof
	} else if ch == '\r' {
		if ch, _, err := r.r.ReadRune(); err != nil {
			// nop
		} else if ch != '\n' {
			_ = r.r.UnreadRune()
		}
		ch = '\n'
	}

	// Save character and position to the buffer.
	r.i = (r.i + 1) % len(r.buf)
	buf := &r.buf[r.i]
	buf.ch, buf.pos = ch, r.pos

	// Update position.
	// Only count EOF once.
	if ch == '\n' {
		r.pos.Line++
		r.pos.Char = 0
	} else if !r.eof {
		r.pos.Char++
	}

	// Mark the reader as EOF.
	// This is used so we don't double count EOF characters.
	if ch == eof {
		r.eof = true
	}

	return r.curr()
}

// unread pushes the previously read rune back onto the buffer.
func (r *reader) unread() {
	r.n++
}

// curr returns the last read character and position.
func (r *reader) curr() (ch rune, pos Pos) {
	i := (r.i - r.n + len(r.buf)) % len(r.buf)
	buf := &r.buf[i]
	return buf.ch, buf.pos
}

// eof is a marker code point to signify that the reader can't read any more.
const eof = rune(0)

// ScanString reads a quoted string from a rune reader.
func ScanString(r io.RuneScanner) (string, error) {
	ending, _, err := r.ReadRune()
	if err != nil {
		return "", errBadString
	}

	var buf bytes.Buffer
	for {
		ch0, _, err := r.ReadRune()
		if ch0 == ending {
			return buf.String(), nil
		} else if err != nil || ch0 == '\n' {
			return buf.String(), errBadString
		} else if ch0 == '\\' {
			// If the next character is an escape then write the escaped char.
			// If it's not a valid escape then return an error.
			ch1, _, _ := r.ReadRune()
			if ch1 == 'n' {
				_, _ = buf.WriteRune('\n')
			} else if ch1 == '\\' {
				_, _ = buf.WriteRune('\\')
			} else if ch1 == '"' {
				_, _ = buf.WriteRune('"')
			} else {
				return string(ch0) + string(ch1), errBadEscape
			}
		} else {
			_, _ = buf.WriteRune(ch0)
		}
	}
}

var errBadString = errors.New("bad string")
var errBadEscape = errors.New("bad escape")

// ScanBareIdent reads bare identifier from a rune reader.
func ScanBareIdent(r io.RuneScanner) string {
	// Read every ident character into the buffer.
	// Non-ident characters and EOF will cause the loop to exit.
	var buf bytes.Buffer
	for {
		ch, _, err := r.ReadRune()
		if err != nil {
			break
		} else if !isIdentChar(ch) {
			r.UnreadRune()
			break
		} else {
			_, _ = buf.WriteRune(ch)
		}
	}
	return buf.String()
}

// SplitIdent splits an identifier into quoted & unquoted segments.
func SplitIdent(s string) (segments []string, err error) {
	var isBareIdent bool

	// Scan over buffered rune reader.
	r := strings.NewReader(s)
	for {
		// If next character is EOF, return an error.
		// If next character is a dot then add an empty segment.
		// If next character is a quote then parse quoted string.
		// Otherwise parse as a bare ident.
		if ch, _, err := r.ReadRune(); err == io.EOF {
			return nil, errInvalidIdentifier

		} else if ch == '.' {
			// Disallow a starting dot.
			if len(segments) == 0 {
				return nil, errInvalidIdentifier
			}
			// Otherwise append blank segment and continue.
			segments = append(segments, "")
			isBareIdent = false
			continue

		} else if ch == '"' {
			_ = r.UnreadRune()
			segment, err := ScanString(r)
			if err != nil {
				return nil, err
			}
			segments = append(segments, segment)
			isBareIdent = false

		} else if isIdentChar(ch) {
			_ = r.UnreadRune()
			segment := ScanBareIdent(r)

			// Append to previous segment if it was a bare ident.
			// Otherwise add new segment.
			if isBareIdent {
				segments[len(segments)-1] = segments[len(segments)-1] + "." + segment
			} else {
				segments = append(segments, segment)
			}
			isBareIdent = true

		} else {
			return nil, errInvalidIdentifier
		}

		// If next character is EOF, return.
		// If next character is a dot, continue.
		// Otherwise return an error.
		if ch, _, err := r.ReadRune(); err != nil {
			return segments, nil
		} else if ch == '.' {
			continue
		} else {
			return nil, errInvalidIdentifier
		}
	}
}

var errInvalidIdentifier = errors.New("invalid identifier")

// assert will panic with a given formatted message if the given condition is false.
func assert(condition bool, msg string, v ...interface{}) {
	if !condition {
		panic(fmt.Sprintf("assert failed: "+msg, v...))
	}
}

func warn(v ...interface{})              { fmt.Fprintln(os.Stderr, v...) }
func warnf(msg string, v ...interface{}) { fmt.Fprintf(os.Stderr, msg+"\n", v...) }

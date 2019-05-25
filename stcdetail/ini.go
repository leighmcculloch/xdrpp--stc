package stcdetail

import "bytes"
import "fmt"
import "io/ioutil"
import "os"
import "strings"

const tabwidth = 8
const eofRune rune = -1

// Section of an INI file.
type IniSection struct {
	Section string
	Subsection *string
}

func (s IniSection) String() string {
	if s.Subsection != nil {
		return fmt.Sprintf("[%s %q]", s.Section, *s.Subsection)
	}
	return fmt.Sprintf("[%s]", s.Section)
}

// Type that receives and processes the parsed INI file.
type IniSink interface {
	Value(sec *IniSection, k string, v string) error
}

// If an IniSink also implements IniSecSink, then it will receive a
// callback for each new section of the file.  This allows the cost of
// looking up a section to be amortized over multiple key=value pairs.
// (The Value method of an IniSecSink can reasonably ignore its sec
// argument.)
type IniSecSink interface {
	IniSink
	Section(sec IniSection) error
}

// Error that an IniSink's Value method should return when there is a
// problem with the key, rather than a problem with the value.  By
// default, the line and column number of an error will correspond to
// the start of the value, but with BadKey the error will point to the
// key.
type BadKey string
func (err BadKey) Error() string {
	return string(err)
}

// Just a random error type useful for bad values in INI files.
// Exists for symmetry with BadKey, though BadValue is in no way
// special.
type BadValue string
func (err BadValue) Error() string {
	return string(err)
}

// A single parse error in an IniFile.
type ParseError struct {
	File string
	Lineno, Colno int
	Msg string
}
func (err ParseError) Error() string {
	if err.File == "" {
		return fmt.Sprintf("%d:%d: %s", err.Lineno, err.Colno, err.Msg)
	}
	return fmt.Sprintf("%s:%d:%d: %s", err.File, err.Lineno, err.Colno, err.Msg)
}

// The collection of parse errors that resulted from parsing a file.
type ParseErrors []ParseError
func (err ParseErrors) Error() string {
	ret := &strings.Builder{}
	for i, e := range err {
		if i != 0 {
			ret.WriteByte('\n')
		}
		ret.WriteString(e.Error())
	}
	return ret.String()
}

type position struct {
	lineno, colno int
}

type iniParse struct {
	position
	input []byte
	file string
	sec *IniSection
	Value func (sec *IniSection, k string, v string) error
	Section func (sec IniSection) error
}

func (l *iniParse) throwAt(pos position, msg string) {
	panic(ParseError{
		File: l.file,
		Lineno: pos.lineno + 1,
		Colno: pos.colno + 1,
		Msg: msg,
	})
}

func (l *iniParse) throw(msg string, args ...interface{}) {
	l.throwAt(l.position, fmt.Sprintf(msg, args...))
}

func (l *iniParse) peek() rune {
	if len(l.input) == 0 {
		return eofRune
	}
	return rune(l.input[0])
}

func (l *iniParse) skip(n int) {
	if n > len(l.input) {
		n = len(l.input)
	}
	for i := 0; i < n; i++ {
		switch l.input[i] {
		case '\n':
			l.lineno++
			l.colno = 0
		case '\t':
			l.colno += 8 - (l.colno % tabwidth)
		default:
			l.colno++
		}
	}
	l.input = l.input[n:]
}

func (l *iniParse) take(n int) string {
	ret := string(l.input[:n])
	l.skip(n)
	return ret
}

func (l *iniParse) match(text string) bool {
	n := len(text)
	if len(l.input) >= n && string(l.input[:n]) == text {
		l.skip(n)
		return true
	}
	return false
}

func (l *iniParse) skipWhile(fn func(rune)bool) bool {
	var n int
	for n = 0; n < len(l.input) && fn(rune(l.input[n])); n++ {
	}
	if n > 0 {
		l.skip(n)
		return true
	}
	return false
}

func (l *iniParse) skipTo(c byte) bool {
	if i := bytes.IndexByte(l.input, c); i >= 0 {
		l.skip(i)
		return true
	}
	l.skip(len(l.input))
	return false
}

func (l *iniParse) takeWhile(fn func(rune)bool) string {
	var n int
	for n = 0; n < len(l.input) && fn(rune(l.input[n])); n++ {
	}
	return l.take(n)
}

func (l *iniParse) skipWS() bool {
	return l.skipWhile(func (r rune) bool { return r == ' ' || r == '\t' })
}

func isAlpha(c rune) bool {
	c &^= 0x20
	return c >= 'A' && c <= 'Z'
}
func isKeyChar(c rune) bool {
	return isAlpha(c) || (c >= '0' && c <= '9') || c == '-'
}

func (l *iniParse) getKey() string {
	return l.takeWhile(isKeyChar)
}

func (l *iniParse) getSubsection() *string {
	if len(l.input) < 2 || l.input[0] != '"' {
		return nil
	}
	ret := &strings.Builder{}
	var i int
loop:
	for i = 1; i + 1 < len(l.input); i++ {
		switch c := l.input[i]; c {
		case '"':
			break loop
		case '\000', '\n':
			return nil
		case '\\':
			nc := l.input[i+1]
			if nc == '\\' || nc == '"' {
				ret.WriteByte(nc)
			}
			i++
		default:
			ret.WriteByte(c)
		}
	}
	if l.input[i] != '"' {
		return nil
	}
	l.skip(i+1)
	s := ret.String()
	return &s
}

func (l *iniParse) getSection() *IniSection {
	if !l.match("[") {
		return nil
	}
	var ret IniSection
	ret.Section = l.getKey()
	if len(ret.Section) == 0 {
		l.throw("expected section name after '['")
	}
	if l.match("]") {
		return &ret
	}
	if !l.skipWS() {
		l.throw("expected ']' or space followed by quoted-subsection")
	}
	if ret.Subsection = l.getSubsection(); ret.Subsection == nil {
		l.throw("expected quoted subsection after space")
	}
	if !l.match("]") {
		l.throw("expected ']'")
	}
	return &ret
}

func (l *iniParse) getValue() string {
	ret := strings.Builder{}
	escape, inquote := false, false
	var i int
	for {
		var c rune
		if i < len(l.input) {
			c = rune(l.input[i])
		} else {
			c = eofRune
		}
		i++
		if escape {
			escape = false
			switch c {
			case '"', '\\':
				ret.WriteByte(byte(c))
			case 'n':
				ret.WriteByte('\n')
			case 't':
				ret.WriteByte('\t')
			case 'b':
				ret.WriteByte('\b')
			case '\n':
				// ignore
			default:
				l.skip(i-2)
				if c == eofRune {
					l.throw("incomplete escape sequence at EOF")
				}
				l.throw("invalid escape sequence \\%c", c)
			}
		} else if c == '\\' {
			escape = true
		} else if c == '"' {
			inquote = !inquote
		} else if c == '\n' || c == eofRune {
			if inquote {
				l.skip(i-1)
				l.throw("missing close quotes")
			}
			l.skip(i)
			return ret.String()
		} else if !inquote && (c == '#' || c == ';') {
			l.skip(i)
			l.skipTo('\n')
		} else {
			ret.WriteByte(byte(c))
		}
	}
}

func (l *iniParse) do1() (err *ParseError) {
	defer func() {
		if i := recover(); i != nil {
			if pe, ok := i.(ParseError); ok {
				err = &pe
				l.skipTo('\n')
			} else {
				panic(i)
			}
		}
	}()
	l.skipWS()
	keypos := l.position
	if sec := l.getSection(); sec != nil {
		l.sec = sec
		if err := l.Section(*sec); err != nil {
			l.throwAt(keypos, err.Error())
		}
	} else if isAlpha(l.peek()) {
		k := l.getKey()
		l.skipWS()
		var v string
		var valpos position
		if !l.match("=") {
			if c := l.peek();
			c != '\n' && c != '#' && c != ';' && c != eofRune {
				l.throw("Expected '=' after %s", k)
			}
			valpos = l.position
		} else {
			l.skipWS()
			valpos = l.position
			v = l.getValue()
		}
		if err := l.Value(l.sec, k, v); err != nil {
			if ke, ok := err.(BadKey); ok {
				l.throwAt(keypos, string(ke))
			} else {
				l.throwAt(valpos, err.Error())
			}
		}
	} else if c := l.peek(); c == '#' || c == ';' || c == '\n' {
		l.skipTo('\n')
		l.skip(1)
	} else {
		l.throw("Expected section or key")
	}
	return
}

func (l *iniParse) do() error {
	var err ParseErrors
	for len(l.input) > 0 {
		if e := l.do1(); e != nil {
			err = append(err, *e)
		}
	}
	if err == nil {
		return nil
	}
	return err
}

func newParser(sink IniSink, path string, input []byte) *iniParse {
	var ret iniParse
	ret.file = path
	ret.input = input
	ret.Value = sink.Value
	if iss, ok := sink.(IniSecSink); ok {
		ret.Section = iss.Section
	} else {
		ret.Section = func(IniSection) error { return nil }
	}
	return &ret
}

// Parse the contents of an INI file.  The filename argument is used
// only for error messages.
func IniParseContents(sink IniSink, filename string, contents []byte) error {
	return newParser(sink, filename, contents).do()
}

// Open, read, and parse an INI file.  If the file is incorrectly
// formatted, will return an error of type ParseErrors.
func IniParse(sink IniSink, filename string) error {
	if f, err := os.Open(filename); err != nil {
		return err
	} else {
		contents, err := ioutil.ReadAll(f)
		f.Close()
		if err != nil {
			return err
		}
		return newParser(sink, filename, contents).do()
	}
}
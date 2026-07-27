package run

import (
	"errors"
	"strings"
)

// safeCharacters is exactly the set Python's shlex.quote leaves alone. It is
// reproduced byte for byte rather than approximated with a "looks harmless"
// rule, because Quote and Split are a pair: anything Quote emits, Split must
// give back unchanged, and the Python original is the conformance oracle for
// both. Note what is absent — `~`, `*`, `!`, `#`, `^`, `{`, `}` — all of which
// a shell would otherwise reinterpret.
const safeCharacters = "%+,-./0123456789:=@ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijklmnopqrstuvwxyz"

var (
	errNoClosingQuotation = errors.New("no closing quotation")
	errNoEscapedCharacter = errors.New("no escaped character")
)

// Quote returns value as exactly one POSIX shell token, matching shlex.quote.
//
// The empty string has to become an explicit empty quoted token: bare, it would
// vanish in the split and silently shift every later argument left by one. A
// value made only of safe characters — every path, UUID and unit name this
// codebase passes — is returned untouched, which keeps the trace readable.
func Quote(value string) string {
	if value == "" {
		return "''"
	}
	if isShellSafe(value) {
		return value
	}
	// Single quotes make every byte literal, so a `;`, a `$(…)`, a newline or a
	// brace is inert. The one byte they cannot carry is the single quote itself,
	// which therefore leaves and re-enters the quoting: '"'"'.
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// isShellSafe reports whether every byte of value needs no quoting. Scanning
// bytes also settles the non-ASCII case the way Python's isascii() check does:
// a UTF-8 continuation byte is not in the safe set, so any non-ASCII value gets
// quoted.
func isShellSafe(value string) bool {
	for index := 0; index < len(value); index++ {
		if strings.IndexByte(safeCharacters, value[index]) < 0 {
			return false
		}
	}
	return true
}

// join renders an argv back into a copy-pasteable command line for the trace
// and for CommandError — shlex.join.
func join(argv []string) string {
	quoted := make([]string, len(argv))
	for index, argument := range argv {
		quoted[index] = Quote(argument)
	}
	return strings.Join(quoted, " ")
}

// Split parses a rendered command line into an argv, honouring the quoting
// Quote emits. It is a port of shlex.split — POSIX mode, whitespace splitting,
// comments off — restricted to that one configuration, which is the only one
// this package ever asks for. In particular `#` is an ordinary character here:
// a comment rule would eat half of a URL fragment or an nft counter name.
func Split(line string) ([]string, error) {
	parser := splitter{line: line}
	return parser.split()
}

// splitter is one pass over a command line. quoted is tracked separately from
// the token's length because an empty token is only an argument when it was
// written as an empty quoted token: `a  b` is two arguments, and `a` followed
// by an empty quoted token followed by `b` is three.
type splitter struct {
	line   string
	index  int
	token  strings.Builder
	quoted bool
	argv   []string
}

func (splitter *splitter) split() ([]string, error) {
	for splitter.index < len(splitter.line) {
		character := splitter.line[splitter.index]
		splitter.index++
		if err := splitter.consume(character); err != nil {
			return nil, err
		}
	}
	splitter.endToken()
	return splitter.argv, nil
}

func (splitter *splitter) consume(character byte) error {
	switch character {
	case ' ', '\t', '\r', '\n':
		splitter.endToken()
	case '\'':
		return splitter.readSingleQuoted()
	case '"':
		return splitter.readDoubleQuoted()
	case '\\':
		return splitter.readEscape()
	default:
		splitter.token.WriteByte(character)
	}
	return nil
}

// readSingleQuoted copies bytes verbatim up to the closing quote. There are no
// escapes at all inside single quotes, which is precisely what makes Quote's
// output safe to re-parse.
func (splitter *splitter) readSingleQuoted() error {
	splitter.quoted = true
	for splitter.index < len(splitter.line) {
		character := splitter.line[splitter.index]
		splitter.index++
		if character == '\'' {
			return nil
		}
		splitter.token.WriteByte(character)
	}
	return errNoClosingQuotation
}

// readDoubleQuoted copies up to the closing quote, where a backslash escapes
// only a quote or another backslash. Before anything else the backslash is a
// literal byte, exactly as a POSIX shell treats it.
func (splitter *splitter) readDoubleQuoted() error {
	splitter.quoted = true
	for splitter.index < len(splitter.line) {
		character := splitter.line[splitter.index]
		splitter.index++
		switch character {
		case '"':
			return nil
		case '\\':
			if err := splitter.readEscapeInDoubleQuotes(); err != nil {
				return err
			}
		default:
			splitter.token.WriteByte(character)
		}
	}
	return errNoClosingQuotation
}

func (splitter *splitter) readEscapeInDoubleQuotes() error {
	if splitter.index >= len(splitter.line) {
		return errNoEscapedCharacter
	}
	character := splitter.line[splitter.index]
	splitter.index++
	if character != '"' && character != '\\' {
		splitter.token.WriteByte('\\')
	}
	splitter.token.WriteByte(character)
	return nil
}

// readEscape takes the next byte literally: outside quotes a backslash escapes
// anything, a space and a newline included.
func (splitter *splitter) readEscape() error {
	if splitter.index >= len(splitter.line) {
		return errNoEscapedCharacter
	}
	splitter.token.WriteByte(splitter.line[splitter.index])
	splitter.index++
	return nil
}

// endToken closes the token under construction, dropping it when it is both
// empty and unquoted — that is what turns a run of spaces into one separator.
func (splitter *splitter) endToken() {
	if splitter.token.Len() == 0 && !splitter.quoted {
		return
	}
	splitter.argv = append(splitter.argv, splitter.token.String())
	splitter.token.Reset()
	splitter.quoted = false
}

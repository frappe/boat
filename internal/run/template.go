package run

import (
	"fmt"
	"strings"
)

// hole is the literal two-character placeholder, and only that.
//
// Nothing here uses a general template engine on purpose. This codebase is
// brace-heavy — nft chain clauses (`{ type filter hook forward priority
// filter; policy accept; }`) and Firecracker JSON bodies appear verbatim inside
// command strings — and any engine that treats `{` as syntax would force every
// one of those braces to be doubled, which is a rule someone eventually forgets
// on the one line that matters. Matching the exact byte pair `{}` leaves every
// other brace byte for byte alone.
const hole = "{}"

// Substitute replaces each literal `{}` in template with a shell-quoted
// parameter, in order, and leaves every other character untouched.
//
// Quoting makes each parameter exactly ONE shell token: a value with an
// internal space, a `;`, a `|`, a `$(…)` or a quote cannot break out of its
// slot. This is the parameterized-SQL trust model — literal template, quoted
// holes — under which forgetting to quote is not expressible.
//
// A count mismatch is a programming bug and returns an error rather than a
// plausible-looking command line: rendering `rm -rf {}` with no parameter must
// never reach a host.
func Substitute(template string, parameters ...any) (string, error) {
	holes := strings.Count(template, hole)
	if holes != len(parameters) {
		return "", fmt.Errorf("%q: %d {} placeholder(s) but %d parameter(s)", template, holes, len(parameters))
	}
	var rendered strings.Builder
	remainder := template
	for _, parameter := range parameters {
		before, after, _ := strings.Cut(remainder, hole)
		rendered.WriteString(before)
		rendered.WriteString(Quote(fmt.Sprint(parameter)))
		remainder = after
	}
	// Whatever follows the last hole is literal template, and stays unquoted.
	rendered.WriteString(remainder)
	return rendered.String(), nil
}

// Render substitutes, then splits the finished line into a real argv for
// execution with no shell. The quoting in Substitute guarantees each parameter
// survives the split as exactly one argv entry, while literals in the template
// split on whitespace as written. There is no shell anywhere in this path.
func Render(template string, parameters ...any) ([]string, error) {
	rendered, err := Substitute(template, parameters...)
	if err != nil {
		return nil, err
	}
	return Split(rendered)
}

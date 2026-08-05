// Package allowlist checks the commands Boat renders against the sudoers
// allow-list that authorises them, because nothing else in the repo connects
// those two files.
//
// Four defects in one week had the same shape: a call site was added or
// tightened and `sudoers.d/boat` was left behind. Both halves look right on
// their own — the Go tests stub above the layer that renders the string, and a
// hand check on a host tests the commands you thought to type. The daemon is
// then denied at runtime, where a denial is not a crash but a wrong answer: a
// host full of VMs adopting none of them, a sleeping VM observed as stopped.
//
// So this package renders every `sudo ` template in the tree and asks the
// allow-list about each one, in both directions: a rendered command with no
// grant is a denial waiting to happen, and a grant nothing renders is a
// standing privilege with no caller — which the file's own header calls a
// liability, and which is how a grant can be wrong without anyone noticing.
package allowlist

import "strings"

// Match reports whether a sudoers command pattern authorises a command line,
// using sudo's own matching rules rather than Go's.
//
// The two halves match differently and the difference is the whole security
// model. The command PATH is matched with pathname semantics, so a `*` there
// cannot cross a `/` and `/usr/bin/*` does not reach `/usr/bin/../sbin/x`. The
// ARGUMENTS are matched as ONE string with the separators already joined in, so
// a `*` there matches anything at all — spaces included. That is why the
// allow-list spells UUIDs out character by character instead of writing `*`: a
// wildcard that can match a space is a wildcard that can grow the command a
// second operand.
func Match(pattern string, command string) bool {
	patternPath, patternArguments, patternHasArguments := strings.Cut(pattern, " ")
	commandPath, commandArguments, commandHasArguments := strings.Cut(command, " ")
	if !matchGlob(patternPath, commandPath, true) {
		return false
	}
	// A grant naming no arguments authorises the bare command and nothing else;
	// this is sudo's rule, and it is what stops `systemctl` (no arguments) from
	// being read as `systemctl` (any arguments).
	if !patternHasArguments || !commandHasArguments {
		return patternHasArguments == commandHasArguments
	}
	return matchGlob(patternArguments, commandArguments, false)
}

// matchGlob is fnmatch(3) over `*`, `?` and `[…]`. withPathSemantics is
// fnmatch's FNM_PATHNAME: it stops `*` and `?` at a `/`.
//
// The loop backtracks on the last `*` rather than recursing, so a pattern of
// many wildcards against a long argument string stays linear in practice —
// these run over every template in the tree on every `make check`.
func matchGlob(pattern string, text string, withPathSemantics bool) bool {
	patternIndex, textIndex := 0, 0
	starPattern, starText := -1, 0
	for textIndex < len(text) {
		switch {
		case patternIndex < len(pattern) && pattern[patternIndex] == '\\' && patternIndex+1 < len(pattern):
			// The allow-list escapes the characters sudoers itself would eat —
			// `fe80\:\:a` is one address, not three fields.
			if text[textIndex] != pattern[patternIndex+1] {
				break
			}
			patternIndex, textIndex = patternIndex+2, textIndex+1
			continue
		case patternIndex < len(pattern) && pattern[patternIndex] == '*':
			if withPathSemantics && text[textIndex] == '/' {
				break
			}
			starPattern, starText = patternIndex, textIndex
			patternIndex++
			continue
		case patternIndex < len(pattern) && pattern[patternIndex] == '?':
			if withPathSemantics && text[textIndex] == '/' {
				break
			}
			patternIndex, textIndex = patternIndex+1, textIndex+1
			continue
		case patternIndex < len(pattern) && pattern[patternIndex] == '[':
			width, ok := matchClass(pattern[patternIndex:], text[textIndex])
			if !ok {
				break
			}
			patternIndex, textIndex = patternIndex+width, textIndex+1
			continue
		case patternIndex < len(pattern) && pattern[patternIndex] == text[textIndex]:
			patternIndex, textIndex = patternIndex+1, textIndex+1
			continue
		}
		if starPattern < 0 {
			return false
		}
		// Backtrack: let the last `*` swallow one more character. Under pathname
		// semantics it may not swallow a `/`, which is what keeps a path grant
		// from reaching a directory deeper than it names.
		if withPathSemantics && text[starText] == '/' {
			return false
		}
		starText++
		patternIndex, textIndex = starPattern+1, starText
	}
	for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(pattern)
}

// matchClass matches one character against a `[…]` class and reports how much
// of the pattern the class consumed. A leading `!` or `^` negates it, and a `]`
// first in the class is a literal — both are fnmatch's rules, and the
// allow-list leans on the ranges heavily: a spelled-out UUID is 36 of these.
func matchClass(pattern string, character byte) (int, bool) {
	index := 1
	negated := false
	if index < len(pattern) && (pattern[index] == '!' || pattern[index] == '^') {
		negated, index = true, index+1
	}
	matched := false
	for first := true; index < len(pattern) && (first || pattern[index] != ']'); first = false {
		switch {
		case index+2 < len(pattern) && pattern[index+1] == '-' && pattern[index+2] != ']':
			if pattern[index] <= character && character <= pattern[index+2] {
				matched = true
			}
			index += 3
		default:
			if pattern[index] == character {
				matched = true
			}
			index++
		}
	}
	if index >= len(pattern) {
		// An unterminated `[` is a literal bracket to fnmatch, not a class.
		return 1, character == '['
	}
	return index + 1, matched != negated
}

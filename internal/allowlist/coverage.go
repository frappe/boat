package allowlist

import "strings"

// hole is what a template writes where a parameter goes. run.Render fills it
// with one shell-quoted value, so for the purpose of this check it stands for
// an arbitrary string.
const hole = "{}"

// Covers reports whether a grant could authorise a template under SOME choice
// of the values its holes are filled with.
//
// It answers the question that actually matters — "is there any grant for this
// call site at all?" — without inventing the values. The alternative was a
// fixture table naming a realistic value for all 176 holes, and that table
// would have been my guesses about the code, checked by nothing: where a guess
// was wrong the test would either fail on a working command or, far worse, pass
// on a broken one because the fabricated value was narrower than the real one.
//
// The cost of not guessing is stated plainly: a grant whose SHAPE is too narrow
// for the values production really passes — a five-digit port class against a
// four-digit port — is covered here and denied on the host. That is a second
// bug class, and the honest place to catch it is the live `sudo -n -l` probe
// the testing guide describes. What this catches is the class that has actually
// shipped four times: a call site with no grant whatsoever.
func Covers(pattern string, command string) bool {
	patternPath, patternArguments, patternHasArguments := strings.Cut(pattern, " ")
	commandPath, commandArguments, commandHasArguments := strings.Cut(command, " ")
	// Templates name a bare binary (`systemctl`), grants name the resolved path
	// (`/usr/bin/systemctl`), and which directory it resolves to is a fact about
	// the host rather than about the call site — so the base name is what these
	// two files can honestly be held to agree on.
	if baseName(patternPath) != commandPath {
		return false
	}
	if !patternHasArguments || !commandHasArguments {
		return patternHasArguments == commandHasArguments
	}
	return intersects(commandArguments, patternArguments)
}

func baseName(path string) string {
	if index := strings.LastIndex(path, "/"); index >= 0 {
		return path[index+1:]
	}
	return path
}

// state is one position in the template and one in the grant, as the two are
// walked over the same hypothetical rendered string.
type state struct{ template, grant int }

// intersects reports whether some string is both a rendering of the template
// and a match for the grant.
//
// Both sides are patterns, which is why this is a search rather than a match:
// the template's holes and the grant's `*`s each stand for text neither side
// pins down, and the question is whether they can agree on any of it. The
// search is over states rather than characters, so a template of many holes
// against a grant of many wildcards terminates on the visited set instead of
// backtracking forever.
func intersects(template string, grant string) bool {
	visited := map[state]bool{}
	var reachable func(state) bool
	reachable = func(current state) bool {
		if current.template == len(template) && current.grant == len(grant) {
			return true
		}
		if visited[current] {
			return false
		}
		visited[current] = true
		for _, next := range steps(template, grant, current) {
			if reachable(next) {
				return true
			}
		}
		return false
	}
	return reachable(state{})
}

// steps enumerates the moves out of one state: what the template can emit next,
// and what the grant can absorb.
func steps(template string, grant string, current state) []state {
	atHole := strings.HasPrefix(template[current.template:], hole)
	grantWildcard := current.grant < len(grant) && grant[current.grant] == '*'
	var next []state
	if grantWildcard {
		// The grant's `*` matches the empty string, or swallows whatever the
		// template emits next.
		next = append(next, state{current.template, current.grant + 1})
		if current.template < len(template) {
			advance := 1
			if atHole {
				advance = len(hole)
			}
			next = append(next, state{current.template + advance, current.grant})
		}
	}
	if atHole {
		// The hole may render to nothing, or to one more character that the
		// grant must be able to accept.
		next = append(next, state{current.template + len(hole), current.grant})
		if width, ok := grantAcceptsAnyCharacter(grant, current.grant); ok {
			next = append(next, state{current.template, current.grant + width})
		}
		return next
	}
	if current.template < len(template) {
		if width, ok := grantAccepts(grant, current.grant, template[current.template]); ok {
			next = append(next, state{current.template + 1, current.grant + width})
		}
	}
	return next
}

// grantAccepts reports whether the grant's next element matches one literal
// character the template emits, and how wide that element is.
func grantAccepts(grant string, index int, character byte) (int, bool) {
	if index >= len(grant) {
		return 0, false
	}
	switch {
	case grant[index] == '\\' && index+1 < len(grant):
		return 2, grant[index+1] == character
	case grant[index] == '?':
		return 1, true
	case grant[index] == '[':
		return matchClass(grant[index:], character)
	case grant[index] == '*':
		return 0, false // handled as a wildcard by the caller
	}
	return 1, grant[index] == character
}

// grantAcceptsAnyCharacter reports whether the grant's next element can accept
// at least one character — what a hole needs in order to render a character
// there at all. A class that matches nothing (there are none today, and a typo
// would make one) correctly answers no.
func grantAcceptsAnyCharacter(grant string, index int) (int, bool) {
	if index >= len(grant) || grant[index] == '*' {
		return 0, false
	}
	for character := byte(0x20); character < 0x7f; character++ {
		if width, ok := grantAccepts(grant, index, character); ok {
			return width, true
		}
	}
	return 0, false
}

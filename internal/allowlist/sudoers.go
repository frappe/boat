package allowlist

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Grant is one command pattern the allow-list authorises, remembered with the
// alias it came from so a failure can name the block an operator must edit.
type Grant struct {
	Alias   string
	Pattern string
}

// ParseSudoers reads a sudoers file and returns the command patterns the `boat`
// user may actually run.
//
// "Actually" is the point: a Cmnd_Alias that no user line references grants
// nothing, so an alias written and never wired is not a privilege — it is a
// belief. The file has held exactly that (eight unit-supervision grants that
// lived only in a scratch file while three places claimed they were pinned
// here), so the parser resolves the user line rather than trusting the aliases.
func ParseSudoers(path string) ([]Grant, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	aliases := map[string][]string{}
	var granted []string
	for _, line := range logicalLines(file) {
		switch {
		case strings.HasPrefix(line, "Cmnd_Alias "):
			name, patterns, found := strings.Cut(strings.TrimPrefix(line, "Cmnd_Alias "), "=")
			if !found {
				return nil, fmt.Errorf("Cmnd_Alias with no `=`: %s", line)
			}
			aliases[strings.TrimSpace(name)] = splitPatterns(patterns)
		case strings.HasPrefix(line, "boat ALL="):
			_, names, found := strings.Cut(line, ":")
			if !found {
				return nil, fmt.Errorf("user line with no `:`: %s", line)
			}
			granted = splitPatterns(names)
		}
	}

	var grants []Grant
	for _, name := range granted {
		patterns, ok := aliases[name]
		if !ok {
			return nil, fmt.Errorf("the boat user is granted %s, which no Cmnd_Alias defines", name)
		}
		for _, pattern := range patterns {
			grants = append(grants, Grant{Alias: name, Pattern: pattern})
		}
	}
	return grants, nil
}

// logicalLines drops comments and joins backslash continuations, in that order.
//
// The order is the whole subtlety, and it mirrors a trap that has already cost
// a rejected file: a comment written INSIDE a Cmnd_Alias breaks the
// continuation and visudo refuses the lot. Stripping comments first is what
// this parser can do that sudo cannot, so a stray comment fails `visudo -cf`
// (which the test also runs) rather than silently truncating an alias here.
func logicalLines(file *os.File) []string {
	var lines []string
	var pending strings.Builder
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		if strings.HasSuffix(line, "\\") {
			pending.WriteString(strings.TrimSpace(strings.TrimSuffix(line, "\\")))
			pending.WriteString(" ")
			continue
		}
		pending.WriteString(line)
		lines = append(lines, pending.String())
		pending.Reset()
	}
	if pending.Len() > 0 {
		lines = append(lines, strings.TrimSpace(pending.String()))
	}
	return lines
}

// splitPatterns splits a comma-separated list and normalises the whitespace
// that line continuations leave behind, so a pattern compares as the single
// spaced string sudo would see.
//
// The comma is both the list separator and a character that appears inside real
// commands — `lvs -o lv_name,lv_size` and `nft … tcp flags syn / fin,syn,rst,ack`
// are two of them — so the file escapes the inner ones and this splits only on
// the bare form. Splitting naively turns one grant into four fragments that
// match nothing, which reads as four dead grants and one missing one.
func splitPatterns(text string) []string {
	var patterns []string
	for _, part := range splitUnescaped(text, ',') {
		if field := strings.Join(strings.Fields(part), " "); field != "" {
			patterns = append(patterns, unescape(field))
		}
	}
	return patterns
}

func splitUnescaped(text string, separator byte) []string {
	var parts []string
	start := 0
	for index := 0; index < len(text); index++ {
		switch text[index] {
		case '\\':
			index++
		case separator:
			parts = append(parts, text[start:index])
			start = index + 1
		}
	}
	return append(parts, text[start:])
}

// unescape removes the backslashes sudoers' own lexer would consume, leaving
// the text fnmatch is handed. Only the characters sudoers treats as special are
// unescaped; a backslash before anything else is left for the glob matcher,
// which has its own use for it.
func unescape(pattern string) string {
	var builder strings.Builder
	for index := 0; index < len(pattern); index++ {
		if pattern[index] == '\\' && index+1 < len(pattern) && strings.IndexByte(",:=() ", pattern[index+1]) >= 0 {
			index++
		}
		builder.WriteByte(pattern[index])
	}
	return builder.String()
}

package allowlist

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
)

// Template is one privileged command template as it appears in the source, kept
// with its position so a failure names the line to edit rather than the string.
type Template struct {
	File string
	Line int
	// Text is the literal template, holes and all: `sudo ip link del {}`.
	Text string
	// Shell is true for a run.Shell call, whose template is a script rather than
	// an argv — its metacharacters are honoured, so it may hold several commands.
	Shell bool
}

// runnerMethods maps each method that takes a command template to the index of
// that template in its argument list. Input takes stdin first, which is exactly
// the kind of off-by-one that would silently skip a privileged call site, so the
// index is data here rather than an assumption in the walker.
var runnerMethods = map[string]int{
	"Run": 1, "RunUnchecked": 1, "OK": 1, "Probe": 1, "Shell": 1, "Input": 2,
}

// Templates finds every command template Boat renders under the given
// directories, skipping tests — a test's fake runner reaches no host, and a
// template that only a test renders needs no grant.
//
// It reads the syntax rather than grepping for `"sudo `, because a grep answers
// with prose: comments in this repo discuss `sudo` constantly, and an
// error message that contains the word is not a command. Walking call sites
// also surfaces the templates that are NOT literals, which a grep cannot see at
// all — and an unreadable template is a hole in this check, so it is reported
// instead of quietly skipped.
func Templates(directories ...string) ([]Template, []Template, error) {
	var literals, dynamic []Template
	fileSet := token.NewFileSet()
	for _, directory := range directories {
		err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !isPortedSource(path) {
				return err
			}
			file, err := parser.ParseFile(fileSet, path, nil, 0)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			found, unreadable := templatesIn(fileSet, file)
			literals, dynamic = append(literals, found...), append(dynamic, unreadable...)
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	return literals, dynamic, nil
}

func isPortedSource(path string) bool {
	return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
}

// templatesIn reads one file twice over, because a template does not always sit
// at the call site that runs it.
//
// The first pass is the call sites, which is where Shell templates are told
// apart and where a template assembled at runtime is caught. The second is
// every remaining `sudo …` literal in the file, wherever it lives — a local
// assigned before the call (`template := "sudo lvextend …"`), a field in a table
// of commands (`listed{volumeGroup, "sudo vgs …"}`). Those reach a host exactly
// like the others, and a check that only read call arguments would have called
// their grants dead while the commands ran daily.
func templatesIn(fileSet *token.FileSet, file *ast.File) (literals []Template, dynamic []Template) {
	consumed := map[token.Pos]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		index, ok := runnerMethods[selector.Sel.Name]
		if !ok || index >= len(call.Args) {
			return true
		}
		template := at(fileSet, call.Args[index])
		template.Shell = selector.Sel.Name == "Shell"
		text, readable := templateText(call.Args[index], consumed)
		if !readable {
			// A name is a template that was written somewhere else: a local
			// assigned just above, a `step.template` field from a table of
			// commands, or — for the wrappers inside `run` itself — the caller's
			// own template being forwarded. All are read where they are written,
			// so reporting them here would flag correct code and train a reader
			// to skim this list.
			if !isName(call.Args[index]) {
				dynamic = append(dynamic, template)
			}
			return true
		}
		template.Text = text
		literals = append(literals, template)
		return true
	})
	for _, template := range sweep(fileSet, file, consumed) {
		literals = append(literals, template)
	}
	return literals, dynamic
}

// isName reports whether an expression is just a name — a variable or a field —
// rather than something computed at the call site.
func isName(expression ast.Expr) bool {
	switch node := expression.(type) {
	case *ast.Ident:
		return true
	case *ast.SelectorExpr:
		return isName(node.X)
	}
	return false
}

func at(fileSet *token.FileSet, expression ast.Expr) Template {
	position := fileSet.Position(expression.Pos())
	return Template{File: position.Filename, Line: position.Line}
}

// templateText reads a template argument, marking the literals it consumed so
// the sweep does not report them a second time.
//
// A concatenation with a non-literal part — `"sudo nft " + counterCommand(uuid)`
// — reads as a hole, which is the same thing a `{}` means: text this check
// cannot see. It is deliberately the weaker answer. The alternative is to
// evaluate the function, and a check that simulates the program to decide what
// it is allowed to run has stopped being a check.
func templateText(expression ast.Expr, consumed map[token.Pos]bool) (string, bool) {
	switch node := expression.(type) {
	case *ast.BasicLit:
		if node.Kind != token.STRING {
			return "", false
		}
		text, err := strconv.Unquote(node.Value)
		if err != nil {
			return "", false
		}
		consumed[node.Pos()] = true
		return normalise(text), true
	case *ast.BinaryExpr:
		if node.Op != token.ADD {
			return "", false
		}
		left, leftReadable := templateText(node.X, consumed)
		right, rightReadable := templateText(node.Y, consumed)
		if !leftReadable && !rightReadable {
			return "", false
		}
		if !leftReadable {
			left = hole
		}
		if !rightReadable {
			right = hole
		}
		return left + right, true
	}
	return "", false
}

// sweep collects the `sudo …` templates no call site accounted for: the
// concatenations first, so `"sudo nft " + counterCommand(uuid)` is read as one
// command rather than as the fragment `sudo nft `, and then the bare literals.
func sweep(fileSet *token.FileSet, file *ast.File, consumed map[token.Pos]bool) []Template {
	var found []Template
	ast.Inspect(file, func(node ast.Node) bool {
		concatenation, ok := node.(*ast.BinaryExpr)
		if !ok || concatenation.Op != token.ADD {
			return true
		}
		text, readable := templateText(concatenation, consumed)
		if !readable || !strings.HasPrefix(text, "sudo ") {
			return true
		}
		template := at(fileSet, concatenation)
		template.Text = text
		found = append(found, template)
		return true
	})
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING || consumed[literal.Pos()] {
			return true
		}
		text, err := strconv.Unquote(literal.Value)
		if err != nil || !strings.HasPrefix(text, "sudo ") {
			return true
		}
		template := at(fileSet, literal)
		template.Text = normalise(text)
		found = append(found, template)
		return true
	})
	return found
}

// PrivilegedCommands returns the `sudo …` command lines a template renders.
//
// Usually that is the template itself or nothing. A Shell template is the
// exception: its metacharacters are real, so `sudo cat {} | jq …` is one
// privileged command and one unprivileged one, and only the first needs a
// grant. Splitting on the separators is what keeps the pipeline out of the
// argument string that gets matched.
func PrivilegedCommands(template Template) []string {
	segments := []string{template.Text}
	if template.Shell {
		segments = splitPipeline(template.Text)
	}
	var commands []string
	for _, segment := range segments {
		if segment = strings.TrimSpace(segment); strings.HasPrefix(segment, "sudo ") {
			commands = append(commands, strings.TrimPrefix(segment, "sudo "))
		}
	}
	return commands
}

// normalise turns a format verb into the hole it is. Two ways of writing "a
// value goes here" reach the same host — `run`'s `{}` and a `fmt.Sprintf` verb,
// which is how `internal/networkd` builds its wg commands — and a check that
// only understood one of them would read `ip link add dev %s` as a device
// literally named `%s` and report a grant that works as missing.
func normalise(template string) string {
	replacer := strings.NewReplacer("%s", hole, "%d", hole, "%q", hole, "%v", hole, "%x", hole)
	return replacer.Replace(template)
}

func splitPipeline(script string) []string {
	replacer := strings.NewReplacer("&&", "\n", "||", "\n", "|", "\n", ";", "\n", ">", "\n")
	return strings.Split(replacer.Replace(script), "\n")
}

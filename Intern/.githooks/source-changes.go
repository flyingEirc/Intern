// Command source-changes distinguishes Go logic/directive changes from ordinary comments.
package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"reflect"
	"strings"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "source-changes: project directory required")
		os.Exit(2)
	}
	changed, err := businessChanged(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "source-changes:", err)
		os.Exit(2)
	}
	if changed {
		fmt.Println("changed")
	} else {
		fmt.Println("unchanged")
	}
}
func git(project string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = project
	return cmd.Output()
}
func businessChanged(project string) (bool, error) {
	files, err := git(project, "diff", "--cached", "--name-only", "-z", "--", "internal", "app")
	if err != nil {
		return false, err
	}
	for _, path := range strings.Split(string(files), "\x00") {
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			continue
		}
		before, beforeErr := git(project, "show", "HEAD:"+path)
		after, afterErr := git(project, "show", ":"+path)
		// New/deleted files and unreadable or invalid source fail closed into the checks.
		if beforeErr != nil || afterErr != nil {
			return true, nil
		}
		oldAST, oldErr := semanticSource(before)
		newAST, newErr := semanticSource(after)
		if oldErr != nil || newErr != nil || !bytes.Equal(oldAST, newAST) {
			return true, nil
		}
	}
	return false, nil
}
func semanticSource(source []byte) ([]byte, error) {
	tree, err := parser.ParseFile(token.NewFileSet(), "", source, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	for _, imp := range tree.Imports {
		if imp.Path.Value == `"C"` {
			return source, nil
		}
	}
	var normalized bytes.Buffer
	// Compiler directives are executable input even though Go spells them as comments.
	for _, group := range tree.Comments {
		for _, comment := range group.List {
			if directive(comment.Text) {
				normalized.WriteString(comment.Text)
				normalized.WriteByte('\n')
			}
		}
	}
	position := reflect.TypeFor[token.Pos]()
	err = ast.Fprint(&normalized, nil, tree, func(name string, value reflect.Value) bool {
		return value.Type() != position && name != "Doc" && name != "Comment" && name != "Comments"
	})
	return normalized.Bytes(), err
}
func directive(text string) bool {
	for _, prefix := range []string{"//go:", "// +build", "//line ", "/*line "} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

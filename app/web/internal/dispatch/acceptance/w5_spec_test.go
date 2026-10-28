//go:build integration

package acceptance

import (
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/flyingEirc/Intern/app/web/api"
)

// The worker is the API's only client here, so a parameter name the embedded
// OpenAPI does not declare is a test bug with no visible symptom: the handler
// ignores it and the call means something else than the test believes. These
// guards read the same document the server's contract tests read.

var w5Spec struct {
	once sync.Once
	doc  *openapi3.T
	err  error
}

// w5SpecDoc loads the embedded OpenAPI document once.
func w5SpecDoc(t *testing.T) *openapi3.T {
	t.Helper()
	w5Spec.once.Do(func() {
		w5Spec.doc, w5Spec.err = openapi3.NewLoader().LoadFromData(api.OpenAPI)
	})
	if w5Spec.err != nil {
		t.Fatalf("the embedded OpenAPI document does not load: %v", w5Spec.err)
	}
	return w5Spec.doc
}

// w5RequireDeclaredQuery fails when a request carries a query parameter the
// OpenAPI does not declare for that method and path.
func w5RequireDeclaredQuery(t *testing.T, method, path string, query url.Values) {
	t.Helper()
	declared := w5DeclaredParameters(t, method, path)["query"]
	for name := range query {
		if !declared[name] {
			t.Fatalf("%s %s sends query parameter %q, which the OpenAPI does not declare (declared: %s)",
				method, path, name, strings.Join(sortedNames(declared), ","))
		}
	}
}

// w5RequireDeclaredHeader fails when a request carries a header parameter the
// OpenAPI does not declare for that method and path.
func w5RequireDeclaredHeader(t *testing.T, method, path, header string) {
	t.Helper()
	if !w5DeclaredParameters(t, method, path)["header"][header] {
		t.Fatalf("%s %s sends header %q, which the OpenAPI does not declare", method, path, header)
	}
}

// w5DeclaredParameters returns the declared parameter names of one operation,
// grouped by location ("query", "header", ...). Path-level parameters count too,
// and $ref parameters are resolved through the components section.
func w5DeclaredParameters(t *testing.T, method, path string) map[string]map[string]bool {
	t.Helper()
	doc := w5SpecDoc(t)
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for template, item := range doc.Paths.Map() {
		if !w5PathMatches(strings.Split(strings.TrimPrefix(template, "/"), "/"), segments) {
			continue
		}
		if operation := item.GetOperation(strings.ToUpper(method)); operation != nil {
			return w5OperationParameters(doc, item, operation)
		}
	}
	t.Fatalf("the OpenAPI declares no %s operation for %s", method, path)
	return nil
}

// w5OperationParameters collects the declared parameters of one operation at
// both the path and the operation level.
func w5OperationParameters(doc *openapi3.T, item *openapi3.PathItem,
	operation *openapi3.Operation,
) map[string]map[string]bool {
	declared := map[string]map[string]bool{}
	for _, references := range [][]*openapi3.ParameterRef{item.Parameters, operation.Parameters} {
		for _, reference := range references {
			if parameter := w5ResolveParameter(doc, reference); parameter != nil {
				if declared[parameter.In] == nil {
					declared[parameter.In] = map[string]bool{}
				}
				declared[parameter.In][parameter.Name] = true
			}
		}
	}
	return declared
}

// w5ResolveParameter follows a component reference and reports the parameter it
// names, or nil when the reference cannot be resolved.
func w5ResolveParameter(doc *openapi3.T, reference *openapi3.ParameterRef) *openapi3.Parameter {
	if reference.Ref == "" {
		return reference.Value
	}
	component, ok := doc.Components.Parameters[strings.TrimPrefix(reference.Ref, "#/components/parameters/")]
	if !ok {
		return nil
	}
	return component.Value
}

// w5PathMatches reports whether one concrete path matches a template path, where
// a {name} segment matches any one segment.
func w5PathMatches(template, concrete []string) bool {
	if len(template) != len(concrete) {
		return false
	}
	for index, segment := range template {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			if concrete[index] == "" {
				return false
			}
			continue
		}
		if segment != concrete[index] {
			return false
		}
	}
	return true
}

func sortedNames(names map[string]bool) []string {
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

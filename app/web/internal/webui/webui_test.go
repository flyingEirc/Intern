package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// export mirrors the layout `next build` emits with output: "export": a page
// may exist both as account.html and as an account/ directory of RSC payloads.
func export() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                      {Data: []byte("<html>overview</html>")},
		"login.html":                      {Data: []byte("<html>login</html>")},
		"login.txt":                       {Data: []byte("rsc login")},
		"account.html":                    {Data: []byte("<html>account</html>")},
		"account/__next._tree.txt":        {Data: []byte("rsc tree")},
		"404.html":                        {Data: []byte("<html>not found</html>")},
		"_next/static/chunks/app.js":      {Data: []byte("console.log(1)")},
		"_next/static/chunks/app.css":     {Data: []byte("body{}")},
		"_next/static/media/favicon.ico":  {Data: []byte{0, 0, 1, 0}},
		"_next/static/build/_ssgManifest": {Data: []byte("x")},
	}
}

type response struct {
	status  int
	body    string
	headers http.Header
}

func do(t *testing.T, h http.Handler, method, target string) response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{status: rec.Code, body: string(body), headers: rec.Result().Header}
}

func TestHandlerResolvesExportedPages(t *testing.T) {
	h := Handler(export())
	cases := []struct {
		target, body, contentType string
	}{
		{"/", "<html>overview</html>", "text/html; charset=utf-8"},
		{"/login", "<html>login</html>", "text/html; charset=utf-8"},
		// The directory of the same name must not shadow account.html.
		{"/account", "<html>account</html>", "text/html; charset=utf-8"},
		{"/login.txt", "rsc login", "text/plain; charset=utf-8"},
		{"/account/__next._tree.txt", "rsc tree", "text/plain; charset=utf-8"},
		{"/_next/static/chunks/app.js", "console.log(1)", "text/javascript; charset=utf-8"},
		{"/_next/static/chunks/app.css", "body{}", "text/css; charset=utf-8"},
	}
	for _, tc := range cases {
		got := do(t, h, http.MethodGet, tc.target)
		if got.status != http.StatusOK || got.body != tc.body {
			t.Errorf("GET %s = %d %q, want 200 %q", tc.target, got.status, got.body, tc.body)
		}
		if ct := got.headers.Get("Content-Type"); ct != tc.contentType {
			t.Errorf("GET %s Content-Type = %q, want %q", tc.target, ct, tc.contentType)
		}
		if got.headers.Get("X-Content-Type-Options") != "nosniff" || got.headers.Get("X-Frame-Options") != "DENY" {
			t.Errorf("GET %s lacks the security headers: %v", tc.target, got.headers)
		}
		if got.headers.Get("Content-Security-Policy") != contentSecurityPolicy {
			t.Errorf("GET %s Content-Security-Policy = %q", tc.target, got.headers.Get("Content-Security-Policy"))
		}
	}
}

func TestHandlerCachesOnlyHashedStaticAssetsForever(t *testing.T) {
	h := Handler(export())
	for target, want := range map[string]string{
		"/_next/static/chunks/app.js":     immutableCache,
		"/_next/static/media/favicon.ico": immutableCache,
		"/":                               revalidate,
		"/login":                          revalidate,
		"/login.txt":                      revalidate,
		"/missing":                        revalidate,
	} {
		if got := do(t, h, http.MethodGet, target).headers.Get("Cache-Control"); got != want {
			t.Errorf("GET %s Cache-Control = %q, want %q", target, got, want)
		}
	}
}

func TestHandlerAnswersUnknownPathsWithTheExport404(t *testing.T) {
	h := Handler(export())
	for _, target := range []string{"/missing", "/_next/static/chunks/gone.js", "/../../etc/passwd", "/account/"} {
		got := do(t, h, http.MethodGet, target)
		if target == "/account/" {
			// Cleaned to /account, which exists.
			if got.status != http.StatusOK {
				t.Errorf("GET %s = %d, want 200", target, got.status)
			}
			continue
		}
		if got.status != http.StatusNotFound || got.body != "<html>not found</html>" {
			t.Errorf("GET %s = %d %q, want 404 with 404.html", target, got.status, got.body)
		}
	}
	head := do(t, h, http.MethodHead, "/missing")
	if head.status != http.StatusNotFound || head.body != "" {
		t.Errorf("HEAD /missing = %d %q, want 404 without a body", head.status, head.body)
	}
}

func TestHandlerServesHeadWithoutBodyAndRejectsOtherMethods(t *testing.T) {
	h := Handler(export())
	head := do(t, h, http.MethodHead, "/login")
	if head.status != http.StatusOK || head.body != "" || head.headers.Get("Content-Length") != "18" {
		t.Errorf("HEAD /login = %d %q length %q, want 200, no body, length 18", head.status, head.body, head.headers.Get("Content-Length"))
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		got := do(t, h, method, "/login")
		if got.status != http.StatusMethodNotAllowed || got.headers.Get("Allow") != "GET, HEAD" {
			t.Errorf("%s /login = %d Allow %q, want 405 Allow GET, HEAD", method, got.status, got.headers.Get("Allow"))
		}
		if got.headers.Get("Content-Security-Policy") != contentSecurityPolicy || got.headers.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s /login refusal lacks the security headers: %v", method, got.headers)
		}
	}
}

func TestMountKeepsTheAPISurfaceOnTheAPIHandler(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "api "+r.URL.Path) //nolint:errcheck // test handler
	})
	h := Mount(api, export())
	for _, target := range []string{"/api", "/api/v1/auth/session", "/api/v1/does-not-exist", "/healthz", "/readyz"} {
		if got := do(t, h, http.MethodGet, target); got.status != http.StatusTeapot || got.body != "api "+target {
			t.Errorf("GET %s reached the UI (%d %q), want the API handler", target, got.status, got.body)
		}
	}
	// POST to the API still reaches it; prefixes that only look similar do not.
	if got := do(t, h, http.MethodPost, "/api/v1/auth/login"); got.status != http.StatusTeapot {
		t.Errorf("POST /api/v1/auth/login = %d, want the API handler", got.status)
	}
	for _, target := range []string{"/apis", "/healthz.html", "/login"} {
		if got := do(t, h, http.MethodGet, target); got.status == http.StatusTeapot {
			t.Errorf("GET %s reached the API handler, want the UI", target)
		}
	}
}

func TestMountWithoutAnExportIsTheAPIHandler(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := Mount(api, nil)
	for _, target := range []string{"/", "/login", "/api/v1/auth/session"} {
		if got := do(t, h, http.MethodGet, target); got.status != http.StatusTeapot {
			t.Errorf("GET %s = %d, want every path on the API handler", target, got.status)
		}
	}
}

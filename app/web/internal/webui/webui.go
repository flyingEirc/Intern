// Package webui serves the static export of app/web/frontend from the same
// origin as the API. The export is embedded only in builds with the webui tag;
// without it Assets returns nil and Mount leaves the API handler untouched, so
// every path keeps the API's own routing and JSON 404.
package webui

import (
	"bytes"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

const (
	immutableCache = "public, max-age=31536000, immutable"
	revalidate     = "no-cache"
	// Next.js inlines its bootstrap and RSC payload scripts and styles, so
	// script-src and style-src need 'unsafe-inline'. Everything else is limited
	// to this origin; framing, plugins and base-URL rewrites are refused.
	contentSecurityPolicy = "default-src 'self'; script-src 'self' 'unsafe-inline'; " +
		"style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; " +
		"connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'; object-src 'none'"
	notFoundPage = "404.html"
)

// Mount routes the API surface (/api/..., /healthz, /readyz) to api and every
// other path to the exported UI. A nil files leaves api as the only handler.
func Mount(api http.Handler, files fs.FS) http.Handler {
	if files == nil {
		return api
	}
	ui := Handler(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAPIPath(r.URL.Path) {
			api.ServeHTTP(w, r)
			return
		}
		ui.ServeHTTP(w, r)
	})
}

func isAPIPath(p string) bool {
	return p == "/api" || strings.HasPrefix(p, "/api/") || p == "/healthz" || p == "/readyz"
}

// Handler serves a Next.js static export (trailingSlash false): /login is
// login.html, / is index.html, and files such as /_next/static/... or the RSC
// payloads /login.txt are served as they are. An unknown path answers 404 with
// the export's 404.html.
func Handler(files fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every UI answer carries the security headers, refusals included.
		setSecurityHeaders(w.Header())
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name, ok := resolve(files, r.URL.Path)
		if !ok {
			serveNotFound(w, r, files)
			return
		}
		if strings.HasPrefix(name, "_next/static/") {
			w.Header().Set("Cache-Control", immutableCache)
		} else {
			w.Header().Set("Cache-Control", revalidate)
		}
		if err := serveFile(w, r, files, name); err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
		}
	})
}

func setSecurityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
}

// resolve maps a request path to an exported file. path.Clean removes dot
// segments, so a name can never leave the export.
func resolve(files fs.FS, requestPath string) (string, bool) {
	name := strings.TrimPrefix(path.Clean("/"+requestPath), "/")
	if name == "" {
		return "index.html", isFile(files, "index.html")
	}
	for _, candidate := range []string{name, name + ".html", name + "/index.html"} {
		if isFile(files, candidate) {
			return candidate, true
		}
	}
	return "", false
}

func isFile(files fs.FS, name string) bool {
	info, err := fs.Stat(files, name)
	return err == nil && info.Mode().IsRegular()
}

// serveFile streams an exported file. embed.FS files implement io.ReadSeeker,
// so ServeContent answers HEAD and Range without buffering; any other fs.FS is
// read into memory once. Only failures before the response starts are returned.
func serveFile(w http.ResponseWriter, r *http.Request, files fs.FS, name string) error {
	file, err := files.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }() //nolint:errcheck // read-only handle, response already decided
	content, ok := file.(io.ReadSeeker)
	if !ok {
		data, readErr := io.ReadAll(file)
		if readErr != nil {
			return readErr
		}
		content = bytes.NewReader(data)
	}
	if contentType := contentTypeOf(name); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	// The export carries no modification times; Cache-Control alone governs reuse.
	http.ServeContent(w, r, name, time.Time{}, content)
	return nil
}

// exportTypes pins the types of every extension a Next.js export emits.
// mime.TypeByExtension alone would let the host's MIME database decide, and
// some Linux distributions map .js to application/x-javascript.
var exportTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".txt":         "text/plain; charset=utf-8",
	".json":        "application/json",
	".ico":         "image/x-icon",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".woff2":       "font/woff2",
	".webmanifest": "application/manifest+json",
}

func contentTypeOf(name string) string {
	ext := path.Ext(name)
	if contentType, ok := exportTypes[ext]; ok {
		return contentType
	}
	return mime.TypeByExtension(ext)
}

func serveNotFound(w http.ResponseWriter, r *http.Request, files fs.FS) {
	w.Header().Set("Cache-Control", revalidate)
	body, err := fs.ReadFile(files, notFoundPage)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(body); err != nil {
		return // The status is already sent; a failed body write means the client left.
	}
}

//go:build webui

package webui

import (
	"embed"
	"io/fs"
)

// dist is a copy of app/web/frontend/out made by `mise run frontend:build`
// (and by the image's frontend stage). It is not committed; a webui build
// without it fails to compile instead of shipping an empty UI.
//
//go:embed all:dist
var dist embed.FS

// Assets returns the embedded static export rooted at its index.html.
func Assets() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic("webui: embedded export has no dist root: " + err.Error())
	}
	return sub
}

//go:build !webui

package webui

import "io/fs"

// Assets is nil without the webui build tag: the binary serves only the API.
func Assets() fs.FS { return nil }

// Package api publishes the versioned HTTP contract used by contract tests and clients.
// Contract validation uses kin-openapi. Server code is deliberately not generated:
// oapi-codegen's Gin server output uses encoding/json rather than encoding/json/v2.
package api

import _ "embed"

//go:embed openapi.yaml
var OpenAPI []byte

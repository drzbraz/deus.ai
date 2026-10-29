// Package api holds the OpenAPI contract of the REST API. It is embedded so
// the running service can serve it (GET /openapi.yaml, GET /docs).
package api

import _ "embed"

//go:embed openapi.yaml
var Spec []byte

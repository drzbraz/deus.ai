package httpapi

import (
	"net/http"

	"github.com/drzbraz/deus.ai/api"
)

// swaggerUIVersion pins the Swagger UI assets loaded by /docs.
const swaggerUIVersion = "5.17.14"

const swaggerPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Logistics API</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@` + swaggerUIVersion + `/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@` + swaggerUIVersion + `/swagger-ui-bundle.js" crossorigin></script>
  <script>
    window.ui = SwaggerUIBundle({ url: "/openapi.yaml", dom_id: "#swagger-ui", tryItOutEnabled: true });
  </script>
</body>
</html>`

// serveSpec returns the OpenAPI contract embedded in the binary, so the
// documentation always matches the running version.
func serveSpec(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(api.Spec)
}

// serveDocs renders Swagger UI on top of /openapi.yaml.
func serveDocs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(swaggerPage))
}

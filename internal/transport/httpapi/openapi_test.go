package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/drzbraz/deus.ai/api"
)

// undocumentedRoutes serve the documentation itself.
var undocumentedRoutes = map[string]bool{"GET /openapi.yaml": true, "GET /docs": true}

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData(api.Spec)
	require.NoError(t, err)
	return doc
}

func TestOpenAPISpec_IsValid(t *testing.T) {
	require.NoError(t, loadSpec(t).Validate(context.Background()))
}

// Regression: the spec and the router must never drift apart. Every route is
// documented and every documented operation exists.
func TestOpenAPISpec_MatchesRegisteredRoutes(t *testing.T) {
	documented := map[string]bool{}
	for path, item := range loadSpec(t).Paths.Map() {
		for method := range item.Operations() {
			documented[method+" "+path] = true
		}
	}

	registered := map[string]bool{}
	err := chi.Walk(routes(Dependencies{Logger: zap.NewNop()}), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := method + " " + strings.TrimSuffix(route, "/")
		if !undocumentedRoutes[key] {
			registered[key] = true
		}
		return nil
	})
	require.NoError(t, err)

	assert.Equal(t, sortedKeys(documented), sortedKeys(registered))
}

func TestDocsEndpoints(t *testing.T) {
	handler := NewRouter(Dependencies{Logger: zap.NewNop()})

	spec := httptest.NewRecorder()
	handler.ServeHTTP(spec, httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))
	assert.Equal(t, http.StatusOK, spec.Code)
	assert.Equal(t, "application/yaml", spec.Header().Get("Content-Type"))
	assert.Equal(t, api.Spec, spec.Body.Bytes())

	docs := httptest.NewRecorder()
	handler.ServeHTTP(docs, httptest.NewRequest(http.MethodGet, "/docs", nil))
	assert.Equal(t, http.StatusOK, docs.Code)
	assert.Contains(t, docs.Body.String(), "SwaggerUIBundle")
	assert.Contains(t, docs.Body.String(), `url: "/openapi.yaml"`)
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

package testkit

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"

	"github.com/drzbraz/deus.ai/api"
)

// The contract validator checks every response the suites receive against
// api/openapi.yaml: status code documented, body matches the schema (no
// extra or missing fields), declared content type. Requests are not
// validated because many tests send invalid input on purpose.

var (
	contractOnce   sync.Once
	contractRouter routers.Router
	contractErr    error
)

func loadContract() (routers.Router, error) {
	contractOnce.Do(func() {
		doc, err := openapi3.NewLoader().LoadFromData(api.Spec)
		if err != nil {
			contractErr = fmt.Errorf("load openapi spec: %w", err)
			return
		}
		if err := doc.Validate(context.Background()); err != nil {
			contractErr = fmt.Errorf("invalid openapi spec: %w", err)
			return
		}
		doc.Servers = nil // match on path only, whatever host the API runs on
		contractRouter, contractErr = gorillamux.NewRouter(doc)
	})
	return contractRouter, contractErr
}

// checkContract returns an error when resp violates the documented contract.
// Requests to undocumented paths or methods (404/405 tests) are skipped.
func checkContract(req *http.Request, resp Response) error {
	router, err := loadContract()
	if err != nil {
		return err
	}
	route, pathParams, err := router.FindRoute(req)
	if err != nil {
		return nil //nolint:nilerr // not a documented operation: nothing to check
	}

	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request:    req,
			PathParams: pathParams,
			Route:      route,
			Options:    &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		},
		Status:  resp.Status,
		Header:  resp.Header,
		Body:    io.NopCloser(bytes.NewReader(resp.Body)),
		Options: &openapi3filter.Options{IncludeResponseStatus: true, MultiError: true},
	}
	if err := openapi3filter.ValidateResponse(context.Background(), input); err != nil {
		return fmt.Errorf("contract violation for %s %s -> %d: %w\nbody: %s", req.Method, req.URL.Path, resp.Status, err, resp.Body)
	}
	return nil
}

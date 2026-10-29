// Package httpapi is the REST transport: routing, JSON encoding, input
// validation at the edge and translation of domain errors to HTTP.
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"go.uber.org/zap"

	"github.com/drzbraz/deus.ai/internal/domain"
	"github.com/drzbraz/deus.ai/internal/logger"
)

// retryAfterSeconds is sent with 503 responses so well-behaved clients back off.
const retryAfterSeconds = "5"

// successBody is the envelope of every successful response.
type successBody struct {
	Data any       `json:"data"`
	Meta *pageMeta `json:"meta,omitempty"`
}

type pageMeta struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Count  int `json:"count"`
}

// errorBody is the envelope of every error response.
type errorBody struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details,omitempty"`
}

// Transport-level errors for malformed requests, before they reach a service.
var (
	errBadRequest           = errors.New("bad request")
	errUnsupportedMediaType = errors.New("unsupported media type")
	errPayloadTooLarge      = errors.New("payload too large")
	errRouteNotFound        = errors.New("route not found")
	errMethodNotAllowed     = errors.New("method not allowed")
)

// errorMapping maps one error kind to its HTTP representation. When
// exposeCause is true the error text is safe and useful for the client
// (e.g. "cannot move from pending to delivered"); otherwise a fixed message
// is used so internals such as SQL never leak.
type errorMapping struct {
	target      error
	status      int
	code        string
	message     string
	exposeCause bool
}

var errorMappings = []errorMapping{
	{errBadRequest, http.StatusBadRequest, "bad_request", "", true},
	{errUnsupportedMediaType, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json", false},
	{errPayloadTooLarge, http.StatusRequestEntityTooLarge, "payload_too_large", "request body is too large", false},
	{errRouteNotFound, http.StatusNotFound, "route_not_found", "no route matches this path", false},
	{errMethodNotAllowed, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed on this route", false},
	{domain.ErrValidation, http.StatusUnprocessableEntity, "validation_failed", "request validation failed", false},
	{domain.ErrNotFound, http.StatusNotFound, "not_found", "resource not found", false},
	{domain.ErrAlreadyExists, http.StatusConflict, "already_exists", "a resource with the same unique fields already exists", false},
	{domain.ErrInvalidTransition, http.StatusConflict, "invalid_status_transition", "", true},
	{domain.ErrCapacityExceeded, http.StatusConflict, "vessel_capacity_exceeded", "", true},
	{domain.ErrCargoDelivered, http.StatusConflict, "cargo_already_delivered", "", true},
	{domain.ErrVersionMismatch, http.StatusPreconditionFailed, "version_mismatch", "", true},
	{domain.ErrIdempotencyKeyReuse, http.StatusUnprocessableEntity, "idempotency_key_reused", "this Idempotency-Key was already used with a different request", false},
	{domain.ErrUnavailable, http.StatusServiceUnavailable, "service_unavailable", "a dependency is temporarily unavailable, retry later", false},
}

var internalError = errorMapping{nil, http.StatusInternalServerError, "internal_error", "internal server error", false}

func mapError(err error) errorMapping {
	for _, m := range errorMappings {
		if errors.Is(err, m.target) {
			return m
		}
	}
	return internalError
}

// responder writes JSON responses and is the single place where request
// errors are logged, so each failure produces exactly one log line.
type responder struct {
	log *zap.Logger
}

func (rs responder) ok(w http.ResponseWriter, status int, data any) {
	rs.write(w, status, successBody{Data: data})
}

func (rs responder) page(w http.ResponseWriter, data any, page domain.Page, count int) {
	rs.write(w, http.StatusOK, successBody{
		Data: data,
		Meta: &pageMeta{Limit: page.Limit, Offset: page.Offset, Count: count},
	})
}

func (rs responder) error(w http.ResponseWriter, r *http.Request, err error) {
	mapping := mapError(err)
	body := apiError{Code: mapping.code, Message: mapping.message}
	if mapping.exposeCause {
		body.Message = err.Error()
	}

	var verr *domain.ValidationError
	if errors.As(err, &verr) {
		body.Details = verr.Fields
	}

	log := logger.WithContext(r.Context(), rs.log)
	switch mapping.status {
	case http.StatusInternalServerError:
		log.Error("request failed", zap.Error(err))
	case http.StatusServiceUnavailable:
		w.Header().Set("Retry-After", retryAfterSeconds)
		log.Warn("dependency unavailable", zap.Error(err))
	}

	rs.write(w, mapping.status, errorBody{Error: body})
}

func (rs responder) write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		rs.log.Warn("write response body", zap.Error(err))
	}
}

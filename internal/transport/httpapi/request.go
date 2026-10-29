package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/drzbraz/deus.ai/internal/domain"
)

// maxBodyBytes bounds memory used per request; our payloads are tiny.
const maxBodyBytes = 1 << 20

func badRequest(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errBadRequest, fmt.Sprintf(format, args...))
}

// decodeJSON reads exactly one JSON object into dst and rejects anything
// ambiguous: wrong content type, unknown fields, trailing data, oversize body.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if err := requireJSON(r); err != nil {
		return err
	}

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return describeDecodeError(err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return badRequest("body must contain a single JSON object")
	}
	return nil
}

func requireJSON(r *http.Request) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errUnsupportedMediaType
	}
	return nil
}

func describeDecodeError(err error) error {
	var (
		syntaxErr  *json.SyntaxError
		typeErr    *json.UnmarshalTypeError
		maxSizeErr *http.MaxBytesError
	)
	switch {
	case errors.Is(err, io.EOF):
		return badRequest("request body is empty")
	case errors.As(err, &maxSizeErr):
		return errPayloadTooLarge
	case errors.As(err, &syntaxErr):
		return badRequest("malformed JSON at position %d", syntaxErr.Offset)
	case errors.Is(err, io.ErrUnexpectedEOF):
		return badRequest("malformed JSON")
	case errors.As(err, &typeErr):
		return badRequest("field %q must be of type %s", typeErr.Field, typeErr.Type)
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return badRequest("unknown field %s", strings.TrimPrefix(err.Error(), "json: unknown field "))
	default:
		return badRequest("invalid JSON body")
	}
}

func pathUUID(r *http.Request, param string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, param))
	if err != nil {
		return uuid.Nil, badRequest("%s must be a valid UUID", param)
	}
	return id, nil
}

// bodyUUID parses an id sent in a JSON body. A bad value is a validation
// problem of that field, not a malformed request.
func bodyUUID(field, raw string) (uuid.UUID, error) {
	if raw == "" {
		return uuid.Nil, domain.NewValidationError(field, "is required")
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, domain.NewValidationError(field, "must be a valid UUID")
	}
	return id, nil
}

func queryPage(r *http.Request) (domain.Page, error) {
	limit, err := queryInt(r, "limit")
	if err != nil {
		return domain.Page{}, err
	}
	offset, err := queryInt(r, "offset")
	if err != nil {
		return domain.Page{}, err
	}
	return domain.NewPage(limit, offset)
}

func queryInt(r *http.Request, name string) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, badRequest("query parameter %s must be an integer", name)
	}
	return value, nil
}

// ifMatchVersion reads an optional If-Match header holding a cargo version
// as produced by our ETag ("3"). A missing header or "*" disables the check.
func ifMatchVersion(r *http.Request) (int, error) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" || raw == "*" {
		return 0, nil
	}
	version, err := strconv.Atoi(strings.Trim(strings.TrimPrefix(raw, "W/"), `"`))
	if err != nil || version < 1 {
		return 0, badRequest("If-Match must be an ETag returned by this API")
	}
	return version, nil
}

func etag(version int) string {
	return strconv.Quote(strconv.Itoa(version))
}

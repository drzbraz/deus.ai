package testkit

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// Client is a minimal JSON client for the API under test.
type Client struct {
	t       testing.TB
	BaseURL string
	http    *http.Client
}

func NewClient(t testing.TB, baseURL string) *Client {
	return &Client{t: t, BaseURL: baseURL, http: &http.Client{Timeout: 30 * time.Second}}
}

// Response is a fully read HTTP response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r Response) String() string { return string(r.Body) }

// Option customises one request.
type Option func(*http.Request)

func WithHeader(name, value string) Option {
	return func(r *http.Request) { r.Header.Set(name, value) }
}

func WithContentType(contentType string) Option {
	return WithHeader("Content-Type", contentType)
}

func WithoutContentType() Option {
	return func(r *http.Request) { r.Header.Del("Content-Type") }
}

// IdempotencyKey sets the Idempotency-Key header.
func IdempotencyKey(key string) Option { return WithHeader("Idempotency-Key", key) }

// IfMatch sets the If-Match header.
func IfMatch(etag string) Option { return WithHeader("If-Match", etag) }

// Do sends a request. body may be nil, a raw string/[]byte, or any value
// that is encoded as JSON. It fails the test only on transport errors.
func (c *Client) Do(method, path string, body any, opts ...Option) Response {
	c.t.Helper()
	resp, err := c.TryDo(method, path, body, opts...)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// TryDo is Do without failing the test, for tests that expect transport
// errors (e.g. during shutdown) or run in goroutines.
func (c *Client) TryDo(method, path string, body any, opts ...Option) (Response, error) {
	reader, err := encodeBody(body)
	if err != nil {
		return Response{}, err
	}
	req, err := http.NewRequest(method, c.BaseURL+path, reader)
	if err != nil {
		return Response{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, opt := range opts {
		opt(req)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, err
	}

	result := Response{Status: resp.StatusCode, Header: resp.Header, Body: raw}
	if err := checkContract(req, result); err != nil {
		c.t.Error(err) // Error (not Fatal) is safe from the goroutines of concurrency tests
	}
	return result, nil
}

func encodeBody(body any) (io.Reader, error) {
	switch b := body.(type) {
	case nil:
		return nil, nil
	case string:
		return bytes.NewBufferString(b), nil
	case []byte:
		return bytes.NewReader(b), nil
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(raw), nil
	}
}

func (c *Client) Get(path string, opts ...Option) Response {
	c.t.Helper()
	return c.Do(http.MethodGet, path, nil, opts...)
}

func (c *Client) Post(path string, body any, opts ...Option) Response {
	c.t.Helper()
	return c.Do(http.MethodPost, path, body, opts...)
}

func (c *Client) Patch(path string, body any, opts ...Option) Response {
	c.t.Helper()
	return c.Do(http.MethodPatch, path, body, opts...)
}

// Data decodes the "data" member of a success envelope.
func Data[T any](t testing.TB, r Response) T {
	t.Helper()
	var envelope struct {
		Data *T `json:"data"`
	}
	if err := json.Unmarshal(r.Body, &envelope); err != nil || envelope.Data == nil {
		t.Fatalf("response is not a data envelope (status %d): %s", r.Status, r.Body)
	}
	return *envelope.Data
}

// Meta is the pagination block of list responses.
type Meta struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Count  int `json:"count"`
}

func MetaOf(t testing.TB, r Response) Meta {
	t.Helper()
	var envelope struct {
		Meta *Meta `json:"meta"`
	}
	if err := json.Unmarshal(r.Body, &envelope); err != nil || envelope.Meta == nil {
		t.Fatalf("response has no meta block: %s", r.Body)
	}
	return *envelope.Meta
}

// APIError is the error envelope of every failed request.
type APIError struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details"`
}

func ErrorOf(t testing.TB, r Response) APIError {
	t.Helper()
	var envelope struct {
		Error *APIError `json:"error"`
	}
	if err := json.Unmarshal(r.Body, &envelope); err != nil || envelope.Error == nil {
		t.Fatalf("response is not an error envelope (status %d): %s", r.Status, r.Body)
	}
	return *envelope.Error
}

package testkit

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The contract check must really reject drift, otherwise every e2e test
// would pass vacuously.
func TestCheckContract(t *testing.T) {
	cargoPath := "/api/v1/cargoes/0192d6c0-0000-7000-8000-000000000002"
	jsonHeader := http.Header{"Content-Type": []string{"application/json"}}
	valid := `{"data":{"id":"0192d6c0-0000-7000-8000-000000000002","name":"x","description":"","weight_kg":1,
		"status":"pending","vessel_id":"0192d6c0-0000-7000-8000-000000000001","version":1,
		"created_at":"2026-10-29T12:00:00Z","updated_at":"2026-10-29T12:00:00Z"}}`

	tests := []struct {
		name    string
		path    string
		status  int
		body    string
		wantErr bool
	}{
		{"valid cargo", cargoPath, 200, valid, false},
		{"undocumented path is skipped", "/nope", 404, `{}`, false},
		{"extra field", cargoPath, 200, valid[:len(valid)-2] + `,"secret":1}}`, true},
		{"missing field", cargoPath, 200, `{"data":{"id":"0192d6c0-0000-7000-8000-000000000002"}}`, true},
		{"unknown status value", cargoPath, 200, strings.Replace(valid, `"pending"`, `"lost"`, 1), true},
		{"undocumented status code", cargoPath, 418, `{"error":{"code":"internal_error","message":"x"}}`, true},
		{"unknown error code", cargoPath, 404, `{"error":{"code":"oops","message":"x"}}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			err := checkContract(req, Response{Status: tt.status, Header: jsonHeader, Body: []byte(tt.body)})
			assert.Equal(t, tt.wantErr, err != nil, "%v", err)
		})
	}
}

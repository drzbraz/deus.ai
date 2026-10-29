//go:build e2e

package e2e

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/drzbraz/deus.ai/tests/testkit"
)

// extractData returns the raw JSON of the "data" member.
func extractData(t *testing.T, r testkit.Response) string {
	t.Helper()
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(r.Body, &envelope))
	return string(envelope["data"])
}

func statuses(entries []testkit.TrackingEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Status)
	}
	return out
}

func locations(entries []testkit.TrackingEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Location)
	}
	return out
}

//go:build e2e

// Package e2e contains black-box acceptance tests: every API flow and edge
// case, exercised over HTTP exactly as a client would.
package e2e

import (
	"os"
	"testing"

	"github.com/drzbraz/deus.ai/tests/testkit"
)

var stack *testkit.Stack

func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, &stack))
}

func api(t *testing.T) *testkit.Client {
	return stack.Client(t)
}

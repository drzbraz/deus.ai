package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drzbraz/deus.ai/internal/domain"
)

func TestNewPage(t *testing.T) {
	page, err := domain.NewPage(0, 0)
	require.NoError(t, err)
	assert.Equal(t, domain.Page{Limit: domain.DefaultPageLimit}, page)

	page, err = domain.NewPage(100, 40)
	require.NoError(t, err)
	assert.Equal(t, domain.Page{Limit: 100, Offset: 40}, page)

	for _, tc := range [][2]int{{-1, 0}, {101, 0}, {10, -1}} {
		_, err := domain.NewPage(tc[0], tc[1])
		assert.ErrorIs(t, err, domain.ErrValidation, "limit=%d offset=%d", tc[0], tc[1])
	}
}

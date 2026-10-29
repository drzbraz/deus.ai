package domain

import "github.com/google/uuid"

const (
	DefaultPageLimit = 20
	MaxPageLimit     = 100
)

// Page is offset-based pagination. It is always normalised before reaching storage.
type Page struct {
	Limit  int
	Offset int
}

// NewPage validates pagination input. A zero limit means "use the default".
func NewPage(limit, offset int) (Page, error) {
	var v validator
	v.check(limit >= 0 && limit <= MaxPageLimit, "limit", "must be between 1 and 100")
	v.check(offset >= 0, "offset", "must be 0 or greater")
	if err := v.err(); err != nil {
		return Page{}, err
	}

	if limit == 0 {
		limit = DefaultPageLimit
	}
	return Page{Limit: limit, Offset: offset}, nil
}

// CargoFilter narrows a cargo listing. Nil fields are not filtered on.
type CargoFilter struct {
	Status   *Status
	VesselID *uuid.UUID
	Page     Page
}

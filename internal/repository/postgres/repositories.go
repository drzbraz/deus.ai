package postgres

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/drzbraz/deus.ai/internal/service"
)

// NewRepositories wires every Postgres adapter to the service ports.
func NewRepositories(pool *pgxpool.Pool) service.Repositories {
	return service.Repositories{
		Tx:       NewTxManager(pool),
		Vessels:  NewVesselRepository(pool),
		Cargoes:  NewCargoRepository(pool),
		Tracking: NewTrackingRepository(pool),
		Outbox:   NewOutboxRepository(pool),
		Events:   NewCargoEventRepository(pool),
	}
}

package testkit

import (
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The types below mirror the public API contract. They are declared here,
// independently of the production DTOs, so a contract change breaks the suite.

type Vessel struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	CapacityKg          float64   `json:"capacity_kg"`
	CurrentLocation     string    `json:"current_location"`
	LoadKg              float64   `json:"load_kg"`
	AvailableCapacityKg float64   `json:"available_capacity_kg"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type Cargo struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	WeightKg    float64   `json:"weight_kg"`
	Status      string    `json:"status"`
	VesselID    string    `json:"vessel_id"`
	Version     int       `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type TrackingEntry struct {
	ID         string    `json:"id"`
	CargoID    string    `json:"cargo_id"`
	Status     string    `json:"status"`
	Location   string    `json:"location"`
	Note       string    `json:"note"`
	RecordedAt time.Time `json:"recorded_at"`
}

type CargoEvent struct {
	EventID    string    `json:"event_id"`
	CargoID    string    `json:"cargo_id"`
	OldStatus  string    `json:"old_status"`
	NewStatus  string    `json:"new_status"`
	OccurredAt time.Time `json:"occurred_at"`
	ReceivedAt time.Time `json:"received_at"`
}

const (
	Pending   = "pending"
	InTransit = "in_transit"
	Delivered = "delivered"
)

// UniqueName returns a name no other test uses, so tests never collide on
// the unique vessel name and never need to clean tables.
func UniqueName(prefix string) string {
	return prefix + "-" + uuid.NewString()
}

func VesselPath(id string) string { return "/api/v1/vessels/" + id }
func CargoPath(id string) string  { return "/api/v1/cargoes/" + id }

// NewVessel creates a vessel located in Rotterdam.
func (c *Client) NewVessel(capacityKg float64) Vessel {
	c.t.Helper()
	resp := c.Post("/api/v1/vessels", map[string]any{
		"name": UniqueName("vessel"), "capacity_kg": capacityKg, "current_location": "Rotterdam",
	})
	require.Equal(c.t, http.StatusCreated, resp.Status, resp.String())
	return Data[Vessel](c.t, resp)
}

func (c *Client) NewCargo(vesselID string, weightKg float64) Cargo {
	c.t.Helper()
	resp := c.Post("/api/v1/cargoes", map[string]any{
		"name": "Coffee beans", "weight_kg": weightKg, "vessel_id": vesselID,
	})
	require.Equal(c.t, http.StatusCreated, resp.Status, resp.String())
	return Data[Cargo](c.t, resp)
}

// NewCargoOnFreshVessel is a shortcut for tests that only care about the cargo.
func (c *Client) NewCargoOnFreshVessel() Cargo {
	c.t.Helper()
	return c.NewCargo(c.NewVessel(10_000).ID, 10)
}

// NewCargoInStatus creates a cargo and moves it to the requested status.
func (c *Client) NewCargoInStatus(status string) Cargo {
	c.t.Helper()
	cargo := c.NewCargoOnFreshVessel()
	switch status {
	case Pending:
	case InTransit:
		cargo = c.MustChangeStatus(cargo.ID, InTransit)
	case Delivered:
		c.MustChangeStatus(cargo.ID, InTransit)
		cargo = c.MustChangeStatus(cargo.ID, Delivered)
	default:
		c.t.Fatalf("unknown status %q", status)
	}
	return cargo
}

func (c *Client) ChangeStatus(cargoID, status string, opts ...Option) Response {
	c.t.Helper()
	return c.Patch(CargoPath(cargoID)+"/status", map[string]any{"status": status}, opts...)
}

func (c *Client) MustChangeStatus(cargoID, status string) Cargo {
	c.t.Helper()
	resp := c.ChangeStatus(cargoID, status)
	require.Equal(c.t, http.StatusOK, resp.Status, resp.String())
	return Data[Cargo](c.t, resp)
}

func (c *Client) GetCargo(cargoID string) Cargo {
	c.t.Helper()
	resp := c.Get(CargoPath(cargoID))
	require.Equal(c.t, http.StatusOK, resp.Status, resp.String())
	return Data[Cargo](c.t, resp)
}

func (c *Client) GetVessel(vesselID string) Vessel {
	c.t.Helper()
	resp := c.Get(VesselPath(vesselID))
	require.Equal(c.t, http.StatusOK, resp.Status, resp.String())
	return Data[Vessel](c.t, resp)
}

func (c *Client) History(cargoID string) []TrackingEntry {
	c.t.Helper()
	resp := c.Get(CargoPath(cargoID) + "/tracking")
	require.Equal(c.t, http.StatusOK, resp.Status, resp.String())
	return Data[[]TrackingEntry](c.t, resp)
}

func (c *Client) Events(cargoID string) []CargoEvent {
	c.t.Helper()
	resp := c.Get(CargoPath(cargoID) + "/events")
	require.Equal(c.t, http.StatusOK, resp.Status, resp.String())
	return Data[[]CargoEvent](c.t, resp)
}

// WaitForEvents waits until the consumer has stored exactly n events.
func (c *Client) WaitForEvents(cargoID string, n int) []CargoEvent {
	c.t.Helper()
	var events []CargoEvent
	require.Eventuallyf(c.t, func() bool {
		events = c.Events(cargoID)
		return len(events) == n
	}, 30*time.Second, 25*time.Millisecond, "expected %d events for cargo %s", n, cargoID)
	return events
}

// AssertEventCountStays checks the event count does not grow, i.e. nothing
// was emitted, giving the async pipeline time to deliver anything pending.
func (c *Client) AssertEventCountStays(cargoID string, n int) {
	c.t.Helper()
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		require.Len(c.t, c.Events(cargoID), n, "no event may be emitted")
		time.Sleep(50 * time.Millisecond)
	}
}

// Parallel runs fn n times concurrently, releasing all goroutines at once to
// maximise contention, and returns the results in call order.
func Parallel[T any](n int, fn func(i int) T) []T {
	results := make([]T, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = fn(i)
		}()
	}
	close(start)
	wg.Wait()
	return results
}

// StatusCounts tallies responses by HTTP status.
func StatusCounts(responses []Response) map[int]int {
	counts := map[int]int{}
	for _, r := range responses {
		counts[r.Status]++
	}
	return counts
}

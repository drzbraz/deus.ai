//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	. "github.com/drzbraz/deus.ai/tests/testkit"
)

func TestTracking_RecordMovementKeepsStatus(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoInStatus(InTransit)

	resp := c.Post(CargoPath(cargo.ID)+"/tracking", map[string]any{"location": " Gibraltar ", "note": "on schedule"})

	require.Equal(t, http.StatusCreated, resp.Status, resp.String())
	entry := Data[TrackingEntry](t, resp)
	assert.Equal(t, cargo.ID, entry.CargoID)
	assert.Equal(t, InTransit, entry.Status)
	assert.Equal(t, "Gibraltar", entry.Location)
	assert.Equal(t, "on schedule", entry.Note)

	after := c.GetCargo(cargo.ID)
	assert.Equal(t, InTransit, after.Status)
	assert.Equal(t, cargo.Version, after.Version, "a movement is not a status change")
	c.WaitForEvents(cargo.ID, 1)
	c.AssertEventCountStays(cargo.ID, 1)
}

func TestTracking_FullHistoryIsCompleteAndChronological(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()
	move := func(location string) {
		require.Equal(t, http.StatusCreated, c.Post(CargoPath(cargo.ID)+"/tracking", map[string]any{"location": location}).Status)
	}

	move("Warehouse 7")
	c.Patch(CargoPath(cargo.ID)+"/status", map[string]any{"status": InTransit, "location": "Rotterdam port"})
	move("North Sea")
	move("English Channel")
	c.Patch(CargoPath(cargo.ID)+"/status", map[string]any{"status": Delivered, "location": "Lisbon"})

	history := c.History(cargo.ID)
	assert.Equal(t, []string{"Rotterdam", "Warehouse 7", "Rotterdam port", "North Sea", "English Channel", "Lisbon"}, locations(history))
	assert.Equal(t, []string{Pending, Pending, InTransit, InTransit, InTransit, Delivered}, statuses(history))
	assert.True(t, sort.SliceIsSorted(history, func(i, j int) bool { return history[i].RecordedAt.Before(history[j].RecordedAt) }))

	seen := map[string]bool{}
	for _, entry := range history {
		assert.False(t, seen[entry.ID], "entry ids are unique")
		seen[entry.ID] = true
		assert.Equal(t, cargo.ID, entry.CargoID)
	}
}

func TestTracking_HistoryIsNeverRewritten(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(100)
	cargo := c.NewCargo(vessel.ID, 1)
	c.MustChangeStatus(cargo.ID, InTransit)
	snapshot := c.History(cargo.ID)

	c.Post(CargoPath(cargo.ID)+"/tracking", map[string]any{"location": "Somewhere"})
	c.Patch(VesselPath(vessel.ID)+"/location", map[string]any{"current_location": "Elsewhere"})
	c.MustChangeStatus(cargo.ID, Delivered)
	c.ChangeStatus(cargo.ID, Pending) // rejected

	assert.Equal(t, snapshot, c.History(cargo.ID)[:len(snapshot)], "earlier entries are byte-for-byte unchanged")
}

func TestTracking_DeliveredCargoAcceptsNoMovement(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoInStatus(Delivered)

	resp := c.Post(CargoPath(cargo.ID)+"/tracking", map[string]any{"location": "Too late"})

	require.Equal(t, http.StatusConflict, resp.Status)
	assert.Equal(t, "cargo_already_delivered", ErrorOf(t, resp).Code)
	assert.Len(t, c.History(cargo.ID), 3)
}

func TestTracking_Validation(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()
	path := CargoPath(cargo.ID) + "/tracking"

	tests := map[string]struct {
		body      map[string]any
		wantField string
	}{
		"missing location":  {map[string]any{"note": "x"}, "location"},
		"blank location":    {map[string]any{"location": "  "}, "location"},
		"location too long": {map[string]any{"location": strings.Repeat("x", 201)}, "location"},
		"note too long":     {map[string]any{"location": "x", "note": strings.Repeat("n", 1001)}, "note"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			resp := c.Post(path, tt.body)
			require.Equal(t, http.StatusUnprocessableEntity, resp.Status)
			assert.Contains(t, ErrorOf(t, resp).Details, tt.wantField)
		})
	}
	assert.Len(t, c.History(cargo.ID), 1)
}

func TestTracking_UnknownAndInvalidCargo(t *testing.T) {
	c := api(t)
	unknown := CargoPath(uuid.NewString())

	assert.Equal(t, http.StatusNotFound, c.Get(unknown+"/tracking").Status, "unknown cargo is 404, not an empty list")
	assert.Equal(t, http.StatusNotFound, c.Get(unknown+"/events").Status)
	assert.Equal(t, http.StatusNotFound, c.Post(unknown+"/tracking", map[string]any{"location": "x"}).Status)
	assert.Equal(t, http.StatusBadRequest, c.Get(CargoPath("x")+"/tracking").Status)
	assert.Equal(t, http.StatusBadRequest, c.Get(CargoPath("x")+"/events").Status)
}

func TestEvents_EmptyListForCargoWithoutChanges(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()

	assert.JSONEq(t, `[]`, extractData(t, c.Get(CargoPath(cargo.ID)+"/events")))
}

func TestEvents_MatchTheStatusChanges(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()
	moved := c.MustChangeStatus(cargo.ID, InTransit)
	delivered := c.MustChangeStatus(cargo.ID, Delivered)

	events := c.WaitForEvents(cargo.ID, 2)

	assert.Equal(t, moved.UpdatedAt, events[0].OccurredAt, "event time is the time of the change")
	assert.Equal(t, delivered.UpdatedAt, events[1].OccurredAt)
	assert.NotEqual(t, events[0].EventID, events[1].EventID)
	for _, e := range events {
		assert.Equal(t, cargo.ID, e.CargoID)
		assert.False(t, e.ReceivedAt.Before(e.OccurredAt), "received after it occurred")
	}
	c.AssertEventCountStays(cargo.ID, 2)
}

// Contract test of the message other services consume.
func TestEvents_WireFormatOnTheBroker(t *testing.T) {
	c := api(t)
	deliveries := TapEvents(t, stack.Env.RabbitMQURL)
	cargo := c.NewCargoOnFreshVessel()
	changed := c.MustChangeStatus(cargo.ID, InTransit)

	var body map[string]any
	timeout := time.After(15 * time.Second)
	for body == nil {
		select {
		case d := <-deliveries:
			var msg map[string]any
			require.NoError(t, json.Unmarshal(d.Body, &msg))
			if msg["cargo_id"] != cargo.ID {
				continue // another test's event
			}
			body = msg
			assert.Equal(t, "application/json", d.ContentType)
			assert.Equal(t, uint8(2), d.DeliveryMode, "persistent")
			assert.Equal(t, msg["event_id"], d.MessageId)
		case <-timeout:
			t.Fatal("no event published for the status change")
		}
	}

	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"event_id", "cargo_id", "old_status", "new_status", "timestamp"}, keys)
	assert.Equal(t, Pending, body["old_status"])
	assert.Equal(t, InTransit, body["new_status"])

	ts, err := time.Parse(time.RFC3339Nano, body["timestamp"].(string))
	require.NoError(t, err)
	assert.True(t, ts.Equal(changed.UpdatedAt))

	stored := c.WaitForEvents(cargo.ID, 1)
	assert.Equal(t, body["event_id"], stored[0].EventID, "the stored event is the published one")
}

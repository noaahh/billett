package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"billett/internal/model"
)

func TestInsertJourneyDeduplicatesBySourceHash(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "billett.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	journey := model.Journey{
		SourceHash:      "same-email",
		MessageID:       "<same@example.test>",
		EmailSubject:    "EasyRide Kaufquittung",
		JourneyDate:     "2026-07-01",
		OriginName:      "Origin",
		DestinationName: "Destination",
		ValidFrom:       "2026-07-01T10:17:00+02:00",
		ValidTo:         "2026-07-01T10:47:00+02:00",
		PriceCents:      240,
		Currency:        "CHF",
		RouteStatus:     "pending",
	}
	first, created, err := database.InsertJourney(context.Background(), journey)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first insert should create a journey")
	}
	second, created, err := database.InsertJourney(context.Background(), journey)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("duplicate insert should not create a journey")
	}
	if first.ID != second.ID {
		t.Fatalf("duplicate returned ID %q, want %q", second.ID, first.ID)
	}

	journeys, err := database.ListJourneys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(journeys) != 1 {
		t.Fatalf("got %d journeys, want 1", len(journeys))
	}
}

func TestRouteQueueProgressAndGeometryCache(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "billett.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	journey, _, err := database.InsertJourney(context.Background(), model.Journey{
		SourceHash:      "route-job",
		EmailSubject:    "Route job",
		JourneyDate:     "2026-07-01",
		OriginName:      "Zürich",
		DestinationName: "Bern",
		ValidFrom:       "2026-07-01T10:17:00+02:00",
		ValidTo:         "2026-07-01T11:47:00+02:00",
		Currency:        "CHF",
		RouteStatus:     "pending",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.RequeueIncompleteRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := database.ClaimNextRoute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !found || claimed.ID != journey.ID || claimed.RouteStatus != "matching" {
		t.Fatalf("claimed route: found=%v journey=%+v", found, claimed)
	}
	if err := database.UpdateRouteProgress(context.Background(), journey.ID, 48, "Tracing rail network"); err != nil {
		t.Fatal(err)
	}
	progressed, err := database.GetJourney(context.Background(), journey.ID)
	if err != nil {
		t.Fatal(err)
	}
	if progressed.RouteProgress != 48 || progressed.RouteProgressMessage != "Tracing rail network" {
		t.Fatalf("stored progress: %+v", progressed)
	}

	geometry := json.RawMessage(`[[8.0,47.0],[8.1,47.1]]`)
	if err := database.PutRouteGeometry(context.Background(), "corridor", geometry, time.Hour); err != nil {
		t.Fatal(err)
	}
	cached, found, err := database.GetRouteGeometry(context.Background(), "corridor")
	if err != nil {
		t.Fatal(err)
	}
	if !found || string(cached) != string(geometry) {
		t.Fatalf("cached geometry: found=%v value=%s", found, cached)
	}
}

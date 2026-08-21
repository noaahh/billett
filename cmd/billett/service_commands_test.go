package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"billett/internal/model"
	"billett/internal/store"
)

func TestBackupRestoreAndExportCommands(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "billett.sqlite")
	database, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	_, created, err := database.InsertJourney(context.Background(), model.Journey{
		SourceHash:      "command-test",
		EmailSubject:    "EasyRide Kaufquittung",
		JourneyDate:     "2026-08-08",
		OriginName:      "Zürich",
		DestinationName: "Bern",
		ValidFrom:       "2026-08-08T10:00:00+02:00",
		ValidTo:         "2026-08-08T11:00:00+02:00",
		PriceCents:      1200,
		Currency:        "CHF",
		RouteStatus:     "pending",
	})
	if err != nil || !created {
		t.Fatalf("seed command test: created=%v err=%v", created, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	backupPath := filepath.Join(directory, "backup.sqlite")
	if err := backupDatabase([]string{"-db", databasePath, "--output", backupPath}); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyDatabase(backupPath); err != nil {
		t.Fatal(err)
	}

	jsonPath := filepath.Join(directory, "journeys.json")
	if err := exportJourneys([]string{"-db", databasePath, "--format", "json", "--output", jsonPath}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var exported struct {
		Version  int             `json:"version"`
		Journeys []model.Journey `json:"journeys"`
	}
	if err := json.Unmarshal(raw, &exported); err != nil {
		t.Fatal(err)
	}
	if exported.Version != 1 || len(exported.Journeys) != 1 {
		t.Fatalf("unexpected export: version=%d journeys=%d", exported.Version, len(exported.Journeys))
	}

	restoredPath := filepath.Join(directory, "restored.sqlite")
	if err := restoreDatabase([]string{"-db", restoredPath, "--from", backupPath}); err != nil {
		t.Fatal(err)
	}
	if err := verifyDatabase([]string{restoredPath}); err != nil {
		t.Fatal(err)
	}
}

func TestCSVExportNeutralizesSpreadsheetFormulas(t *testing.T) {
	var output strings.Builder
	if err := writeCSV(&output, []model.Journey{{
		ID: "id", JourneyDate: "2026-08-08", OriginName: "=IMPORTXML(\"https://example.test\")", Notes: "@SUM(1,1)",
	}}); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(output.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if rows[1][2] != "'=IMPORTXML(\"https://example.test\")" || rows[1][15] != "'@SUM(1,1)" {
		t.Fatalf("formula cells were not neutralized: %q %q", rows[1][2], rows[1][15])
	}
}

func TestListenSecurityRequiresCompleteCredentialsOutsideLoopback(t *testing.T) {
	if err := validateListenSecurity("127.0.0.1:8080", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := validateListenSecurity("0.0.0.0:8080", "", ""); err == nil {
		t.Fatal("non-loopback listener accepted without authentication")
	}
	if err := validateListenSecurity("0.0.0.0:8080", "owner", ""); err == nil {
		t.Fatal("partial authentication configuration was accepted")
	}
	if err := validateListenSecurity("0.0.0.0:8080", "owner", "password"); err != nil {
		t.Fatal(err)
	}
}

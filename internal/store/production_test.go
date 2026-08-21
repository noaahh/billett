package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"billett/internal/model"
)

func TestProductionSchemaIngestionPatchDeleteAndBackup(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "billett.sqlite")
	database, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	info, err := os.Stat(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database permissions: got %o, want 600", info.Mode().Perm())
	}
	version, err := database.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("schema version: got %d, want %d", version, CurrentSchemaVersion)
	}
	emptySummary, err := database.IngestionSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if emptySummary.Total != 0 {
		t.Fatalf("empty ingestion summary: %+v", emptySummary)
	}

	ingestion, err := database.BeginIngestion(ctx, model.Ingestion{
		SourceHash: "raw-hash", SourceKind: "eml", SourceName: "receipt.eml", ParserVersion: "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	journey, created, err := database.InsertJourney(ctx, model.Journey{
		IngestionID: ingestion.ID, SourceHash: "journey-hash", EmailSubject: "EasyRide Kaufquittung",
		JourneyDate: "2026-07-01", OriginName: "Zürich", DestinationName: "Bern",
		ValidFrom: "2026-07-01T10:17:00+02:00", ValidTo: "2026-07-01T11:17:00+02:00",
		Currency: "CHF", RouteStatus: "pending",
	})
	if err != nil || !created {
		t.Fatalf("insert journey: created=%v err=%v", created, err)
	}
	if journey.EvidenceType != "observed" || journey.OccurrenceStatus != "confirmed" || journey.TimeSource != "recorded" {
		t.Fatalf("default evidence semantics: %+v", journey)
	}
	if _, err := database.FinishIngestion(ctx, ingestion.ID, "imported", 1, 1, "", false); err != nil {
		t.Fatal(err)
	}
	summary, err := database.IngestionSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Imported != 1 || summary.Journeys != 1 {
		t.Fatalf("ingestion summary: %+v", summary)
	}

	status := "excluded"
	notes := "Ticket was refunded"
	patched, err := database.PatchJourney(ctx, journey.ID, model.JourneyPatch{OccurrenceStatus: &status, Notes: &notes})
	if err != nil {
		t.Fatal(err)
	}
	if patched.OccurrenceStatus != status || patched.Notes != notes || patched.RouteStatus != "queued" {
		t.Fatalf("patched journey: %+v", patched)
	}

	backupPath := filepath.Join(t.TempDir(), "backup.sqlite")
	if err := database.Backup(ctx, backupPath); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDatabase(backupPath); err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteJourney(ctx, journey.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.GetJourney(ctx, journey.ID); err == nil {
		t.Fatal("deleted journey is still available")
	}
}

func TestBeginIngestionReusesSourceAndCountsAttempts(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "billett.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	first, err := database.BeginIngestion(ctx, model.Ingestion{SourceHash: "same", SourceKind: "eml", ParserVersion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := database.BeginIngestion(ctx, model.Ingestion{SourceHash: "same", SourceKind: "mbox", ParserVersion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || second.AttemptCount != 2 || second.SourceKind != "mbox" {
		t.Fatalf("repeated ingestion: first=%+v second=%+v", first, second)
	}
}

func TestVerifyDatabaseRejectsMissingAndUnrelatedFiles(t *testing.T) {
	directory := t.TempDir()
	missing := filepath.Join(directory, "missing.sqlite")
	if err := VerifyDatabase(missing); err == nil {
		t.Fatal("missing database was accepted")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verification created the missing database: %v", err)
	}
	unrelated := filepath.Join(directory, "unrelated.sqlite")
	if err := os.WriteFile(unrelated, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDatabase(unrelated); err == nil {
		t.Fatal("unrelated file was accepted")
	}
	databasePath := filepath.Join(directory, "billett.sqlite")
	database, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDatabase(databasePath); err != nil {
		t.Fatal(err)
	}
}

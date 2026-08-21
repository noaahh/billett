package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"billett/internal/model"
)

func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

func (s *Store) BeginIngestion(ctx context.Context, ingestion model.Ingestion) (model.Ingestion, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	if ingestion.ID == "" {
		ingestion.ID = newID()
	}
	if ingestion.Status == "" {
		ingestion.Status = "processing"
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO ingestions (
    id, source_hash, message_id, source_kind, source_name, email_subject,
    status, parser_version, journey_count, created_count, error, raw_archived,
    attempt_count, first_received_at, last_attempt_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 0, '', 0, 1, ?, ?)
ON CONFLICT(source_hash) DO UPDATE SET
    message_id = CASE WHEN excluded.message_id <> '' THEN excluded.message_id ELSE ingestions.message_id END,
    source_kind = excluded.source_kind,
    source_name = excluded.source_name,
    email_subject = CASE WHEN excluded.email_subject <> '' THEN excluded.email_subject ELSE ingestions.email_subject END,
    status = 'processing',
    parser_version = excluded.parser_version,
    error = '',
    attempt_count = ingestions.attempt_count + 1,
    last_attempt_at = excluded.last_attempt_at
`, ingestion.ID, ingestion.SourceHash, ingestion.MessageID, ingestion.SourceKind,
		ingestion.SourceName, ingestion.EmailSubject, ingestion.Status, ingestion.ParserVersion, now, now)
	if err != nil {
		return model.Ingestion{}, fmt.Errorf("begin ingestion: %w", err)
	}
	return s.GetIngestionByHash(ctx, ingestion.SourceHash)
}

func (s *Store) FinishIngestion(ctx context.Context, id, status string, journeyCount, createdCount int, detail string, rawArchived bool) (model.Ingestion, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE ingestions
SET status = ?, journey_count = ?, created_count = ?, error = ?, raw_archived = ?
WHERE id = ?
`, status, journeyCount, createdCount, detail, rawArchived, id)
	if err != nil {
		return model.Ingestion{}, fmt.Errorf("finish ingestion: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return model.Ingestion{}, fmt.Errorf("read ingestion update result: %w", err)
	}
	if rows != 1 {
		return model.Ingestion{}, fmt.Errorf("ingestion not found")
	}
	return s.GetIngestion(ctx, id)
}

func (s *Store) GetIngestion(ctx context.Context, id string) (model.Ingestion, error) {
	return scanIngestion(s.db.QueryRowContext(ctx, ingestionColumns+` WHERE id = ?`, id))
}

func (s *Store) GetIngestionByHash(ctx context.Context, sourceHash string) (model.Ingestion, error) {
	return scanIngestion(s.db.QueryRowContext(ctx, ingestionColumns+` WHERE source_hash = ?`, sourceHash))
}

func (s *Store) ListIngestions(ctx context.Context, limit int) ([]model.Ingestion, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, ingestionColumns+` ORDER BY last_attempt_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list ingestions: %w", err)
	}
	defer rows.Close()
	result := make([]model.Ingestion, 0)
	for rows.Next() {
		item, scanErr := scanIngestion(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ingestions: %w", err)
	}
	return result, nil
}

func (s *Store) IngestionSummary(ctx context.Context) (model.IngestionSummary, error) {
	var summary model.IngestionSummary
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(CASE WHEN status = 'imported' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN status = 'duplicate' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN status = 'rejected' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(created_count), 0)
FROM ingestions
`).Scan(&summary.Total, &summary.Imported, &summary.Duplicates, &summary.Rejected, &summary.Failed, &summary.Journeys)
	if err != nil {
		return model.IngestionSummary{}, fmt.Errorf("summarize ingestions: %w", err)
	}
	return summary, nil
}

const ingestionColumns = `
SELECT id, source_hash, message_id, source_kind, source_name, email_subject,
       status, parser_version, journey_count, created_count, error, raw_archived,
       attempt_count, first_received_at, last_attempt_at
FROM ingestions`

func scanIngestion(row scanner) (model.Ingestion, error) {
	var ingestion model.Ingestion
	err := row.Scan(&ingestion.ID, &ingestion.SourceHash, &ingestion.MessageID,
		&ingestion.SourceKind, &ingestion.SourceName, &ingestion.EmailSubject,
		&ingestion.Status, &ingestion.ParserVersion, &ingestion.JourneyCount,
		&ingestion.CreatedCount, &ingestion.Error, &ingestion.RawArchived,
		&ingestion.AttemptCount, &ingestion.FirstReceivedAt, &ingestion.LastAttemptAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Ingestion{}, fmt.Errorf("ingestion not found")
	}
	if err != nil {
		return model.Ingestion{}, fmt.Errorf("scan ingestion: %w", err)
	}
	return ingestion, nil
}

func (s *Store) PatchJourney(ctx context.Context, id string, patch model.JourneyPatch) (model.Journey, error) {
	journey, err := s.GetJourney(ctx, id)
	if err != nil {
		return model.Journey{}, err
	}
	if patch.JourneyDate != nil {
		if _, err := time.Parse("2006-01-02", *patch.JourneyDate); err != nil {
			return model.Journey{}, fmt.Errorf("journeyDate must use YYYY-MM-DD")
		}
		journey.JourneyDate = *patch.JourneyDate
	}
	if patch.OriginName != nil {
		journey.OriginName = strings.TrimSpace(*patch.OriginName)
	}
	if patch.DestinationName != nil {
		journey.DestinationName = strings.TrimSpace(*patch.DestinationName)
	}
	if journey.OriginName == "" || journey.DestinationName == "" {
		return model.Journey{}, fmt.Errorf("originName and destinationName are required")
	}
	if patch.ValidFrom != nil {
		journey.ValidFrom = *patch.ValidFrom
	}
	if patch.ValidTo != nil {
		journey.ValidTo = *patch.ValidTo
	}
	start, startErr := time.Parse(time.RFC3339, journey.ValidFrom)
	end, endErr := time.Parse(time.RFC3339, journey.ValidTo)
	if startErr != nil || endErr != nil || !end.After(start) {
		return model.Journey{}, fmt.Errorf("validFrom and validTo must be RFC3339 values with validTo after validFrom")
	}
	if patch.OccurrenceStatus != nil {
		switch *patch.OccurrenceStatus {
		case "confirmed", "unconfirmed", "excluded":
			journey.OccurrenceStatus = *patch.OccurrenceStatus
		default:
			return model.Journey{}, fmt.Errorf("occurrenceStatus must be confirmed, unconfirmed, or excluded")
		}
	}
	if patch.TimeSource != nil {
		switch *patch.TimeSource {
		case "recorded", "validity_window", "user_supplied", "timetable_estimate":
			journey.TimeSource = *patch.TimeSource
		default:
			return model.Journey{}, fmt.Errorf("unsupported timeSource")
		}
	}
	if patch.Notes != nil {
		journey.Notes = strings.TrimSpace(*patch.Notes)
		if len(journey.Notes) > 2000 {
			return model.Journey{}, fmt.Errorf("notes must not exceed 2000 characters")
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.ExecContext(ctx, `
UPDATE journeys
SET journey_date = ?, origin_name = ?, destination_name = ?, valid_from = ?, valid_to = ?,
    occurrence_status = ?, time_source = ?, notes = ?,
    route_status = 'queued', route_progress = 0,
    route_progress_message = 'Waiting for route matcher', route_geojson = '',
    route_provider = '', route_summary = '', route_confidence = 0,
    route_data_version = '', updated_at = ?
WHERE id = ?
`, journey.JourneyDate, journey.OriginName, journey.DestinationName, journey.ValidFrom,
		journey.ValidTo, journey.OccurrenceStatus, journey.TimeSource, journey.Notes, now, id)
	if err != nil {
		return model.Journey{}, fmt.Errorf("update journey: %w", err)
	}
	return s.GetJourney(ctx, id)
}

func (s *Store) DeleteJourney(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM journeys WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete journey: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read delete result: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("journey not found")
	}
	return nil
}

func (s *Store) FindComparableRoute(ctx context.Context, origin, destination string) (model.RouteUpdate, bool, error) {
	var update model.RouteUpdate
	var geoJSON string
	var timeEstimated bool
	err := s.db.QueryRowContext(ctx, `
SELECT route_confidence, route_summary, route_geojson, route_data_version, route_time_estimated
FROM journeys
WHERE lower(origin_name) = lower(?) AND lower(destination_name) = lower(?)
  AND route_geojson <> '' AND route_status IN ('matched', 'ambiguous', 'estimated')
ORDER BY CASE route_status WHEN 'matched' THEN 0 WHEN 'ambiguous' THEN 1 ELSE 2 END,
         route_confidence DESC, journey_date DESC
LIMIT 1
`, strings.TrimSpace(origin), strings.TrimSpace(destination)).Scan(
		&update.Confidence, &update.Summary, &geoJSON, &update.DataVersion, &timeEstimated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return model.RouteUpdate{}, false, nil
	}
	if err != nil {
		return model.RouteUpdate{}, false, fmt.Errorf("find comparable route: %w", err)
	}
	update.GeoJSON = []byte(geoJSON)
	update.TimeEstimated = timeEstimated
	return update, true, nil
}

func (s *Store) Backup(ctx context.Context, destination string) error {
	if strings.TrimSpace(destination) == "" {
		return fmt.Errorf("backup destination is required")
	}
	databasePath, err := filepath.Abs(s.path)
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}
	destinationPath, err := filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("resolve backup destination: %w", err)
	}
	if databasePath == destinationPath {
		return fmt.Errorf("backup destination must differ from the database path")
	}
	if _, err := os.Stat(destinationPath); err == nil {
		return fmt.Errorf("backup destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect backup destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o700); err != nil {
		return fmt.Errorf("create backup directory: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, destinationPath); err != nil {
		return fmt.Errorf("create SQLite backup: %w", err)
	}
	if err := os.Chmod(destinationPath, 0o600); err != nil {
		return fmt.Errorf("secure backup permissions: %w", err)
	}
	return VerifyDatabase(destinationPath)
}

func VerifyDatabase(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect database: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("database must be a non-empty regular file")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}
	databaseURL := &url.URL{Scheme: "file", Path: absolutePath, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", databaseURL.String())
	if err != nil {
		return fmt.Errorf("open database for verification: %w", err)
	}
	defer db.Close()
	var result string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil {
		return fmt.Errorf("run SQLite integrity check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("SQLite integrity check returned %q", result)
	}
	var requiredTables int
	if err := db.QueryRow(`
SELECT COUNT(*) FROM sqlite_master
WHERE type = 'table' AND name IN ('journeys', 'schema_migrations')
`).Scan(&requiredTables); err != nil {
		return fmt.Errorf("inspect application schema: %w", err)
	}
	if requiredTables != 2 {
		return fmt.Errorf("database is not a billett database")
	}
	var schemaVersion int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&schemaVersion); err != nil {
		return fmt.Errorf("read database schema version: %w", err)
	}
	if schemaVersion < 1 || schemaVersion > CurrentSchemaVersion {
		return fmt.Errorf("unsupported billett schema version %d", schemaVersion)
	}
	return nil
}

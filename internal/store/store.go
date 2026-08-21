package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"billett/internal/model"

	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	path string
}

const CurrentSchemaVersion = 3

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, path: path}
	if err := store.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := secureDatabaseFiles(path); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func secureDatabaseFiles(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(candidate, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("secure database file %s: %w", filepath.Base(candidate), err)
		}
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
PRAGMA busy_timeout = 5000;
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    applied_at TEXT NOT NULL
);
`); err != nil {
		return fmt.Errorf("initialize migrations: %w", err)
	}
	migrations := []struct {
		version int
		name    string
		apply   func(context.Context) error
	}{
		{version: 1, name: "initial schema", apply: s.migrateInitialSchema},
		{version: 2, name: "journey evidence and provenance", apply: s.migrateJourneyEvidence},
		{version: 3, name: "ingestion audit ledger", apply: s.migrateIngestionLedger},
	}
	for _, migration := range migrations {
		var applied int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, migration.version,
		).Scan(&applied); err != nil {
			return fmt.Errorf("inspect migration %d: %w", migration.version, err)
		}
		if applied > 0 {
			continue
		}
		if err := migration.apply(ctx); err != nil {
			return fmt.Errorf("apply migration %d (%s): %w", migration.version, migration.name, err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			migration.version, migration.name, time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			return fmt.Errorf("record migration %d: %w", migration.version, err)
		}
	}
	return nil
}

func (s *Store) migrateInitialSchema(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS journeys (
    id TEXT PRIMARY KEY,
    source_hash TEXT NOT NULL UNIQUE,
    source_filename TEXT NOT NULL DEFAULT '',
    message_id TEXT NOT NULL DEFAULT '',
    email_subject TEXT NOT NULL,
    sender TEXT NOT NULL DEFAULT '',
    order_number TEXT NOT NULL DEFAULT '',
    reference_number TEXT NOT NULL DEFAULT '',
    journey_date TEXT NOT NULL,
    origin_name TEXT NOT NULL,
    destination_name TEXT NOT NULL,
    valid_from TEXT NOT NULL,
    valid_to TEXT NOT NULL,
    travel_class TEXT NOT NULL DEFAULT '',
    fare_product TEXT NOT NULL DEFAULT '',
    fare_detail TEXT NOT NULL DEFAULT '',
    fare_reduction TEXT NOT NULL DEFAULT '',
    payment_method TEXT NOT NULL DEFAULT '',
    price_cents INTEGER NOT NULL,
    currency TEXT NOT NULL DEFAULT 'CHF',
    route_status TEXT NOT NULL DEFAULT 'pending',
    route_progress INTEGER NOT NULL DEFAULT 0,
    route_progress_message TEXT NOT NULL DEFAULT '',
    route_provider TEXT NOT NULL DEFAULT '',
    route_confidence REAL NOT NULL DEFAULT 0,
    route_summary TEXT NOT NULL DEFAULT '',
    route_geojson TEXT NOT NULL DEFAULT '',
    route_external_notice TEXT NOT NULL DEFAULT '',
    route_cache_hits INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS journeys_message_id_unique
    ON journeys(message_id) WHERE message_id <> '';
CREATE INDEX IF NOT EXISTS journeys_journey_date_idx ON journeys(journey_date DESC);
CREATE TABLE IF NOT EXISTS route_geometry_cache (
    cache_key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    last_used_at TEXT NOT NULL,
    hit_count INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS route_geometry_cache_expires_idx
    ON route_geometry_cache(expires_at);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("create initial schema: %w", err)
	}
	columns := []struct {
		name       string
		definition string
	}{
		{name: "route_progress", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "route_progress_message", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "route_cache_hits", definition: "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, column := range columns {
		if err := s.ensureJourneyColumn(ctx, column.name, column.definition); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrateJourneyEvidence(ctx context.Context) error {
	columns := []struct {
		name       string
		definition string
	}{
		{name: "ingestion_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "evidence_type", definition: "TEXT NOT NULL DEFAULT 'observed'"},
		{name: "occurrence_status", definition: "TEXT NOT NULL DEFAULT 'confirmed'"},
		{name: "time_source", definition: "TEXT NOT NULL DEFAULT 'recorded'"},
		{name: "notes", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "route_data_version", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "route_time_estimated", definition: "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, column := range columns {
		if err := s.ensureJourneyColumn(ctx, column.name, column.definition); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE journeys
SET evidence_type = 'ticket', occurrence_status = 'unconfirmed',
    time_source = 'validity_window', route_time_estimated = 1
WHERE lower(email_subject) LIKE '%ihr online-kauf bei der sbb%'
`); err != nil {
		return fmt.Errorf("classify existing ticket purchases: %w", err)
	}
	return nil
}

func (s *Store) migrateIngestionLedger(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS ingestions (
    id TEXT PRIMARY KEY,
    source_hash TEXT NOT NULL UNIQUE,
    message_id TEXT NOT NULL DEFAULT '',
    source_kind TEXT NOT NULL,
    source_name TEXT NOT NULL DEFAULT '',
    email_subject TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    parser_version TEXT NOT NULL DEFAULT '',
    journey_count INTEGER NOT NULL DEFAULT 0,
    created_count INTEGER NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT '',
    raw_archived INTEGER NOT NULL DEFAULT 0,
    attempt_count INTEGER NOT NULL DEFAULT 1,
    first_received_at TEXT NOT NULL,
    last_attempt_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS ingestions_last_attempt_idx ON ingestions(last_attempt_at DESC);
CREATE INDEX IF NOT EXISTS ingestions_status_idx ON ingestions(status);
CREATE INDEX IF NOT EXISTS journeys_ingestion_id_idx ON journeys(ingestion_id);
`)
	if err != nil {
		return fmt.Errorf("create ingestion ledger: %w", err)
	}
	return nil
}

func (s *Store) ensureJourneyColumn(ctx context.Context, name, definition string) error {
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('journeys') WHERE name = ?`, name,
	).Scan(&count); err != nil {
		return fmt.Errorf("inspect journey column %s: %w", name, err)
	}
	if count > 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE journeys ADD COLUMN "+name+" "+definition); err != nil {
		return fmt.Errorf("add journey column %s: %w", name, err)
	}
	return nil
}

func (s *Store) InsertJourney(ctx context.Context, journey model.Journey) (model.Journey, bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	journey.ID = newID()
	journey.CreatedAt = now
	journey.UpdatedAt = now
	if journey.EvidenceType == "" {
		journey.EvidenceType = "observed"
	}
	if journey.OccurrenceStatus == "" {
		journey.OccurrenceStatus = "confirmed"
	}
	if journey.TimeSource == "" {
		journey.TimeSource = "recorded"
	}

	result, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO journeys (
    id, ingestion_id, source_hash, source_filename, message_id, email_subject, sender,
    order_number, reference_number, journey_date, origin_name, destination_name,
    valid_from, valid_to, travel_class, fare_product, fare_detail, fare_reduction,
    payment_method, price_cents, currency, evidence_type, occurrence_status,
    time_source, notes, route_status, route_progress,
    route_progress_message, route_provider, route_confidence, route_summary,
    route_geojson, route_external_notice, route_cache_hits, route_data_version,
    route_time_estimated,
    created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, journey.ID, journey.IngestionID, journey.SourceHash, journey.SourceFilename, journey.MessageID,
		journey.EmailSubject, journey.Sender, journey.OrderNumber, journey.ReferenceNumber,
		journey.JourneyDate, journey.OriginName, journey.DestinationName, journey.ValidFrom,
		journey.ValidTo, journey.TravelClass, journey.FareProduct, journey.FareDetail,
		journey.FareReduction, journey.PaymentMethod, journey.PriceCents, journey.Currency,
		journey.EvidenceType, journey.OccurrenceStatus, journey.TimeSource, journey.Notes,
		journey.RouteStatus, journey.RouteProgress, journey.RouteProgressMessage,
		journey.RouteProvider, journey.RouteConfidence,
		journey.RouteSummary, string(journey.RouteGeoJSON), journey.RouteExternalNotice,
		journey.RouteCacheHits, journey.RouteDataVersion, journey.RouteTimeEstimated,
		journey.CreatedAt, journey.UpdatedAt)
	if err != nil {
		return model.Journey{}, false, fmt.Errorf("insert journey: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return model.Journey{}, false, fmt.Errorf("read insert result: %w", err)
	}
	if rows == 1 {
		return journey, true, nil
	}

	existing, err := s.findDuplicate(ctx, journey.SourceHash, journey.MessageID)
	if err != nil {
		return model.Journey{}, false, err
	}
	return existing, false, nil
}

func (s *Store) ListJourneys(ctx context.Context) ([]model.Journey, error) {
	rows, err := s.db.QueryContext(ctx, selectColumns+` ORDER BY journey_date DESC, valid_from DESC, created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list journeys: %w", err)
	}
	defer rows.Close()

	journeys := make([]model.Journey, 0)
	for rows.Next() {
		journey, scanErr := scanJourney(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		journeys = append(journeys, journey)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate journeys: %w", err)
	}
	return journeys, nil
}

func (s *Store) GetJourney(ctx context.Context, id string) (model.Journey, error) {
	row := s.db.QueryRowContext(ctx, selectColumns+` WHERE id = ?`, id)
	journey, err := scanJourney(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Journey{}, fmt.Errorf("journey not found")
	}
	return journey, err
}

func (s *Store) UpdateRoute(ctx context.Context, id string, update model.RouteUpdate) (model.Journey, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := s.db.ExecContext(ctx, `
UPDATE journeys
SET route_status = ?, route_progress = 100, route_progress_message = ?,
    route_provider = ?, route_confidence = ?, route_summary = ?, route_geojson = ?,
    route_external_notice = ?, route_cache_hits = ?, route_data_version = ?,
    route_time_estimated = ?, updated_at = ?
WHERE id = ?
`, update.Status, routeCompletionMessage(update.Status), update.Provider, update.Confidence,
		update.Summary, string(update.GeoJSON), update.ExternalDataNotice, update.CacheHits,
		update.DataVersion, update.TimeEstimated, now, id)
	if err != nil {
		return model.Journey{}, fmt.Errorf("update route: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return model.Journey{}, fmt.Errorf("read route update result: %w", err)
	}
	if rows != 1 {
		return model.Journey{}, fmt.Errorf("journey not found")
	}
	return s.GetJourney(ctx, id)
}

func routeCompletionMessage(status string) string {
	if status == "error" {
		return "Route matching failed"
	}
	return "Route matched"
}

func (s *Store) QueueRoute(ctx context.Context, id string) (model.Journey, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := s.db.ExecContext(ctx, `
UPDATE journeys
SET route_status = 'queued', route_progress = 0,
    route_progress_message = 'Waiting for route matcher', updated_at = ?
WHERE id = ?
`, now, id)
	if err != nil {
		return model.Journey{}, fmt.Errorf("queue route: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return model.Journey{}, fmt.Errorf("read route queue result: %w", err)
	}
	if rows != 1 {
		return model.Journey{}, fmt.Errorf("journey not found")
	}
	return s.GetJourney(ctx, id)
}

func (s *Store) RequeueIncompleteRoutes(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
UPDATE journeys
SET route_status = 'queued', route_progress = 0,
    route_progress_message = 'Waiting for route matcher', updated_at = ?
WHERE route_status IN ('pending', 'matching')
`, now)
	if err != nil {
		return fmt.Errorf("requeue incomplete routes: %w", err)
	}
	return nil
}

func (s *Store) ClaimNextRoute(ctx context.Context) (model.Journey, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Journey{}, false, fmt.Errorf("begin route claim: %w", err)
	}
	defer tx.Rollback()

	var id string
	err = tx.QueryRowContext(ctx, `
SELECT id FROM journeys
WHERE route_status = 'queued'
ORDER BY updated_at, created_at, id
LIMIT 1
`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Journey{}, false, nil
	}
	if err != nil {
		return model.Journey{}, false, fmt.Errorf("select route job: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `
UPDATE journeys
SET route_status = 'matching', route_progress = 5,
    route_progress_message = 'Starting route matching', updated_at = ?
WHERE id = ? AND route_status = 'queued'
`, now, id); err != nil {
		return model.Journey{}, false, fmt.Errorf("claim route job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Journey{}, false, fmt.Errorf("commit route claim: %w", err)
	}
	journey, err := s.GetJourney(ctx, id)
	if err != nil {
		return model.Journey{}, false, err
	}
	return journey, true, nil
}

func (s *Store) UpdateRouteProgress(ctx context.Context, id string, progress int, message string) error {
	progress = max(0, min(99, progress))
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
UPDATE journeys
SET route_progress = ?, route_progress_message = ?, updated_at = ?
WHERE id = ? AND route_status = 'matching'
`, progress, message, now, id)
	if err != nil {
		return fmt.Errorf("update route progress: %w", err)
	}
	return nil
}

func (s *Store) GetRouteGeometry(ctx context.Context, key string) (json.RawMessage, bool, error) {
	var value string
	var expiresAt string
	err := s.db.QueryRowContext(ctx, `
SELECT value, expires_at FROM route_geometry_cache WHERE cache_key = ?
`, key).Scan(&value, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read route geometry cache: %w", err)
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || !expires.After(time.Now().UTC()) {
		if _, deleteErr := s.db.ExecContext(ctx, `DELETE FROM route_geometry_cache WHERE cache_key = ?`, key); deleteErr != nil {
			return nil, false, fmt.Errorf("delete expired route geometry cache: %w", deleteErr)
		}
		return nil, false, nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `
UPDATE route_geometry_cache
SET last_used_at = ?, hit_count = hit_count + 1
WHERE cache_key = ?
`, now, key); err != nil {
		return nil, false, fmt.Errorf("record route geometry cache hit: %w", err)
	}
	return json.RawMessage(value), true, nil
}

func (s *Store) PutRouteGeometry(ctx context.Context, key string, value json.RawMessage, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("route geometry cache TTL must be positive")
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO route_geometry_cache (
    cache_key, value, created_at, expires_at, last_used_at, hit_count
) VALUES (?, ?, ?, ?, ?, 0)
ON CONFLICT(cache_key) DO UPDATE SET
    value = excluded.value,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at
`, key, string(value), now.Format(time.RFC3339Nano), now.Add(ttl).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("write route geometry cache: %w", err)
	}
	return nil
}

func (s *Store) findDuplicate(ctx context.Context, sourceHash, messageID string) (model.Journey, error) {
	row := s.db.QueryRowContext(ctx, selectColumns+`
 WHERE source_hash = ? OR (? <> '' AND message_id = ?)
 LIMIT 1`, sourceHash, messageID, messageID)
	journey, err := scanJourney(row)
	if err != nil {
		return model.Journey{}, fmt.Errorf("find duplicate journey: %w", err)
	}
	return journey, nil
}

const selectColumns = `
SELECT id, ingestion_id, source_hash, source_filename, message_id, email_subject, sender,
       order_number, reference_number, journey_date, origin_name, destination_name,
       valid_from, valid_to, travel_class, fare_product, fare_detail, fare_reduction,
       payment_method, price_cents, currency, evidence_type, occurrence_status,
       time_source, notes, route_status, route_progress,
       route_progress_message, route_provider, route_confidence, route_summary,
       route_geojson, route_external_notice, route_cache_hits, route_data_version,
       route_time_estimated,
       created_at, updated_at
FROM journeys`

type scanner interface {
	Scan(dest ...any) error
}

func scanJourney(row scanner) (model.Journey, error) {
	var journey model.Journey
	var routeGeoJSON string
	err := row.Scan(
		&journey.ID, &journey.IngestionID, &journey.SourceHash, &journey.SourceFilename, &journey.MessageID,
		&journey.EmailSubject, &journey.Sender, &journey.OrderNumber,
		&journey.ReferenceNumber, &journey.JourneyDate, &journey.OriginName,
		&journey.DestinationName, &journey.ValidFrom, &journey.ValidTo,
		&journey.TravelClass, &journey.FareProduct, &journey.FareDetail,
		&journey.FareReduction, &journey.PaymentMethod, &journey.PriceCents,
		&journey.Currency, &journey.EvidenceType, &journey.OccurrenceStatus,
		&journey.TimeSource, &journey.Notes, &journey.RouteStatus, &journey.RouteProgress,
		&journey.RouteProgressMessage, &journey.RouteProvider,
		&journey.RouteConfidence, &journey.RouteSummary, &routeGeoJSON,
		&journey.RouteExternalNotice, &journey.RouteCacheHits, &journey.RouteDataVersion,
		&journey.RouteTimeEstimated,
		&journey.CreatedAt, &journey.UpdatedAt,
	)
	if err != nil {
		return model.Journey{}, err
	}
	if routeGeoJSON != "" && json.Valid([]byte(routeGeoJSON)) {
		journey.RouteGeoJSON = json.RawMessage(routeGeoJSON)
	}
	return journey, nil
}

func newID() string {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		panic(fmt.Sprintf("generate journey ID: %v", err))
	}
	return hex.EncodeToString(random)
}

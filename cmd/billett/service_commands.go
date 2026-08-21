package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"billett/internal/ingest"
	"billett/internal/model"
	"billett/internal/store"
)

func validateListenSecurity(address, username, password string) error {
	if (username == "") != (password == "") {
		return fmt.Errorf("BILLETT_AUTH_USERNAME and BILLETT_AUTH_PASSWORD must be configured together")
	}
	if username != "" {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("parse listen address: %w", err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("authentication is required when listening on non-loopback address %q", address)
}

func importMBOX(arguments []string) error {
	flags := flag.NewFlagSet("import-mbox", flag.ContinueOnError)
	databasePath := flags.String("db", envOr("BILLETT_DB", "./data/billett.sqlite"), "SQLite database path")
	rawArchiveDir := flags.String("raw-archive", os.Getenv("BILLETT_RAW_ARCHIVE_DIR"), "optional private raw email archive directory")
	reportPath := flags.String("report", "", "optional JSON report path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("provide exactly one MBOX file")
	}
	database, err := store.Open(*databasePath)
	if err != nil {
		return err
	}
	defer database.Close()
	service := &ingest.Service{Store: database, RawArchiveDir: *rawArchiveDir}
	report, err := service.ImportMBOX(context.Background(), flags.Arg(0))
	if err != nil {
		return err
	}
	printBatchReport(report)
	if *reportPath != "" {
		if err := writeJSONFile(*reportPath, report); err != nil {
			return err
		}
	}
	if report.Failed > 0 {
		return fmt.Errorf("%d MBOX message(s) failed to import; inspect the report", report.Failed)
	}
	return nil
}

func printBatchReport(report ingest.BatchReport) {
	for _, item := range report.Items {
		fmt.Printf("%s: %s", item.Status, item.SourceName)
		if item.Created > 0 {
			fmt.Printf(" (%d journey(s))", item.Created)
		}
		if item.Error != "" {
			fmt.Printf(" (%s)", item.Error)
		}
		fmt.Println()
	}
	fmt.Printf("messages=%d imported=%d duplicates=%d rejected=%d failed=%d journeys=%d\n",
		report.Messages, report.Imported, report.Duplicates, report.Rejected, report.Failed, report.Journeys)
}

func backupDatabase(arguments []string) error {
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	databasePath := flags.String("db", envOr("BILLETT_DB", "./data/billett.sqlite"), "SQLite database path")
	output := flags.String("output", "", "backup destination")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *output == "" {
		return fmt.Errorf("--output is required")
	}
	database, err := store.Open(*databasePath)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := database.Backup(context.Background(), *output); err != nil {
		return err
	}
	fmt.Printf("backup verified: %s\n", *output)
	return nil
}

func restoreDatabase(arguments []string) error {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	databasePath := flags.String("db", envOr("BILLETT_DB", "./data/billett.sqlite"), "SQLite database path")
	source := flags.String("from", "", "verified SQLite backup")
	replace := flags.Bool("replace", false, "replace an existing database after preserving it")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *source == "" {
		return fmt.Errorf("--from is required")
	}
	if err := store.VerifyDatabase(*source); err != nil {
		return fmt.Errorf("verify restore source: %w", err)
	}
	sourcePath, err := filepath.Abs(*source)
	if err != nil {
		return err
	}
	targetPath, err := filepath.Abs(*databasePath)
	if err != nil {
		return err
	}
	if sourcePath == targetPath {
		return fmt.Errorf("restore source and database path must differ")
	}
	if _, err := os.Stat(targetPath); err == nil {
		if !*replace {
			return fmt.Errorf("database exists; stop the service and pass --replace to preserve and replace it")
		}
		database, openErr := store.Open(targetPath)
		if openErr != nil {
			return fmt.Errorf("open existing database before restore: %w", openErr)
		}
		recoveryPath := targetPath + ".before-restore-" + time.Now().UTC().Format("20060102T150405Z")
		backupErr := database.Backup(context.Background(), recoveryPath)
		closeErr := database.Close()
		if backupErr != nil {
			return fmt.Errorf("preserve existing database: %w", backupErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close existing database: %w", closeErr)
		}
		fmt.Printf("existing database preserved: %s\n", recoveryPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect restore target: %w", err)
	}
	if err := copyFileAtomic(sourcePath, targetPath); err != nil {
		return err
	}
	_ = os.Remove(targetPath + "-wal")
	_ = os.Remove(targetPath + "-shm")
	if err := store.VerifyDatabase(targetPath); err != nil {
		return fmt.Errorf("verify restored database: %w", err)
	}
	fmt.Printf("database restored and verified: %s\n", targetPath)
	return nil
}

func verifyDatabase(arguments []string) error {
	flags := flag.NewFlagSet("verify-database", flag.ContinueOnError)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("provide exactly one SQLite database")
	}
	if err := store.VerifyDatabase(flags.Arg(0)); err != nil {
		return err
	}
	fmt.Printf("database verified: %s\n", flags.Arg(0))
	return nil
}

func exportJourneys(arguments []string) error {
	flags := flag.NewFlagSet("export", flag.ContinueOnError)
	databasePath := flags.String("db", envOr("BILLETT_DB", "./data/billett.sqlite"), "SQLite database path")
	format := flags.String("format", "json", "json, csv, or geojson")
	output := flags.String("output", "", "export destination")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *output == "" {
		return fmt.Errorf("--output is required")
	}
	database, err := store.Open(*databasePath)
	if err != nil {
		return err
	}
	defer database.Close()
	journeys, err := database.ListJourneys(context.Background())
	if err != nil {
		return err
	}
	file, err := openPrivateOutput(*output)
	if err != nil {
		return err
	}
	defer file.Close()
	switch *format {
	case "json":
		err = json.NewEncoder(file).Encode(map[string]any{"version": 1, "journeys": journeys})
	case "csv":
		err = writeCSV(file, journeys)
	case "geojson":
		err = writeGeoJSON(file, journeys)
	default:
		return fmt.Errorf("--format must be json, csv, or geojson")
	}
	if err != nil {
		return fmt.Errorf("write export: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync export: %w", err)
	}
	fmt.Printf("exported %d journey(s): %s\n", len(journeys), *output)
	return nil
}

func writeCSV(writer io.Writer, journeys []model.Journey) error {
	csvWriter := csv.NewWriter(writer)
	if err := csvWriter.Write([]string{"id", "date", "origin", "destination", "valid_from", "valid_to", "evidence_type", "occurrence_status", "time_source", "fare_product", "price_cents", "currency", "route_status", "route_provider", "route_confidence", "notes"}); err != nil {
		return err
	}
	for _, journey := range journeys {
		if err := csvWriter.Write([]string{spreadsheetSafe(journey.ID), spreadsheetSafe(journey.JourneyDate), spreadsheetSafe(journey.OriginName),
			spreadsheetSafe(journey.DestinationName), spreadsheetSafe(journey.ValidFrom), spreadsheetSafe(journey.ValidTo), spreadsheetSafe(journey.EvidenceType),
			spreadsheetSafe(journey.OccurrenceStatus), spreadsheetSafe(journey.TimeSource), spreadsheetSafe(journey.FareProduct),
			strconv.FormatInt(journey.PriceCents, 10), journey.Currency, journey.RouteStatus,
			spreadsheetSafe(journey.RouteProvider), strconv.FormatFloat(journey.RouteConfidence, 'f', -1, 64), spreadsheetSafe(journey.Notes)}); err != nil {
			return err
		}
	}
	csvWriter.Flush()
	return csvWriter.Error()
}

func spreadsheetSafe(value string) string {
	trimmed := strings.TrimLeft(value, " \t\r\n")
	if trimmed == "" || !strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return value
	}
	return "'" + value
}

func writeGeoJSON(writer io.Writer, journeys []model.Journey) error {
	features := make([]map[string]any, 0)
	for _, journey := range journeys {
		if len(journey.RouteGeoJSON) == 0 {
			continue
		}
		var collection struct {
			Features []map[string]any `json:"features"`
		}
		if json.Unmarshal(journey.RouteGeoJSON, &collection) != nil {
			continue
		}
		for _, feature := range collection.Features {
			properties, _ := feature["properties"].(map[string]any)
			if properties == nil {
				properties = make(map[string]any)
			}
			properties["journeyId"] = journey.ID
			properties["date"] = journey.JourneyDate
			properties["origin"] = journey.OriginName
			properties["destination"] = journey.DestinationName
			properties["evidenceType"] = journey.EvidenceType
			properties["occurrenceStatus"] = journey.OccurrenceStatus
			properties["routeProvider"] = journey.RouteProvider
			feature["properties"] = properties
			features = append(features, feature)
		}
	}
	return json.NewEncoder(writer).Encode(map[string]any{"type": "FeatureCollection", "features": features})
}

func writeJSONFile(path string, value any) error {
	file, err := openPrivateOutput(path)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func openPrivateOutput(path string) (*os.File, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("output path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create output directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open output: %w", err)
	}
	return file, nil
}

func copyFileAtomic(source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("create restore directory: %w", err)
	}
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open restore source: %w", err)
	}
	defer input.Close()
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".billett-restore-*")
	if err != nil {
		return fmt.Errorf("create restore temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := io.Copy(temporary, input); err != nil {
		temporary.Close()
		return fmt.Errorf("copy restore source: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync restored database: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, destination); err != nil {
		return fmt.Errorf("replace database: %w", err)
	}
	return nil
}

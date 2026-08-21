package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"billett/internal/httpapp"
	"billett/internal/ingest"
	"billett/internal/model"
	"billett/internal/routes"
	"billett/internal/store"
)

var version = "dev"

func main() {
	logger := log.New(os.Stderr, "billett: ", log.LstdFlags)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:], logger)
	case "import":
		err = importEmails(os.Args[2:])
	case "import-mbox":
		err = importMBOX(os.Args[2:])
	case "backup":
		err = backupDatabase(os.Args[2:])
	case "restore":
		err = restoreDatabase(os.Args[2:])
	case "export":
		err = exportJourneys(os.Args[2:])
	case "verify-database":
		err = verifyDatabase(os.Args[2:])
	case "smoke-route":
		err = smokeRoute(os.Args[2:])
	case "seed-demo":
		err = seedDemo(os.Args[2:])
	case "version":
		fmt.Println(version)
		return
	case "help", "-h", "--help":
		usage()
		return
	default:
		usage()
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		logger.Fatal(err)
	}
}

func serve(arguments []string, logger *log.Logger) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	address := flags.String("addr", envOr("BILLETT_ADDR", "127.0.0.1:8080"), "HTTP listen address")
	databasePath := flags.String("db", envOr("BILLETT_DB", "./data/billett.sqlite"), "SQLite database path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	database, err := store.Open(*databasePath)
	if err != nil {
		return err
	}
	defer database.Close()

	matcher := routes.NewMatcher(os.Getenv("GEOPS_API_KEY"))
	matcher.OSRMURL = envOr("OSRM_URL", matcher.OSRMURL)
	matcher.OverpassURL = envOr("OVERPASS_URL", matcher.OverpassURL)
	matcher.Cache = database
	authUsername := os.Getenv("BILLETT_AUTH_USERNAME")
	authPassword := os.Getenv("BILLETT_AUTH_PASSWORD")
	if err := validateListenSecurity(*address, authUsername, authPassword); err != nil {
		return err
	}
	app, err := httpapp.NewWithConfig(database, matcher, httpapp.Config{
		InboundToken:  os.Getenv("INBOUND_TOKEN"),
		AuthUsername:  authUsername,
		AuthPassword:  authPassword,
		RawArchiveDir: os.Getenv("BILLETT_RAW_ARCHIVE_DIR"),
		Version:       version,
	}, logger)
	if err != nil {
		return err
	}
	serviceContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if err := app.Start(serviceContext); err != nil {
		return err
	}
	server := &http.Server{
		Addr:              *address,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.ListenAndServe()
	}()
	logger.Printf("listening on http://%s", *address)
	logger.Printf("database %s", *databasePath)
	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-serviceContext.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return server.Shutdown(shutdownContext)
	}
}

func importEmails(arguments []string) error {
	flags := flag.NewFlagSet("import", flag.ContinueOnError)
	databasePath := flags.String("db", envOr("BILLETT_DB", "./data/billett.sqlite"), "SQLite database path")
	rawArchiveDir := flags.String("raw-archive", os.Getenv("BILLETT_RAW_ARCHIVE_DIR"), "optional private raw email archive directory")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() == 0 {
		return fmt.Errorf("provide at least one .eml file")
	}
	database, err := store.Open(*databasePath)
	if err != nil {
		return err
	}
	defer database.Close()

	service := &ingest.Service{Store: database, RawArchiveDir: *rawArchiveDir}
	report := service.ImportFiles(context.Background(), flags.Args())
	printBatchReport(report)
	if report.Failed > 0 {
		return fmt.Errorf("%d email(s) failed to import", report.Failed)
	}
	return nil
}

func smokeRoute(arguments []string) error {
	flags := flag.NewFlagSet("smoke-route", flag.ContinueOnError)
	from := flags.String("from", "Bern", "synthetic origin")
	to := flags.String("to", "Zürich HB", "synthetic destination")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	location, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		return err
	}
	start := time.Now().In(location).Add(15 * time.Minute).Truncate(time.Minute)
	journey := model.Journey{
		OriginName:      *from,
		DestinationName: *to,
		JourneyDate:     start.Format("2006-01-02"),
		ValidFrom:       start.Format(time.RFC3339),
		ValidTo:         start.Add(2 * time.Hour).Format(time.RFC3339),
	}
	matcher := routes.NewMatcher(os.Getenv("GEOPS_API_KEY"))
	matcher.OSRMURL = envOr("OSRM_URL", matcher.OSRMURL)
	matcher.OverpassURL = envOr("OVERPASS_URL", matcher.OverpassURL)
	update, err := matcher.Match(context.Background(), journey)
	if err != nil {
		return err
	}
	result := map[string]any{
		"status":     update.Status,
		"provider":   update.Provider,
		"confidence": update.Confidence,
		"summary":    update.Summary,
		"geometry":   json.RawMessage(update.GeoJSON),
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

func seedDemo(arguments []string) error {
	flags := flag.NewFlagSet("seed-demo", flag.ContinueOnError)
	databasePath := flags.String("db", envOr("BILLETT_DB", "./data/billett.sqlite"), "SQLite database path")
	from := flags.String("from", "Zürich", "demo origin")
	to := flags.String("to", "Bern", "demo destination")
	date := flags.String("date", "2026-08-08", "journey date")
	departureTime := flags.String("time", "08:46", "journey validity start")
	validity := flags.Duration("validity", 4*time.Hour, "journey validity duration")
	priceCHF := flags.Float64("price", 0, "synthetic ticket price in CHF")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	location, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		return err
	}
	start, err := time.ParseInLocation("2006-01-02 15:04", *date+" "+*departureTime, location)
	if err != nil {
		return fmt.Errorf("parse demo departure: %w", err)
	}
	seedKey := fmt.Sprintf("demo|%s|%s|%s", *from, *to, start.Format(time.RFC3339))
	seedHash := sha256.Sum256([]byte(seedKey))
	journey := model.Journey{
		SourceHash:      fmt.Sprintf("%x", seedHash),
		SourceFilename:  "demo-seed",
		MessageID:       fmt.Sprintf("<demo-%x@billett.local>", seedHash[:8]),
		EmailSubject:    fmt.Sprintf("Demo journey from %s to %s", *from, *to),
		Sender:          "billett demo seed",
		JourneyDate:     start.Format("2006-01-02"),
		OriginName:      *from,
		DestinationName: *to,
		ValidFrom:       start.Format(time.RFC3339),
		ValidTo:         start.Add(*validity).Format(time.RFC3339),
		TravelClass:     "2. Klasse",
		FareProduct:     "Demo journey",
		FareDetail:      "Synthetic route seed",
		PriceCents:      int64(math.Round(*priceCHF * 100)),
		Currency:        "CHF",
		RouteStatus:     "pending",
		PaymentMethod:   "Demo data",
		FareReduction:   "Synthetic data",
	}

	database, err := store.Open(*databasePath)
	if err != nil {
		return err
	}
	defer database.Close()
	stored, created, err := database.InsertJourney(context.Background(), journey)
	if err != nil {
		return err
	}
	matcher := routes.NewMatcher(os.Getenv("GEOPS_API_KEY"))
	matcher.OSRMURL = envOr("OSRM_URL", matcher.OSRMURL)
	matcher.OverpassURL = envOr("OVERPASS_URL", matcher.OverpassURL)
	matcher.Cache = database
	update, err := matcher.Match(context.Background(), stored)
	if err != nil {
		_, updateErr := database.UpdateRoute(context.Background(), stored.ID, model.RouteUpdate{
			Status:  "error",
			Summary: err.Error(),
		})
		if updateErr != nil {
			return fmt.Errorf("match demo route: %v; save failure: %w", err, updateErr)
		}
		return fmt.Errorf("demo journey was stored but route matching failed: %w", err)
	}
	stored, err = database.UpdateRoute(context.Background(), stored.ID, update)
	if err != nil {
		return err
	}
	state := "updated"
	if created {
		state = "seeded"
	}
	fmt.Printf("%s demo: %s → %s, %s, %s\n", state, stored.OriginName, stored.DestinationName, stored.RouteStatus, stored.RouteSummary)
	return nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage:
  billett serve [-addr 127.0.0.1:8080] [-db ./data/billett.sqlite]
  billett import [-db ./data/billett.sqlite] [--raw-archive DIR] receipt.eml [...]
  billett import-mbox [-db ./data/billett.sqlite] [--raw-archive DIR] [--report FILE] mail.mbox
  billett backup [-db ./data/billett.sqlite] --output backup.sqlite
  billett restore [-db ./data/billett.sqlite] --from backup.sqlite [--replace]
  billett export [-db ./data/billett.sqlite] --format json|csv|geojson --output FILE
  billett verify-database file.sqlite
  billett smoke-route [-from Bern] [-to "Zürich HB"]
  billett seed-demo [-from Zürich] [-to Bern] [-date 2026-08-08] [-time 08:46]
  billett version`)
}

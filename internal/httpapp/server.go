package httpapp

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"billett/internal/ingest"
	"billett/internal/model"
	"billett/internal/routes"
	"billett/internal/store"
)

//go:embed web/*
var webFiles embed.FS

type Server struct {
	store        *store.Store
	ingester     *ingest.Service
	matcher      routeMatcher
	inboundToken string
	authUsername string
	authPassword string
	version      string
	logger       *log.Logger
	web          http.Handler
	routeWake    chan struct{}
}

type Config struct {
	InboundToken  string
	AuthUsername  string
	AuthPassword  string
	RawArchiveDir string
	Version       string
}

type routeMatcher interface {
	MatchWithProgress(context.Context, model.Journey, routes.ProgressFunc) (model.RouteUpdate, error)
}

func New(store *store.Store, matcher routeMatcher, inboundToken string, logger *log.Logger) (*Server, error) {
	return NewWithConfig(store, matcher, Config{InboundToken: inboundToken}, logger)
}

func NewWithConfig(database *store.Store, matcher routeMatcher, config Config, logger *log.Logger) (*Server, error) {
	if (config.AuthUsername == "") != (config.AuthPassword == "") {
		return nil, fmt.Errorf("BILLETT_AUTH_USERNAME and BILLETT_AUTH_PASSWORD must be configured together")
	}
	if config.Version == "" {
		config.Version = "dev"
	}
	staticFiles, err := fs.Sub(webFiles, "web")
	if err != nil {
		return nil, fmt.Errorf("open embedded web files: %w", err)
	}
	return &Server{
		store:        database,
		ingester:     &ingest.Service{Store: database, RawArchiveDir: config.RawArchiveDir},
		matcher:      matcher,
		inboundToken: config.InboundToken,
		authUsername: config.AuthUsername,
		authPassword: config.AuthPassword,
		version:      config.Version,
		logger:       logger,
		web:          http.FileServer(http.FS(staticFiles)),
		routeWake:    make(chan struct{}, 1),
	}, nil
}

func (s *Server) Start(ctx context.Context) error {
	if err := s.store.RequeueIncompleteRoutes(ctx); err != nil {
		return err
	}
	go s.runRouteWorker(ctx)
	s.wakeRouteWorker()
	return nil
}

func (s *Server) Handler() http.Handler {
	public := http.NewServeMux()
	public.HandleFunc("GET /api/health", s.health)
	public.HandleFunc("POST /api/inbound-email", s.importInbound)
	protected := http.NewServeMux()
	protected.HandleFunc("GET /api/journeys", s.listJourneys)
	protected.HandleFunc("PATCH /api/journeys/{id}", s.patchJourney)
	protected.HandleFunc("DELETE /api/journeys/{id}", s.deleteJourney)
	protected.HandleFunc("POST /api/import", s.importUpload)
	protected.HandleFunc("GET /api/ingestions", s.listIngestions)
	protected.HandleFunc("POST /api/journeys/{id}/match-route", s.matchRoute)
	protected.HandleFunc("/", s.serveWeb)
	public.Handle("/", s.requireAuthentication(s.requireSameOrigin(protected)))
	return s.securityHeaders(s.logging(public))
}

func (s *Server) health(writer http.ResponseWriter, request *http.Request) {
	if err := s.store.Ping(request.Context()); err != nil {
		writeError(writer, http.StatusServiceUnavailable, err)
		return
	}
	schemaVersion, err := s.store.SchemaVersion(request.Context())
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"ok":                     true,
		"inboundEmailConfigured": s.inboundToken != "",
		"authenticationEnabled":  s.authUsername != "",
		"schemaVersion":          schemaVersion,
		"version":                s.version,
	})
}

func (s *Server) listJourneys(writer http.ResponseWriter, request *http.Request) {
	journeys, err := s.store.ListJourneys(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"journeys": journeys})
}

func (s *Server) importUpload(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, ingest.MaxMessageBytes+(1<<20))
	if err := request.ParseMultipartForm(ingest.MaxMessageBytes); err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("read upload: %w", err))
		return
	}
	file, header, err := request.FormFile("email")
	if err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("email file is required"))
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(file)
	if err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("read email file: %w", err))
		return
	}
	s.ingest(writer, request, raw, path.Base(header.Filename), "browser")
}

func (s *Server) importInbound(writer http.ResponseWriter, request *http.Request) {
	if s.inboundToken == "" {
		writeError(writer, http.StatusServiceUnavailable, fmt.Errorf("INBOUND_TOKEN is not configured"))
		return
	}
	expected := "Bearer " + s.inboundToken
	provided := request.Header.Get("Authorization")
	if len(expected) != len(provided) || subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) != 1 {
		writeError(writer, http.StatusUnauthorized, fmt.Errorf("invalid inbound token"))
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, ingest.MaxMessageBytes)
	raw, err := io.ReadAll(request.Body)
	if err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("read inbound email: %w", err))
		return
	}
	filename := request.Header.Get("X-Email-Filename")
	if filename == "" {
		filename = "inbound.eml"
	}
	s.ingest(writer, request, raw, path.Base(filename), "inbound")
}

func (s *Server) ingest(writer http.ResponseWriter, request *http.Request, raw []byte, filename, sourceKind string) {
	result, err := s.ingester.Import(request.Context(), raw, ingest.Source{Kind: sourceKind, Name: filename})
	if err != nil {
		writeError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	status := http.StatusOK
	storedJourneys := result.Journeys
	if result.Created > 0 {
		status = http.StatusCreated
		for index, journey := range storedJourneys {
			if journey.RouteStatus != "pending" {
				continue
			}
			queued, queueErr := s.store.QueueRoute(request.Context(), journey.ID)
			if queueErr != nil {
				writeError(writer, http.StatusInternalServerError, queueErr)
				return
			}
			storedJourneys[index] = queued
		}
	}
	if result.Created > 0 {
		s.wakeRouteWorker()
	}
	if len(storedJourneys) == 0 {
		writeError(writer, http.StatusUnprocessableEntity, fmt.Errorf("email contains no journeys"))
		return
	}
	writeJSON(writer, status, map[string]any{
		"created":      result.Created > 0,
		"createdCount": result.Created,
		"journey":      storedJourneys[0],
		"journeys":     storedJourneys,
		"ingestion":    result.Ingestion,
	})
}

func (s *Server) listIngestions(writer http.ResponseWriter, request *http.Request) {
	items, err := s.store.ListIngestions(request.Context(), 100)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	summary, err := s.store.IngestionSummary(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"ingestions": items, "summary": summary})
}

func (s *Server) patchJourney(writer http.ResponseWriter, request *http.Request) {
	var patch model.JourneyPatch
	decoder := json.NewDecoder(io.LimitReader(request.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&patch); err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("decode journey update: %w", err))
		return
	}
	journey, err := s.store.PatchJourney(request.Context(), request.PathValue("id"), patch)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	s.wakeRouteWorker()
	writeJSON(writer, http.StatusOK, map[string]any{"journey": journey})
}

func (s *Server) deleteJourney(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-Billett-Confirm-Delete") != "delete" {
		writeError(writer, http.StatusPreconditionFailed, fmt.Errorf("set X-Billett-Confirm-Delete to delete"))
		return
	}
	if err := s.store.DeleteJourney(request.Context(), request.PathValue("id")); err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) matchRoute(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	if _, err := s.store.GetJourney(request.Context(), id); err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	journey, err := s.store.QueueRoute(request.Context(), id)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	s.wakeRouteWorker()
	writeJSON(writer, http.StatusAccepted, map[string]any{"journey": journey})
}

func (s *Server) wakeRouteWorker() {
	select {
	case s.routeWake <- struct{}{}:
	default:
	}
}

func (s *Server) runRouteWorker(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		journey, found, err := s.store.ClaimNextRoute(ctx)
		if err != nil {
			s.logger.Printf("claim route job: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-s.routeWake:
			case <-time.After(time.Second):
			}
			continue
		}
		if !found {
			select {
			case <-ctx.Done():
				return
			case <-s.routeWake:
			}
			continue
		}

		update, matchErr := s.matcher.MatchWithProgress(ctx, journey, func(percent int, message string) {
			if err := s.store.UpdateRouteProgress(ctx, journey.ID, percent, message); err != nil && ctx.Err() == nil {
				s.logger.Printf("save route progress for %s: %v", journey.ID, err)
			}
		})
		if matchErr != nil {
			update = failedRouteUpdate(matchErr)
		}
		if _, err := s.store.UpdateRoute(ctx, journey.ID, update); err != nil && ctx.Err() == nil {
			s.logger.Printf("save route result for %s: %v", journey.ID, err)
		}
	}
}

func failedRouteUpdate(routeErr error) model.RouteUpdate {
	return model.RouteUpdate{
		Status:  "error",
		Summary: routeErr.Error(),
	}
}

func (s *Server) serveWeb(writer http.ResponseWriter, request *http.Request) {
	if strings.HasPrefix(request.URL.Path, "/api/") {
		writeError(writer, http.StatusNotFound, fmt.Errorf("not found"))
		return
	}
	s.web.ServeHTTP(writer, request)
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		s.logger.Printf("%s %s", request.Method, request.URL.Path)
		next.ServeHTTP(writer, request)
	})
}

func (s *Server) requireAuthentication(next http.Handler) http.Handler {
	if s.authUsername == "" {
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		usernameOK := subtle.ConstantTimeCompare([]byte(username), []byte(s.authUsername)) == 1
		passwordOK := subtle.ConstantTimeCompare([]byte(password), []byte(s.authPassword)) == 1
		if !ok || !usernameOK || !passwordOK {
			writer.Header().Set("WWW-Authenticate", `Basic realm="billett", charset="UTF-8"`)
			writeError(writer, http.StatusUnauthorized, fmt.Errorf("authentication required"))
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (s *Server) requireSameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet || request.Method == http.MethodHead || request.Method == http.MethodOptions {
			next.ServeHTTP(writer, request)
			return
		}
		if fetchSite := request.Header.Get("Sec-Fetch-Site"); fetchSite != "" && fetchSite != "same-origin" && fetchSite != "none" {
			writeError(writer, http.StatusForbidden, fmt.Errorf("cross-origin mutation rejected"))
			return
		}
		if origin := request.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || !strings.EqualFold(parsed.Host, request.Host) {
				writeError(writer, http.StatusForbidden, fmt.Errorf("cross-origin mutation rejected"))
				return
			}
		}
		next.ServeHTTP(writer, request)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		writer.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		writer.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		writer.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		writer.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' https://unpkg.com; style-src 'self' https://unpkg.com; img-src 'self' data: https://tile.openstreetmap.org; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if strings.HasPrefix(request.URL.Path, "/api/") {
			writer.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(writer, request)
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		return
	}
}

func writeError(writer http.ResponseWriter, status int, err error) {
	writeJSON(writer, status, map[string]any{"error": err.Error()})
}

func Shutdown(ctx context.Context, server *http.Server) error {
	return server.Shutdown(ctx)
}

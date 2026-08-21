package httpapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"billett/internal/model"
	"billett/internal/routes"
	"billett/internal/store"
)

func TestUploadListsJourneyAndDeduplicates(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "billett.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	app, err := New(database, routes.NewMatcher(""), "", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	handler := app.Handler()
	raw, err := os.ReadFile(filepath.Join("..", "emailparse", "testdata", "easyride.eml"))
	if err != nil {
		t.Fatal(err)
	}

	first := uploadEmail(t, handler, raw)
	if first.Code != http.StatusCreated {
		t.Fatalf("first upload: got %d, want %d: %s", first.Code, http.StatusCreated, first.Body.String())
	}
	second := uploadEmail(t, handler, raw)
	if second.Code != http.StatusOK {
		t.Fatalf("duplicate upload: got %d, want %d: %s", second.Code, http.StatusOK, second.Body.String())
	}
	var duplicate struct {
		Created bool `json:"created"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &duplicate); err != nil {
		t.Fatal(err)
	}
	if duplicate.Created {
		t.Fatal("duplicate upload was reported as created")
	}

	request := httptest.NewRequest(http.MethodGet, "/api/journeys", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("list journeys: got %d: %s", response.Code, response.Body.String())
	}
	var listed struct {
		Journeys []struct {
			Origin      string `json:"originName"`
			Destination string `json:"destinationName"`
			PriceCents  int64  `json:"priceCents"`
		} `json:"journeys"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Journeys) != 1 {
		t.Fatalf("journey count: got %d, want 1", len(listed.Journeys))
	}
	journey := listed.Journeys[0]
	if journey.Origin != "Zürich HB" || journey.Destination != "Bern" || journey.PriceCents != 2550 {
		t.Fatalf("unexpected journey: %+v", journey)
	}
}

func TestUploadStoresEveryOnlinePurchaseTicket(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "billett.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	app, err := New(database, routes.NewMatcher(""), "", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "emailparse", "testdata", "online_purchase.eml"))
	if err != nil {
		t.Fatal(err)
	}
	secondTicket := strings.Join([]string{
		"++ Abschnitt 3: Billett 2",
		"Example Passenger",
		"01.01.1990",
		"Streckenbillett",
		"Interlaken Ost",
		"Bern",
		"2. Klasse, einfache Fahrt, Halbtax-Abo",
		"CHF 15.00",
		"Gültig:",
		"03.08.2026 00:00 -",
		"04.08.2026 05:00",
		"[Ende Abschnitt 3]",
		"++ Abschnitt 4:",
	}, "\n")
	raw = []byte(strings.Replace(string(raw), "++ Abschnitt 3:\n", secondTicket+"\n", 1))

	response := uploadEmail(t, app.Handler(), raw)
	if response.Code != http.StatusCreated {
		t.Fatalf("upload: got %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}
	var imported struct {
		CreatedCount int             `json:"createdCount"`
		Journeys     []model.Journey `json:"journeys"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &imported); err != nil {
		t.Fatal(err)
	}
	if imported.CreatedCount != 2 || len(imported.Journeys) != 2 {
		t.Fatalf("imported tickets: got %d created and %d returned, want 2", imported.CreatedCount, len(imported.Journeys))
	}

	duplicate := uploadEmail(t, app.Handler(), raw)
	if duplicate.Code != http.StatusOK {
		t.Fatalf("duplicate: got %d, want %d: %s", duplicate.Code, http.StatusOK, duplicate.Body.String())
	}
}

func TestImportReturnsWhileRouteMatchesInBackground(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "billett.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	matcher := &blockingRouteMatcher{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	app, err := New(database, matcher, "", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	workerContext, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	if err := app.Start(workerContext); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "emailparse", "testdata", "easyride.eml"))
	if err != nil {
		t.Fatal(err)
	}

	response := uploadEmail(t, app.Handler(), raw)
	if response.Code != http.StatusCreated {
		t.Fatalf("upload: got %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}
	var imported struct {
		Journey model.Journey `json:"journey"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &imported); err != nil {
		t.Fatal(err)
	}
	if imported.Journey.RouteStatus != "queued" && imported.Journey.RouteStatus != "matching" {
		t.Fatalf("imported route status: got %q, want queued or matching", imported.Journey.RouteStatus)
	}

	select {
	case <-matcher.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background matcher did not start")
	}
	request := httptest.NewRequest(http.MethodGet, "/api/journeys", nil)
	listedResponse := httptest.NewRecorder()
	app.Handler().ServeHTTP(listedResponse, request)
	var listed struct {
		Journeys []model.Journey `json:"journeys"`
	}
	if err := json.Unmarshal(listedResponse.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Journeys) != 1 {
		t.Fatalf("listed journeys: got %d, want 1", len(listed.Journeys))
	}
	if listed.Journeys[0].RouteStatus != "matching" || listed.Journeys[0].RouteProgress != 55 {
		t.Fatalf("visible progress: got status %q at %d%%", listed.Journeys[0].RouteStatus, listed.Journeys[0].RouteProgress)
	}
	if listed.Journeys[0].RouteProgressMessage != "Tracing test corridor" {
		t.Fatalf("progress message: got %q", listed.Journeys[0].RouteProgressMessage)
	}

	close(matcher.release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		journey, err := database.GetJourney(context.Background(), imported.Journey.ID)
		if err != nil {
			t.Fatal(err)
		}
		if journey.RouteStatus == "matched" {
			if journey.RouteProgress != 100 {
				t.Fatalf("completed progress: got %d, want 100", journey.RouteProgress)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("route did not complete: last status %q", journey.RouteStatus)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type blockingRouteMatcher struct {
	started chan struct{}
	release chan struct{}
}

func (m *blockingRouteMatcher) MatchWithProgress(
	ctx context.Context,
	_ model.Journey,
	progress routes.ProgressFunc,
) (model.RouteUpdate, error) {
	progress(55, "Tracing test corridor")
	close(m.started)
	select {
	case <-ctx.Done():
		return model.RouteUpdate{}, ctx.Err()
	case <-m.release:
	}
	return model.RouteUpdate{
		Status:   "matched",
		Provider: "test network",
		Summary:  "test route",
		GeoJSON:  json.RawMessage(`{"type":"FeatureCollection","features":[]}`),
	}, nil
}

func TestHealthReportsInboundEmailConfiguration(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "billett.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	for _, test := range []struct {
		name       string
		token      string
		configured bool
	}{
		{name: "disabled", configured: false},
		{name: "enabled", token: "test-token", configured: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, err := New(database, routes.NewMatcher(""), test.token, log.New(io.Discard, "", 0))
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
			response := httptest.NewRecorder()
			app.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("got %d, want %d", response.Code, http.StatusOK)
			}
			var result struct {
				OK                     bool `json:"ok"`
				InboundEmailConfigured bool `json:"inboundEmailConfigured"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if !result.OK || result.InboundEmailConfigured != test.configured {
				t.Fatalf("unexpected health response: %+v", result)
			}
		})
	}
}

func TestInboundEndpointRequiresConfiguredToken(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "billett.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	app, err := New(database, routes.NewMatcher(""), "", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/inbound-email", bytes.NewBufferString("email"))
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func TestInboundEndpointAcceptsRawEmailWithToken(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "billett.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	app, err := New(database, routes.NewMatcher(""), "test-token", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "emailparse", "testdata", "easyride.eml"))
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/inbound-email", bytes.NewReader(raw))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("got %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}
}

func TestBasicAuthenticationProtectsDashboardButNotHealthOrInbound(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "billett.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	app, err := NewWithConfig(database, routes.NewMatcher(""), Config{
		InboundToken: "inbound-token", AuthUsername: "owner", AuthPassword: "strong-password",
	}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	unauthenticated := httptest.NewRecorder()
	app.Handler().ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/api/journeys", nil))
	if unauthenticated.Code != http.StatusUnauthorized || unauthenticated.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("unauthenticated response: code=%d headers=%v", unauthenticated.Code, unauthenticated.Header())
	}
	authenticatedRequest := httptest.NewRequest(http.MethodGet, "/api/journeys", nil)
	authenticatedRequest.SetBasicAuth("owner", "strong-password")
	authenticated := httptest.NewRecorder()
	app.Handler().ServeHTTP(authenticated, authenticatedRequest)
	if authenticated.Code != http.StatusOK {
		t.Fatalf("authenticated response: got %d, want 200", authenticated.Code)
	}
	health := httptest.NewRecorder()
	app.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health response: got %d, want 200", health.Code)
	}
}

func TestCrossOriginMutationIsRejected(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "billett.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	app, err := NewWithConfig(database, routes.NewMatcher(""), Config{
		AuthUsername: "owner", AuthPassword: "strong-password",
	}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "https://journeys.example.com/api/import", nil)
	request.Host = "journeys.example.com"
	request.Header.Set("Origin", "https://attacker.example")
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	request.SetBasicAuth("owner", "strong-password")
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("got %d, want %d: %s", response.Code, http.StatusForbidden, response.Body.String())
	}
}

func uploadEmail(t *testing.T, handler http.Handler, raw []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("email", "easyride.eml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/import", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

package routes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"billett/internal/model"
)

func TestMatcherSelectsCandidateAndBuildsStopSequence(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Query().Get("from") != "Zürich HB" {
			t.Fatalf("unexpected origin %q", request.URL.Query().Get("from"))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(testConnectionResponse)),
			Request:    request,
		}, nil
	})}

	matcher := NewMatcher("")
	matcher.Client = client
	matcher.OSRMURL = ""
	journey := model.Journey{
		OriginName:      "Zürich HB",
		DestinationName: "Bern",
		ValidFrom:       "2026-07-01T10:17:00+02:00",
		ValidTo:         "2026-07-01T10:47:00+02:00",
	}
	update, err := matcher.Match(context.Background(), journey)
	if err != nil {
		t.Fatal(err)
	}
	if update.Status != "matched" {
		t.Fatalf("status: got %q, want matched", update.Status)
	}
	if update.Provider != "transport.opendata.ch stop sequence" {
		t.Fatalf("provider: got %q", update.Provider)
	}
	var geometry featureCollection
	if err := json.Unmarshal(update.GeoJSON, &geometry); err != nil {
		t.Fatal(err)
	}
	if len(geometry.Features) != 1 {
		t.Fatalf("features: got %d, want 1", len(geometry.Features))
	}
	var coordinates [][]float64
	if err := json.Unmarshal(geometry.Features[0].Geometry.Coordinates, &coordinates); err != nil {
		t.Fatal(err)
	}
	if len(coordinates) != 3 {
		t.Fatalf("coordinates: got %d, want 3", len(coordinates))
	}
}

func TestMatcherUsesOSRMRoadGeometry(t *testing.T) {
	osrmRequests := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body string
		switch request.URL.Host {
		case "transport.test":
			body = testConnectionResponse
		case "osrm.test":
			osrmRequests++
			if !strings.Contains(request.URL.Path, "/route/v1/driving/") {
				t.Fatalf("unexpected OSRM path %q", request.URL.Path)
			}
			if request.URL.Query().Get("overview") != "full" || request.URL.Query().Get("geometries") != "geojson" {
				t.Fatalf("OSRM did not request full GeoJSON geometry")
			}
			body = `{"code":"Ok","waypoints":[{"distance":8},{"distance":5},{"distance":9}],"routes":[{"geometry":{"type":"LineString","coordinates":[[8.540,47.378],[8.4300,47.3600],[8.100,47.250],[7.700,47.050],[7.439,46.949]]}}]}`
		default:
			t.Fatalf("unexpected host %q", request.URL.Host)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}

	matcher := NewMatcher("")
	matcher.Client = client
	matcher.TransportURL = "https://transport.test/connections"
	matcher.OSRMURL = "https://osrm.test"
	matcher.Cache = &memoryGeometryCache{values: make(map[string]json.RawMessage)}
	journey := model.Journey{
		OriginName:      "Zürich HB",
		DestinationName: "Bern",
		ValidFrom:       "2026-07-01T10:17:00+02:00",
		ValidTo:         "2026-07-01T10:47:00+02:00",
	}
	update, err := matcher.Match(context.Background(), journey)
	if err != nil {
		t.Fatal(err)
	}
	if update.Provider != "transport.opendata.ch + OpenStreetMap routing" {
		t.Fatalf("provider: got %q", update.Provider)
	}
	if !strings.Contains(string(update.GeoJSON), `"source":"OpenStreetMap road routing"`) {
		t.Fatalf("geometry did not include OSRM provenance: %s", update.GeoJSON)
	}
	var geometry featureCollection
	if err := json.Unmarshal(update.GeoJSON, &geometry); err != nil {
		t.Fatal(err)
	}
	var coordinates [][]float64
	if err := json.Unmarshal(geometry.Features[0].Geometry.Coordinates, &coordinates); err != nil {
		t.Fatal(err)
	}
	if len(coordinates) != 5 {
		t.Fatalf("coordinates: got %d, want 5", len(coordinates))
	}
	second, err := matcher.Match(context.Background(), journey)
	if err != nil {
		t.Fatal(err)
	}
	if osrmRequests != 1 {
		t.Fatalf("OSRM requests: got %d, want 1 after repeated corridor", osrmRequests)
	}
	if second.CacheHits != 1 {
		t.Fatalf("second match cache hits: got %d, want 1", second.CacheHits)
	}
}

func TestMatcherUsesGeOpsGeometryWithPersonalKey(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body string
		switch request.URL.Host {
		case "transport.test":
			body = testConnectionResponse
		case "geops.test":
			if request.URL.Query().Get("key") != "personal-test-key" {
				t.Fatalf("unexpected geOps key")
			}
			if request.URL.Query().Get("mot") != "bus" {
				t.Fatalf("unexpected geOps mode %q", request.URL.Query().Get("mot"))
			}
			body = `{"type":"FeatureCollection","features":[{"type":"Feature","properties":{"length":900},"geometry":{"type":"LineString","coordinates":[[8.540,47.378],[8.200,47.300],[7.439,46.949]]}}]}`
		default:
			t.Fatalf("unexpected host %q", request.URL.Host)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}

	matcher := NewMatcher("personal-test-key")
	matcher.Client = client
	matcher.TransportURL = "https://transport.test/connections"
	matcher.GeOpsURL = "https://geops.test/routing"
	update, err := matcher.Match(context.Background(), model.Journey{
		OriginName:      "Zürich HB",
		DestinationName: "Bern",
		ValidFrom:       "2026-07-01T10:17:00+02:00",
		ValidTo:         "2026-07-01T10:47:00+02:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	if update.Provider != "transport.opendata.ch + geOps routing" {
		t.Fatalf("provider: got %q", update.Provider)
	}
	if !strings.Contains(string(update.GeoJSON), `"source":"geOps routing"`) {
		t.Fatalf("geometry did not include geOps provenance: %s", update.GeoJSON)
	}
}

func TestMatcherUsesMiddayForTicketValidityWindow(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.URL.Query().Get("time"); got != "12:00" {
			t.Fatalf("ticket lookup time: got %q, want 12:00", got)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(testConnectionResponse)), Request: request}, nil
	})}
	matcher := NewMatcher("")
	matcher.Client = client
	matcher.OSRMURL = ""
	update, err := matcher.Match(context.Background(), model.Journey{
		OriginName: "Zürich HB", DestinationName: "Bern",
		ValidFrom: "2026-07-01T00:00:00+02:00", ValidTo: "2026-07-02T05:00:00+02:00",
		TimeSource: "validity_window",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !update.TimeEstimated {
		t.Fatal("ticket route time was not marked estimated")
	}
}

func TestMatcherConvertsStoredUTCToSwissLocalTime(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.URL.Query().Get("time"); got != "10:17" {
			t.Fatalf("Swiss lookup time: got %q, want 10:17", got)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(testConnectionResponse)), Request: request}, nil
	})}
	matcher := NewMatcher("")
	matcher.Client = client
	matcher.OSRMURL = ""
	update, err := matcher.Match(context.Background(), model.Journey{
		OriginName: "Zürich HB", DestinationName: "Bern",
		ValidFrom: "2026-07-01T08:17:00Z", ValidTo: "2026-07-01T08:47:00Z",
		TimeSource: "user_supplied",
	})
	if err != nil {
		t.Fatal(err)
	}
	if update.DataVersion != "transport.opendata.ch:2026-07-01" {
		t.Fatalf("data version: got %q", update.DataVersion)
	}
}

func TestMatcherFallsBackToCurrentTopologyForOldTimetable(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		body := `{"connections":[]}`
		if requests == 2 {
			body = testConnectionResponse
			if request.URL.Query().Get("date") == "2024-07-01" {
				t.Fatal("fallback reused the unavailable historical date")
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	matcher := NewMatcher("")
	matcher.Client = client
	matcher.OSRMURL = ""
	update, err := matcher.Match(context.Background(), model.Journey{
		OriginName: "Zürich HB", DestinationName: "Bern",
		ValidFrom: "2024-07-01T10:17:00+02:00", ValidTo: "2024-07-01T10:47:00+02:00",
		TimeSource: "recorded",
	})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || update.Status != "estimated" || !update.TimeEstimated || !strings.HasPrefix(update.DataVersion, "current-timetable:") {
		t.Fatalf("historical fallback: requests=%d update=%+v", requests, update)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type memoryGeometryCache struct {
	values map[string]json.RawMessage
}

func (cache *memoryGeometryCache) GetRouteGeometry(_ context.Context, key string) (json.RawMessage, bool, error) {
	value, found := cache.values[key]
	return append(json.RawMessage(nil), value...), found, nil
}

func (cache *memoryGeometryCache) PutRouteGeometry(_ context.Context, key string, value json.RawMessage, _ time.Duration) error {
	cache.values[key] = append(json.RawMessage(nil), value...)
	return nil
}

const testConnectionResponse = `{
  "connections": [{
    "from": {
      "departure": "2026-07-01T10:18:00+02:00",
      "station": {"name": "Zürich HB", "coordinate": {"x": 47.378, "y": 8.540}}
    },
    "to": {
      "arrival": "2026-07-01T10:25:00+02:00",
      "station": {"name": "Bern", "coordinate": {"x": 46.949, "y": 7.439}}
    },
    "duration": "00d00:07:00",
    "products": ["Bus 2"],
    "sections": [{
      "departure": {
        "departure": "2026-07-01T10:18:00+02:00",
        "station": {"name": "Zürich HB", "coordinate": {"x": 47.378, "y": 8.540}}
      },
      "arrival": {
        "arrival": "2026-07-01T10:25:00+02:00",
        "station": {"name": "Bern", "coordinate": {"x": 46.949, "y": 7.439}}
      },
      "journey": {
        "name": "Bus 2",
        "category": "Bus",
        "number": "2",
        "operator": "PostAuto",
        "to": "Olten Hammer",
        "passList": [{
          "station": {"name": "Olten", "coordinate": {"x": "47.352", "y": "7.908"}}
        }]
      }
    }]
  }, {
    "from": {"departure": "2026-07-01T11:18:00+02:00", "station": {}},
    "to": {"arrival": "2026-07-01T11:25:00+02:00", "station": {}},
    "sections": []
  }]
}`

package routes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRailGraphRoutesThroughTrackNodes(t *testing.T) {
	var payload struct {
		Elements []osmWay `json:"elements"`
	}
	if err := json.Unmarshal([]byte(`{
		"elements": [
			{"type":"way","id":1,"nodes":[1,2,3],"tags":{"railway":"rail"},"geometry":[{"lat":47.0,"lon":8.0},{"lat":47.0,"lon":8.01},{"lat":47.0,"lon":8.02}]},
			{"type":"way","id":2,"nodes":[2,4,5],"tags":{"railway":"rail"},"geometry":[{"lat":47.0,"lon":8.01},{"lat":47.01,"lon":8.01},{"lat":47.01,"lon":8.02}]}
		]
	}`), &payload); err != nil {
		t.Fatal(err)
	}
	graph := buildRailGraph(payload.Elements, "rail")
	coordinates, err := graph.routeThrough([][]float64{{8.0, 47.0}, {8.02, 47.01}})
	if err != nil {
		t.Fatal(err)
	}
	if len(coordinates) != 4 {
		t.Fatalf("coordinates: got %d, want 4", len(coordinates))
	}
	if coordinates[1][0] != 8.01 || coordinates[1][1] != 47.0 {
		t.Fatalf("path did not use the connected junction: %v", coordinates)
	}
}

func TestOverpassRailGeometryUsesCacheForRepeatedLeg(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"elements": [{
					"type":"way",
					"id":1,
					"nodes":[1,2,3],
					"tags":{"railway":"rail"},
					"geometry":[
						{"lat":47.0,"lon":8.0},
						{"lat":47.0,"lon":8.01},
						{"lat":47.0,"lon":8.02}
					]
				}]
			}`)),
			Request: request,
		}, nil
	})}
	matcher := NewMatcher("")
	matcher.Client = client
	matcher.OverpassURL = "https://overpass.test/interpreter"
	matcher.Cache = &memoryGeometryCache{values: make(map[string]json.RawMessage)}

	first, firstHit, err := matcher.fetchOverpassRailPart(context.Background(), []float64{8.0, 47.0}, []float64{8.02, 47.0}, "rail")
	if err != nil {
		t.Fatal(err)
	}
	second, secondHit, err := matcher.fetchOverpassRailPart(context.Background(), []float64{8.0, 47.0}, []float64{8.02, 47.0}, "rail")
	if err != nil {
		t.Fatal(err)
	}
	if firstHit || !secondHit {
		t.Fatalf("cache hits: first=%v second=%v", firstHit, secondHit)
	}
	if requests != 1 {
		t.Fatalf("Overpass requests: got %d, want 1", requests)
	}
	if len(first) != 3 || len(second) != 3 {
		t.Fatalf("cached rail coordinates: first=%d second=%d", len(first), len(second))
	}
}

func TestNetworkModeRecognizesSwissCategories(t *testing.T) {
	tests := map[string]string{
		"B":    "road",
		"NFB":  "road",
		"Bus":  "road",
		"T":    "tram",
		"IC":   "rail",
		"S":    "rail",
		"walk": "",
		"BAT":  "",
	}
	for input, expected := range tests {
		if actual := networkMode(input); actual != expected {
			t.Errorf("networkMode(%q): got %q, want %q", input, actual, expected)
		}
	}
}

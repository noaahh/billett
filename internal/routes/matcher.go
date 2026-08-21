package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"billett/internal/model"
)

const defaultTransportURL = "https://transport.opendata.ch/v1/connections"
const defaultGeOpsURL = "https://api.geops.io/routing/v1/"
const defaultOSRMURL = "https://router.project-osrm.org"
const defaultOverpassURL = "https://overpass.osm.ch/api/interpreter"

type Matcher struct {
	Client          *http.Client
	TransportURL    string
	GeOpsURL        string
	GeOpsAPIKey     string
	OSRMURL         string
	OverpassURL     string
	UserAgent       string
	NumberOfResults int
	Cache           GeometryCache
}

type GeometryCache interface {
	GetRouteGeometry(context.Context, string) (json.RawMessage, bool, error)
	PutRouteGeometry(context.Context, string, json.RawMessage, time.Duration) error
}

type ComparableRouteStore interface {
	FindComparableRoute(context.Context, string, string) (model.RouteUpdate, bool, error)
}

type ProgressFunc func(percent int, message string)

func NewMatcher(geOpsAPIKey string) *Matcher {
	return &Matcher{
		Client:          &http.Client{Timeout: 20 * time.Second},
		TransportURL:    defaultTransportURL,
		GeOpsURL:        defaultGeOpsURL,
		GeOpsAPIKey:     geOpsAPIKey,
		OSRMURL:         defaultOSRMURL,
		OverpassURL:     defaultOverpassURL,
		UserAgent:       "billett/1.0 (+https://github.com/noaahh/billett)",
		NumberOfResults: 6,
	}
}

func (m *Matcher) Match(ctx context.Context, journey model.Journey) (model.RouteUpdate, error) {
	return m.MatchWithProgress(ctx, journey, nil)
}

func (m *Matcher) MatchWithProgress(ctx context.Context, journey model.Journey, progress ProgressFunc) (model.RouteUpdate, error) {
	reportProgress(progress, 10, "Reading journey times")
	validFrom, err := time.Parse(time.RFC3339, journey.ValidFrom)
	if err != nil {
		return model.RouteUpdate{}, fmt.Errorf("parse journey validity start: %w", err)
	}
	validTo, err := time.Parse(time.RFC3339, journey.ValidTo)
	if err != nil {
		return model.RouteUpdate{}, fmt.Errorf("parse journey validity end: %w", err)
	}
	location, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		return model.RouteUpdate{}, fmt.Errorf("load Swiss time zone: %w", err)
	}
	validFrom = validFrom.In(location)
	validTo = validTo.In(location)

	matchTime := validFrom
	timeEstimated := journey.TimeSource == "validity_window" || journey.TimeSource == "timetable_estimate"
	if timeEstimated {
		location := validFrom.Location()
		matchTime = time.Date(validFrom.Year(), validFrom.Month(), validFrom.Day(), 12, 0, 0, 0, location)
	}
	reportProgress(progress, 15, "Finding timetable connections")
	connections, err := m.fetchConnections(ctx, journey, matchTime)
	historicalApproximation := false
	dataVersion := "transport.opendata.ch:" + matchTime.Format("2006-01-02")
	if err != nil && matchTime.Before(time.Now().AddDate(0, -3, 0)) {
		if comparableStore, ok := m.Cache.(ComparableRouteStore); ok {
			if comparable, found, comparableErr := comparableStore.FindComparableRoute(ctx, journey.OriginName, journey.DestinationName); comparableErr == nil && found {
				comparable.Status = "estimated"
				comparable.Provider = "local corridor history"
				comparable.Confidence = math.Min(0.65, comparable.Confidence)
				comparable.Summary = "Approximate route reused from another stored journey on the same corridor"
				comparable.ExternalDataNotice = "No external request was needed. Geometry was reused from the local journey history."
				comparable.TimeEstimated = true
				return comparable, nil
			}
		}
		fallbackTime := currentTimetableReference(matchTime, time.Now())
		connections, err = m.fetchConnections(ctx, journey, fallbackTime)
		if err == nil {
			matchTime = fallbackTime
			validTo = fallbackTime.Add(8 * time.Hour)
			historicalApproximation = true
			timeEstimated = true
			dataVersion = "current-timetable:" + fallbackTime.Format("2006-01-02")
		}
	}
	if err != nil {
		return model.RouteUpdate{}, err
	}
	reportProgress(progress, 30, "Selecting the closest connection")
	selected, confidence, ambiguous, err := selectConnection(connections, matchTime, validTo)
	if err != nil {
		return model.RouteUpdate{}, err
	}

	geoJSON, summary := connectionGeoJSON(selected)
	provider := "transport.opendata.ch stop sequence"
	cacheHits := 0
	if m.GeOpsAPIKey != "" {
		reportProgress(progress, 40, "Tracing scheduled sections with geOps")
		if geOpsGeoJSON, geOpsErr := m.fetchGeOpsGeometry(ctx, selected); geOpsErr == nil && len(geOpsGeoJSON.Features) > 0 {
			geoJSON = geOpsGeoJSON
			provider = "transport.opendata.ch + geOps routing"
		}
	} else {
		reportProgress(progress, 40, "Tracing scheduled sections on the transport network")
		networkGeoJSON, refinedSections, hits, networkErr := m.fetchOpenNetworkGeometry(ctx, selected, func(completed, total, sectionCacheHits int) {
			cacheHits = sectionCacheHits
			percent := 40
			if total > 0 {
				percent += int(math.Round(float64(completed) / float64(total) * 50))
			}
			message := fmt.Sprintf("Traced section %d of %d", completed, total)
			if sectionCacheHits > 0 {
				message += fmt.Sprintf(" using %d cached segment(s)", sectionCacheHits)
			}
			reportProgress(progress, percent, message)
		})
		cacheHits = hits
		if len(networkGeoJSON.Features) > 0 {
			geoJSON = networkGeoJSON
		}
		if networkErr == nil && refinedSections > 0 {
			provider = "transport.opendata.ch + OpenStreetMap routing"
		}
	}

	status := "matched"
	if ambiguous {
		status = "ambiguous"
	}
	if historicalApproximation {
		status = "estimated"
		summary = "Approximate historical route using current timetable topology"
		provider = "current timetable topology approximation + " + provider
	}
	encoded, err := json.Marshal(geoJSON)
	if err != nil {
		return model.RouteUpdate{}, fmt.Errorf("encode route geometry: %w", err)
	}
	notice := "Matched using origin, destination, journey date, and validity time sent to transport.opendata.ch."
	if timeEstimated {
		notice += " The ticket did not contain an observed departure time, so the selected connection is a timetable estimate."
	}
	if historicalApproximation {
		notice += " The original timetable was unavailable, so route topology came from the current timetable and may differ historically."
	}
	if m.GeOpsAPIKey != "" {
		notice += " Stop coordinates were sent to api.geops.io for network geometry."
	} else if provider == "transport.opendata.ch + OpenStreetMap routing" {
		notice += " Scheduled stop coordinates were sent to the configured OSRM or Overpass service for road and track geometry."
	}
	reportProgress(progress, 95, "Saving matched route")
	return model.RouteUpdate{
		Status:             status,
		Provider:           provider,
		Confidence:         confidence,
		Summary:            summary,
		GeoJSON:            encoded,
		ExternalDataNotice: notice,
		CacheHits:          cacheHits,
		TimeEstimated:      timeEstimated,
		DataVersion:        dataVersion,
	}, nil
}

func currentTimetableReference(original, now time.Time) time.Time {
	reference := now.In(original.Location()).AddDate(0, 0, 7)
	daysUntilWeekday := (int(original.Weekday()) - int(reference.Weekday()) + 7) % 7
	reference = reference.AddDate(0, 0, daysUntilWeekday)
	return time.Date(reference.Year(), reference.Month(), reference.Day(), original.Hour(), original.Minute(), 0, 0, original.Location())
}

func reportProgress(progress ProgressFunc, percent int, message string) {
	if progress != nil {
		progress(percent, message)
	}
}

func (m *Matcher) fetchConnections(ctx context.Context, journey model.Journey, validFrom time.Time) ([]connection, error) {
	endpoint, err := url.Parse(m.TransportURL)
	if err != nil {
		return nil, fmt.Errorf("parse transport API URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("from", journey.OriginName)
	query.Set("to", journey.DestinationName)
	query.Set("date", validFrom.Format("2006-01-02"))
	query.Set("time", validFrom.Format("15:04"))
	limit := m.NumberOfResults
	if limit <= 0 {
		limit = 6
	}
	query.Set("limit", strconv.Itoa(limit))
	endpoint.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create transport API request: %w", err)
	}
	request.Header.Set("User-Agent", m.UserAgent)
	response, err := m.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("query transport API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return nil, fmt.Errorf("transport API returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Connections []connection `json:"connections"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode transport API response: %w", err)
	}
	if len(payload.Connections) == 0 {
		return nil, fmt.Errorf("transport API returned no route candidates")
	}
	return payload.Connections, nil
}

func selectConnection(connections []connection, validFrom, validTo time.Time) (connection, float64, bool, error) {
	type scored struct {
		connection connection
		score      float64
	}
	items := make([]scored, 0, len(connections))
	for _, candidate := range connections {
		departure, depErr := parseAPITime(candidate.From.Departure)
		arrival, arrErr := parseAPITime(candidate.To.Arrival)
		if depErr != nil || arrErr != nil {
			continue
		}
		delta := math.Abs(departure.Sub(validFrom).Minutes())
		score := math.Max(0, 1-delta/90)
		if !departure.Before(validFrom.Add(-5*time.Minute)) && !arrival.After(validTo.Add(15*time.Minute)) {
			score = math.Min(1, score+0.05)
		}
		items = append(items, scored{connection: candidate, score: score})
	}
	if len(items) == 0 {
		return connection{}, 0, false, fmt.Errorf("transport API candidates had no usable timestamps")
	}
	sort.SliceStable(items, func(left, right int) bool { return items[left].score > items[right].score })
	ambiguous := items[0].score < 0.75
	if len(items) > 1 && items[0].score-items[1].score < 0.1 {
		ambiguous = true
	}
	return items[0].connection, math.Round(items[0].score*100) / 100, ambiguous, nil
}

func connectionGeoJSON(selected connection) (featureCollection, string) {
	collection := featureCollection{Type: "FeatureCollection", Features: make([]feature, 0)}
	labels := make([]string, 0)
	for index, section := range selected.Sections {
		coordinates := sectionCoordinates(section)
		if len(coordinates) < 2 {
			continue
		}
		mode, line := sectionModeAndLine(section)
		label := strings.TrimSpace(strings.Join([]string{mode, line}, " "))
		if label != "" && (len(labels) == 0 || labels[len(labels)-1] != label) {
			labels = append(labels, label)
		}
		collection.Features = append(collection.Features, feature{
			Type: "Feature",
			Properties: map[string]any{
				"section": index,
				"mode":    mode,
				"line":    line,
				"source":  "scheduled stop sequence",
			},
			Geometry: geometry{Type: "LineString", Line: coordinates},
		})
	}
	departure := clockValue(selected.From.Departure)
	arrival := clockValue(selected.To.Arrival)
	summary := strings.Join(labels, " · ")
	if departure != "" && arrival != "" {
		if summary != "" {
			summary += " · "
		}
		summary += departure + "–" + arrival
	}
	return collection, summary
}

func (m *Matcher) fetchGeOpsGeometry(ctx context.Context, selected connection) (featureCollection, error) {
	result := featureCollection{Type: "FeatureCollection", Features: make([]feature, 0)}
	for index, section := range selected.Sections {
		coordinates := sectionCoordinates(section)
		if len(coordinates) < 2 {
			continue
		}
		mode, line := sectionModeAndLine(section)
		geOpsMode := geOpsMode(mode)
		if geOpsMode == "" {
			continue
		}
		viaParts := make([]string, 0, len(coordinates))
		for _, coordinate := range coordinates {
			viaParts = append(viaParts, fmt.Sprintf("%.6f,%.6f", coordinate[0], coordinate[1]))
		}
		endpoint, err := url.Parse(m.GeOpsURL)
		if err != nil {
			return featureCollection{}, err
		}
		query := endpoint.Query()
		query.Set("via", strings.Join(viaParts, "|"))
		query.Set("mot", geOpsMode)
		query.Set("resolve-hops", "true")
		query.Set("length", "true")
		query.Set("key", m.GeOpsAPIKey)
		endpoint.RawQuery = query.Encode()

		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return featureCollection{}, err
		}
		request.Header.Set("User-Agent", m.UserAgent)
		response, err := m.Client.Do(request)
		if err != nil {
			return featureCollection{}, err
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return featureCollection{}, fmt.Errorf("geOps routing returned %s", response.Status)
		}
		var raw struct {
			Type     string          `json:"type"`
			Features json.RawMessage `json:"features"`
		}
		if err := json.NewDecoder(response.Body).Decode(&raw); err != nil {
			response.Body.Close()
			return featureCollection{}, err
		}
		response.Body.Close()
		var features []feature
		if err := json.Unmarshal(raw.Features, &features); err != nil {
			return featureCollection{}, err
		}
		for featureIndex := range features {
			if features[featureIndex].Properties == nil {
				features[featureIndex].Properties = map[string]any{}
			}
			features[featureIndex].Properties["section"] = index
			features[featureIndex].Properties["mode"] = mode
			features[featureIndex].Properties["line"] = line
			features[featureIndex].Properties["source"] = "geOps routing"
		}
		result.Features = append(result.Features, features...)
	}
	if len(result.Features) == 0 {
		return featureCollection{}, fmt.Errorf("geOps returned no usable route geometry")
	}
	return result, nil
}

func sectionCoordinates(section section) [][]float64 {
	coordinates := make([][]float64, 0)
	coordinates = appendCoordinate(coordinates, section.Departure.Station.Coordinate)
	if section.Journey != nil {
		for _, stop := range section.Journey.PassList {
			coordinates = appendCoordinate(coordinates, stop.Station.Coordinate)
		}
	}
	coordinates = appendCoordinate(coordinates, section.Arrival.Station.Coordinate)
	return coordinates
}

func appendCoordinate(coordinates [][]float64, value coordinate) [][]float64 {
	latitude, latOK := value.X.Value()
	longitude, lonOK := value.Y.Value()
	if !latOK || !lonOK || latitude == 0 || longitude == 0 {
		return coordinates
	}
	next := []float64{longitude, latitude}
	if len(coordinates) > 0 {
		last := coordinates[len(coordinates)-1]
		if last[0] == next[0] && last[1] == next[1] {
			return coordinates
		}
	}
	return append(coordinates, next)
}

func sectionModeAndLine(section section) (string, string) {
	if section.Journey == nil {
		return "walk", ""
	}
	mode := strings.TrimSpace(section.Journey.Category)
	line := strings.TrimSpace(section.Journey.Number)
	if line == "" {
		line = strings.TrimSpace(section.Journey.Name)
	}
	return mode, line
}

func geOpsMode(mode string) string {
	upper := strings.ToUpper(mode)
	switch {
	case networkMode(mode) == "road":
		return "bus"
	case networkMode(mode) == "tram":
		return "tram"
	case strings.Contains(upper, "SHIP") || strings.Contains(upper, "BAT"):
		return "ferry"
	case strings.Contains(upper, "GONDOLA") || strings.Contains(upper, "CABLE"):
		return "gondola"
	case upper == "":
		return ""
	default:
		return "rail"
	}
}

func clockValue(value string) string {
	parsed, err := parseAPITime(value)
	if err != nil {
		return ""
	}
	return parsed.Format("15:04")
}

func parseAPITime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05-0700"} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported API timestamp %q", value)
}

type floatValue float64

func (f *floatValue) UnmarshalJSON(data []byte) error {
	value := strings.Trim(string(data), `"`)
	if value == "" || value == "null" {
		*f = 0
		return nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return err
	}
	*f = floatValue(parsed)
	return nil
}

func (f floatValue) Value() (float64, bool) {
	value := float64(f)
	return value, !math.IsNaN(value) && !math.IsInf(value, 0)
}

type coordinate struct {
	X floatValue `json:"x"`
	Y floatValue `json:"y"`
}

type station struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Coordinate coordinate `json:"coordinate"`
}

type stop struct {
	Station   station `json:"station"`
	Arrival   string  `json:"arrival"`
	Departure string  `json:"departure"`
}

type journeyDetail struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	Number   string `json:"number"`
	Operator string `json:"operator"`
	To       string `json:"to"`
	PassList []stop `json:"passList"`
}

type section struct {
	Journey   *journeyDetail `json:"journey"`
	Departure stop           `json:"departure"`
	Arrival   stop           `json:"arrival"`
}

type connection struct {
	From     stop      `json:"from"`
	To       stop      `json:"to"`
	Duration string    `json:"duration"`
	Products []string  `json:"products"`
	Sections []section `json:"sections"`
}

type featureCollection struct {
	Type     string    `json:"type"`
	Features []feature `json:"features"`
}

type feature struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties,omitempty"`
	Geometry   geometry       `json:"geometry"`
}

type geometry struct {
	Type        string          `json:"type"`
	Coordinates json.RawMessage `json:"-"`
	Line        [][]float64     `json:"-"`
}

func (g geometry) MarshalJSON() ([]byte, error) {
	if len(g.Coordinates) > 0 {
		return json.Marshal(struct {
			Type        string          `json:"type"`
			Coordinates json.RawMessage `json:"coordinates"`
		}{Type: g.Type, Coordinates: g.Coordinates})
	}
	return json.Marshal(struct {
		Type        string      `json:"type"`
		Coordinates [][]float64 `json:"coordinates"`
	}{Type: g.Type, Coordinates: g.Line})
}

func (g *geometry) UnmarshalJSON(data []byte) error {
	var raw struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	g.Type = raw.Type
	g.Coordinates = raw.Coordinates
	return nil
}

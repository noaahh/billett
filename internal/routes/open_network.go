package routes

import (
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const maxOSRMWaypoints = 75
const geometryCacheTTL = 30 * 24 * time.Hour

func (m *Matcher) fetchOpenNetworkGeometry(
	ctx context.Context,
	selected connection,
	progress func(completed, total, cacheHits int),
) (featureCollection, int, int, error) {
	result := featureCollection{Type: "FeatureCollection", Features: make([]feature, 0)}
	refinedSections := 0
	cacheHits := 0
	for index, section := range selected.Sections {
		stops := sectionCoordinates(section)
		if len(stops) < 2 {
			if progress != nil {
				progress(index+1, len(selected.Sections), cacheHits)
			}
			continue
		}
		mode, line := sectionModeAndLine(section)
		network := networkMode(mode)
		coordinates := stops
		source := "scheduled stop sequence"
		var routeErr error
		sectionCacheHits := 0
		switch network {
		case "road":
			coordinates, sectionCacheHits, routeErr = m.fetchOSRMRoute(ctx, stops)
			source = "OpenStreetMap road routing"
		case "rail", "tram":
			coordinates, sectionCacheHits, routeErr = m.fetchOverpassRailRoute(ctx, stops, network)
			source = "OpenStreetMap track routing"
		default:
			routeErr = fmt.Errorf("mode %q has no open network router", mode)
		}
		if routeErr != nil || len(coordinates) < 2 {
			coordinates = stops
			source = "scheduled stop sequence"
		} else {
			refinedSections++
		}
		cacheHits += sectionCacheHits
		properties := map[string]any{
			"section": index,
			"mode":    mode,
			"line":    line,
			"source":  source,
		}
		if routeErr != nil {
			properties["fallbackReason"] = routeErr.Error()
		}
		result.Features = append(result.Features, feature{
			Type:       "Feature",
			Properties: properties,
			Geometry:   geometry{Type: "LineString", Line: coordinates},
		})
		if progress != nil {
			progress(index+1, len(selected.Sections), cacheHits)
		}
	}
	if len(result.Features) == 0 {
		return featureCollection{}, 0, cacheHits, fmt.Errorf("connection has no route geometry")
	}
	if refinedSections == 0 {
		return result, 0, cacheHits, fmt.Errorf("no section could be matched to an open transport network")
	}
	return result, refinedSections, cacheHits, nil
}

func networkMode(mode string) string {
	upper := strings.ToUpper(strings.TrimSpace(mode))
	switch {
	case upper == "B", upper == "BUS", strings.Contains(upper, "BUS"), strings.HasPrefix(upper, "NF"):
		return "road"
	case upper == "T", upper == "TRAM", strings.Contains(upper, "TRAM"):
		return "tram"
	case upper == "", upper == "WALK":
		return ""
	case strings.Contains(upper, "SHIP"), upper == "BAT", strings.Contains(upper, "FERRY"):
		return ""
	case strings.Contains(upper, "GONDOLA"), strings.Contains(upper, "CABLE"), upper == "FUN":
		return ""
	default:
		return "rail"
	}
}

func (m *Matcher) fetchOSRMRoute(ctx context.Context, stops [][]float64) ([][]float64, int, error) {
	if strings.TrimSpace(m.OSRMURL) == "" {
		return nil, 0, fmt.Errorf("OSRM_URL is empty")
	}
	result := make([][]float64, 0)
	cacheHits := 0
	for start := 0; start < len(stops)-1; {
		end := min(start+maxOSRMWaypoints, len(stops))
		part, cacheHit, err := m.fetchOSRMRoutePart(ctx, stops[start:end])
		if err != nil {
			return nil, cacheHits, err
		}
		if cacheHit {
			cacheHits++
		}
		result = appendCoordinates(result, part)
		start = end - 1
	}
	return result, cacheHits, nil
}

func (m *Matcher) fetchOSRMRoutePart(ctx context.Context, stops [][]float64) ([][]float64, bool, error) {
	key := geometryCacheKey("osrm", m.OSRMURL, "road", stops)
	return m.cachedCoordinates(ctx, key, func() ([][]float64, error) {
		return m.fetchOSRMRoutePartNetwork(ctx, stops)
	})
}

func (m *Matcher) fetchOSRMRoutePartNetwork(ctx context.Context, stops [][]float64) ([][]float64, error) {
	parts := make([]string, 0, len(stops))
	for _, stop := range stops {
		parts = append(parts, fmt.Sprintf("%.6f,%.6f", stop[0], stop[1]))
	}
	endpoint, err := url.Parse(strings.TrimRight(m.OSRMURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse OSRM URL: %w", err)
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/route/v1/driving/" + strings.Join(parts, ";")
	query := endpoint.Query()
	query.Set("overview", "full")
	query.Set("geometries", "geojson")
	query.Set("steps", "false")
	query.Set("continue_straight", "false")
	endpoint.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create OSRM request: %w", err)
	}
	request.Header.Set("User-Agent", m.UserAgent)
	response, err := m.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("query OSRM: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return nil, fmt.Errorf("OSRM returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Code      string `json:"code"`
		Waypoints []struct {
			Distance float64 `json:"distance"`
		} `json:"waypoints"`
		Routes []struct {
			Geometry struct {
				Type        string      `json:"type"`
				Coordinates [][]float64 `json:"coordinates"`
			} `json:"geometry"`
		} `json:"routes"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode OSRM response: %w", err)
	}
	if payload.Code != "Ok" || len(payload.Routes) == 0 {
		return nil, fmt.Errorf("OSRM did not find a road route: %s", payload.Code)
	}
	for _, waypoint := range payload.Waypoints {
		if waypoint.Distance > 750 {
			return nil, fmt.Errorf("OSRM snapped a stop %.0f metres away from the road network", waypoint.Distance)
		}
	}
	coordinates := payload.Routes[0].Geometry.Coordinates
	if payload.Routes[0].Geometry.Type != "LineString" || len(coordinates) < 2 {
		return nil, fmt.Errorf("OSRM returned no usable LineString")
	}
	return coordinates, nil
}

func (m *Matcher) fetchOverpassRailRoute(ctx context.Context, stops [][]float64, mode string) ([][]float64, int, error) {
	if strings.TrimSpace(m.OverpassURL) == "" {
		return nil, 0, fmt.Errorf("OVERPASS_URL is empty")
	}
	result := make([][]float64, 0)
	cacheHits := 0
	for index := 1; index < len(stops); index++ {
		part, cacheHit, err := m.fetchOverpassRailPart(ctx, stops[index-1], stops[index], mode)
		if err != nil {
			return nil, cacheHits, fmt.Errorf("track leg %d of %d: %w", index, len(stops)-1, err)
		}
		if cacheHit {
			cacheHits++
		}
		result = appendCoordinates(result, part)
	}
	return result, cacheHits, nil
}

func (m *Matcher) fetchOverpassRailPart(ctx context.Context, from, to []float64, mode string) ([][]float64, bool, error) {
	stops := [][]float64{from, to}
	key := geometryCacheKey("overpass", m.OverpassURL, mode, stops)
	return m.cachedCoordinates(ctx, key, func() ([][]float64, error) {
		return m.fetchOverpassRailPartNetwork(ctx, from, to, mode)
	})
}

func (m *Matcher) fetchOverpassRailPartNetwork(ctx context.Context, from, to []float64, mode string) ([][]float64, error) {
	latitudeMargin := 0.15
	if mode == "tram" {
		latitudeMargin = 0.015
	}
	middleLatitude := (from[1] + to[1]) / 2
	longitudeMargin := latitudeMargin / math.Max(0.4, math.Cos(middleLatitude*math.Pi/180))
	south := math.Min(from[1], to[1]) - latitudeMargin
	north := math.Max(from[1], to[1]) + latitudeMargin
	west := math.Min(from[0], to[0]) - longitudeMargin
	east := math.Max(from[0], to[0]) + longitudeMargin
	railwayPattern := "^(rail|narrow_gauge)$"
	if mode == "tram" {
		railwayPattern = "^(tram|light_rail)$"
	}
	query := fmt.Sprintf(`[out:json][timeout:25];way["railway"~"%s"](%.6f,%.6f,%.6f,%.6f);out body geom;`, railwayPattern, south, west, north, east)
	form := url.Values{"data": []string{query}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.OverpassURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create Overpass request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", m.UserAgent)
	response, err := m.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("query Overpass: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return nil, fmt.Errorf("Overpass returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Elements []osmWay `json:"elements"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 32<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode Overpass response: %w", err)
	}
	graph := buildRailGraph(payload.Elements, mode)
	return graph.routeThrough([][]float64{from, to})
}

func (m *Matcher) cachedCoordinates(
	ctx context.Context,
	key string,
	fetch func() ([][]float64, error),
) ([][]float64, bool, error) {
	if m.Cache != nil {
		cached, found, err := m.Cache.GetRouteGeometry(ctx, key)
		if err == nil && found {
			var coordinates [][]float64
			if json.Unmarshal(cached, &coordinates) == nil && len(coordinates) >= 2 {
				return coordinates, true, nil
			}
		}
	}
	coordinates, err := fetch()
	if err != nil {
		return nil, false, err
	}
	if m.Cache != nil {
		if encoded, encodeErr := json.Marshal(coordinates); encodeErr == nil {
			_ = m.Cache.PutRouteGeometry(ctx, key, encoded, geometryCacheTTL)
		}
	}
	return coordinates, false, nil
}

func geometryCacheKey(provider, endpoint, mode string, stops [][]float64) string {
	hash := sha256.New()
	fmt.Fprintf(hash, "billett-route-geometry-v1\n%s\n%s\n%s\n", provider, strings.TrimRight(endpoint, "/"), mode)
	for _, stop := range stops {
		if len(stop) < 2 {
			continue
		}
		fmt.Fprintf(hash, "%.6f,%.6f\n", stop[0], stop[1])
	}
	return provider + ":" + hex.EncodeToString(hash.Sum(nil))
}

type osmWay struct {
	ID       int64             `json:"id"`
	Nodes    []int64           `json:"nodes"`
	Tags     map[string]string `json:"tags"`
	Geometry []struct {
		Lat float64 `json:"lat"`
		Lon float64 `json:"lon"`
	} `json:"geometry"`
}

type graphEdge struct {
	to     int64
	weight float64
}

type railGraph struct {
	nodes map[int64][]float64
	edges map[int64][]graphEdge
}

func buildRailGraph(ways []osmWay, mode string) railGraph {
	graph := railGraph{nodes: make(map[int64][]float64), edges: make(map[int64][]graphEdge)}
	for _, way := range ways {
		if len(way.Nodes) < 2 || len(way.Nodes) != len(way.Geometry) {
			continue
		}
		penalty := railWayPenalty(way.Tags, mode)
		for index, nodeID := range way.Nodes {
			point := way.Geometry[index]
			graph.nodes[nodeID] = []float64{point.Lon, point.Lat}
			if index == 0 {
				continue
			}
			previousID := way.Nodes[index-1]
			weight := haversineMeters(graph.nodes[previousID], graph.nodes[nodeID]) * penalty
			graph.edges[previousID] = append(graph.edges[previousID], graphEdge{to: nodeID, weight: weight})
			graph.edges[nodeID] = append(graph.edges[nodeID], graphEdge{to: previousID, weight: weight})
		}
	}
	return graph
}

func railWayPenalty(tags map[string]string, mode string) float64 {
	penalty := 1.0
	railway := tags["railway"]
	if mode == "tram" && railway != "tram" && railway != "light_rail" {
		penalty *= 2.5
	}
	if mode == "rail" && railway == "tram" {
		penalty *= 4
	}
	switch tags["service"] {
	case "yard", "siding", "spur":
		penalty *= 3
	}
	return penalty
}

func (g railGraph) routeThrough(stops [][]float64) ([][]float64, error) {
	if len(g.nodes) == 0 {
		return nil, fmt.Errorf("OpenStreetMap returned no track nodes")
	}
	result := make([][]float64, 0)
	for index := 1; index < len(stops); index++ {
		path, err := g.shortestPathNear(stops[index-1], stops[index])
		if err != nil {
			return nil, err
		}
		coordinates := make([][]float64, 0, len(path))
		for _, nodeID := range path {
			coordinates = append(coordinates, g.nodes[nodeID])
		}
		result = appendCoordinates(result, coordinates)
	}
	if len(result) < 2 {
		return nil, fmt.Errorf("track routing returned no usable path")
	}
	return result, nil
}

type nodeCandidate struct {
	node     int64
	distance float64
}

func (g railGraph) nearestNodes(point []float64, limit int) []nodeCandidate {
	candidates := make([]nodeCandidate, 0, len(g.nodes))
	for nodeID, candidate := range g.nodes {
		distance := haversineMeters(point, candidate)
		if distance <= 2500 {
			candidates = append(candidates, nodeCandidate{node: nodeID, distance: distance})
		}
	}
	sort.Slice(candidates, func(left, right int) bool { return candidates[left].distance < candidates[right].distance })
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates
}

func (g railGraph) shortestPathNear(from, to []float64) ([]int64, error) {
	const candidateCount = 512
	const snapPenalty = 3.0
	starts := g.nearestNodes(from, candidateCount)
	ends := g.nearestNodes(to, candidateCount)
	if len(starts) == 0 || len(ends) == 0 {
		return nil, fmt.Errorf("scheduled stop is too far from the OpenStreetMap track network")
	}
	targets := make(map[int64]float64, len(ends))
	for _, candidate := range ends {
		targets[candidate.node] = candidate.distance * snapPenalty
	}
	distances := make(map[int64]float64)
	previous := make(map[int64]int64)
	queue := &pathQueue{}
	for _, candidate := range starts {
		distance := candidate.distance * snapPenalty
		distances[candidate.node] = distance
		heap.Push(queue, pathQueueItem{node: candidate.node, distance: distance})
	}
	heap.Init(queue)
	bestTarget := int64(0)
	bestDistance := math.Inf(1)
	for queue.Len() > 0 {
		current := heap.Pop(queue).(pathQueueItem)
		if current.distance > distances[current.node] {
			continue
		}
		if current.distance >= bestDistance {
			break
		}
		if targetDistance, found := targets[current.node]; found && current.distance+targetDistance < bestDistance {
			bestTarget = current.node
			bestDistance = current.distance + targetDistance
		}
		for _, edge := range g.edges[current.node] {
			candidate := current.distance + edge.weight
			known, seen := distances[edge.to]
			if seen && candidate >= known {
				continue
			}
			distances[edge.to] = candidate
			previous[edge.to] = current.node
			heap.Push(queue, pathQueueItem{node: edge.to, distance: candidate})
		}
	}
	if bestTarget == 0 {
		return nil, fmt.Errorf("OpenStreetMap track graph has no path between scheduled stops (%d start candidates reached %d of %d nodes; %d end candidates)", len(starts), len(distances), len(g.nodes), len(ends))
	}
	path := []int64{bestTarget}
	for {
		prior, found := previous[path[len(path)-1]]
		if !found {
			break
		}
		path = append(path, prior)
	}
	for left, right := 0, len(path)-1; left < right; left, right = left+1, right-1 {
		path[left], path[right] = path[right], path[left]
	}
	return path, nil
}

type pathQueueItem struct {
	node     int64
	distance float64
}

type pathQueue []pathQueueItem

func (q pathQueue) Len() int           { return len(q) }
func (q pathQueue) Less(i, j int) bool { return q[i].distance < q[j].distance }
func (q pathQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *pathQueue) Push(value any)    { *q = append(*q, value.(pathQueueItem)) }
func (q *pathQueue) Pop() any {
	old := *q
	last := old[len(old)-1]
	*q = old[:len(old)-1]
	return last
}

func appendCoordinates(target, coordinates [][]float64) [][]float64 {
	for _, coordinate := range coordinates {
		if len(target) > 0 {
			last := target[len(target)-1]
			if last[0] == coordinate[0] && last[1] == coordinate[1] {
				continue
			}
		}
		target = append(target, coordinate)
	}
	return target
}

func haversineMeters(left, right []float64) float64 {
	const earthRadius = 6371000
	lat1 := left[1] * math.Pi / 180
	lat2 := right[1] * math.Pi / 180
	deltaLat := (right[1] - left[1]) * math.Pi / 180
	deltaLon := (right[0] - left[0]) * math.Pi / 180
	a := math.Sin(deltaLat/2)*math.Sin(deltaLat/2) + math.Cos(lat1)*math.Cos(lat2)*math.Sin(deltaLon/2)*math.Sin(deltaLon/2)
	return earthRadius * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

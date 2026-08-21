package model

import "encoding/json"

type Journey struct {
	ID                   string          `json:"id"`
	IngestionID          string          `json:"-"`
	SourceHash           string          `json:"-"`
	SourceFilename       string          `json:"sourceFilename,omitempty"`
	MessageID            string          `json:"messageId,omitempty"`
	EmailSubject         string          `json:"emailSubject"`
	Sender               string          `json:"sender,omitempty"`
	OrderNumber          string          `json:"orderNumber,omitempty"`
	ReferenceNumber      string          `json:"referenceNumber,omitempty"`
	JourneyDate          string          `json:"journeyDate"`
	OriginName           string          `json:"originName"`
	DestinationName      string          `json:"destinationName"`
	ValidFrom            string          `json:"validFrom"`
	ValidTo              string          `json:"validTo"`
	TravelClass          string          `json:"travelClass"`
	FareProduct          string          `json:"fareProduct"`
	FareDetail           string          `json:"fareDetail,omitempty"`
	FareReduction        string          `json:"fareReduction,omitempty"`
	PaymentMethod        string          `json:"paymentMethod,omitempty"`
	PriceCents           int64           `json:"priceCents"`
	Currency             string          `json:"currency"`
	EvidenceType         string          `json:"evidenceType"`
	OccurrenceStatus     string          `json:"occurrenceStatus"`
	TimeSource           string          `json:"timeSource"`
	Notes                string          `json:"notes,omitempty"`
	RouteStatus          string          `json:"routeStatus"`
	RouteProgress        int             `json:"routeProgress"`
	RouteProgressMessage string          `json:"routeProgressMessage,omitempty"`
	RouteProvider        string          `json:"routeProvider,omitempty"`
	RouteConfidence      float64         `json:"routeConfidence,omitempty"`
	RouteSummary         string          `json:"routeSummary,omitempty"`
	RouteGeoJSON         json.RawMessage `json:"routeGeojson,omitempty"`
	RouteExternalNotice  string          `json:"routeExternalNotice,omitempty"`
	RouteCacheHits       int             `json:"routeCacheHits,omitempty"`
	RouteDataVersion     string          `json:"routeDataVersion,omitempty"`
	RouteTimeEstimated   bool            `json:"routeTimeEstimated"`
	CreatedAt            string          `json:"createdAt"`
	UpdatedAt            string          `json:"updatedAt"`
}

type RouteUpdate struct {
	Status             string
	Provider           string
	Confidence         float64
	Summary            string
	GeoJSON            json.RawMessage
	ExternalDataNotice string
	CacheHits          int
	DataVersion        string
	TimeEstimated      bool
}

type JourneyPatch struct {
	JourneyDate      *string `json:"journeyDate"`
	OriginName       *string `json:"originName"`
	DestinationName  *string `json:"destinationName"`
	ValidFrom        *string `json:"validFrom"`
	ValidTo          *string `json:"validTo"`
	OccurrenceStatus *string `json:"occurrenceStatus"`
	TimeSource       *string `json:"timeSource"`
	Notes            *string `json:"notes"`
}

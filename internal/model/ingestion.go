package model

type Ingestion struct {
	ID              string `json:"id"`
	SourceHash      string `json:"-"`
	MessageID       string `json:"messageId,omitempty"`
	SourceKind      string `json:"sourceKind"`
	SourceName      string `json:"sourceName"`
	EmailSubject    string `json:"emailSubject,omitempty"`
	Status          string `json:"status"`
	ParserVersion   string `json:"parserVersion"`
	JourneyCount    int    `json:"journeyCount"`
	CreatedCount    int    `json:"createdCount"`
	Error           string `json:"error,omitempty"`
	RawArchived     bool   `json:"rawArchived"`
	AttemptCount    int    `json:"attemptCount"`
	FirstReceivedAt string `json:"firstReceivedAt"`
	LastAttemptAt   string `json:"lastAttemptAt"`
}

type IngestionSummary struct {
	Total      int `json:"total"`
	Imported   int `json:"imported"`
	Duplicates int `json:"duplicates"`
	Rejected   int `json:"rejected"`
	Failed     int `json:"failed"`
	Journeys   int `json:"journeys"`
}

package ingest

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/emersion/go-mbox"
)

type BatchItem struct {
	Index      int    `json:"index"`
	SourceName string `json:"sourceName"`
	Subject    string `json:"subject,omitempty"`
	Status     string `json:"status"`
	Journeys   int    `json:"journeys"`
	Created    int    `json:"created"`
	Error      string `json:"error,omitempty"`
}

type BatchReport struct {
	Source     string      `json:"source"`
	Messages   int         `json:"messages"`
	Imported   int         `json:"imported"`
	Duplicates int         `json:"duplicates"`
	Rejected   int         `json:"rejected"`
	Failed     int         `json:"failed"`
	Journeys   int         `json:"journeys"`
	Items      []BatchItem `json:"items"`
}

func (s *Service) ImportMBOX(ctx context.Context, filename string) (BatchReport, error) {
	file, err := os.Open(filename)
	if err != nil {
		return BatchReport{}, fmt.Errorf("open MBOX: %w", err)
	}
	defer file.Close()
	report := BatchReport{Source: filename, Items: make([]BatchItem, 0)}
	reader := mbox.NewReader(file)
	for index := 1; ; index++ {
		message, nextErr := reader.NextMessage()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return report, fmt.Errorf("read MBOX message %d: %w", index, nextErr)
		}
		raw, readErr := io.ReadAll(io.LimitReader(message, MaxMessageBytes+1))
		sourceName := fmt.Sprintf("%s#%d", filepath.Base(filename), index)
		item := BatchItem{Index: index, SourceName: sourceName}
		report.Messages++
		if readErr != nil {
			item.Status = "failed"
			item.Error = readErr.Error()
			report.Failed++
			report.Items = append(report.Items, item)
			continue
		}
		if len(raw) > MaxMessageBytes {
			item.Status = "failed"
			item.Error = "email exceeds the 25 MiB ingestion limit"
			report.Failed++
			report.Items = append(report.Items, item)
			continue
		}
		result, importErr := s.Import(ctx, raw, Source{Kind: "mbox", Name: sourceName})
		item.Subject = result.Ingestion.EmailSubject
		item.Status = result.Ingestion.Status
		item.Journeys = len(result.Journeys)
		item.Created = result.Created
		if importErr != nil {
			item.Error = importErr.Error()
		}
		report.Journeys += result.Created
		switch item.Status {
		case "imported":
			report.Imported++
		case "duplicate":
			report.Duplicates++
		case "rejected":
			report.Rejected++
		default:
			report.Failed++
		}
		report.Items = append(report.Items, item)
	}
	return report, nil
}

func (s *Service) ImportFiles(ctx context.Context, filenames []string) BatchReport {
	report := BatchReport{Items: make([]BatchItem, 0)}
	for index, filename := range filenames {
		item := BatchItem{Index: index + 1, SourceName: filename}
		report.Messages++
		raw, err := os.ReadFile(filename)
		if err != nil {
			item.Status = "failed"
			item.Error = err.Error()
			report.Failed++
			report.Items = append(report.Items, item)
			continue
		}
		result, importErr := s.Import(ctx, raw, Source{Kind: "eml", Name: filepath.Base(filename)})
		item.Subject = result.Ingestion.EmailSubject
		item.Status = result.Ingestion.Status
		item.Journeys = len(result.Journeys)
		item.Created = result.Created
		if importErr != nil {
			item.Error = importErr.Error()
		}
		report.Journeys += result.Created
		switch item.Status {
		case "imported":
			report.Imported++
		case "duplicate":
			report.Duplicates++
		case "rejected":
			report.Rejected++
		default:
			report.Failed++
		}
		report.Items = append(report.Items, item)
	}
	return report
}

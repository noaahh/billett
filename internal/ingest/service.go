package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"net/mail"
	"os"
	"path/filepath"
	"strings"

	"billett/internal/emailparse"
	"billett/internal/model"
	"billett/internal/store"
)

const MaxMessageBytes = 25 << 20

type Source struct {
	Kind string
	Name string
}

type Result struct {
	Ingestion model.Ingestion `json:"ingestion"`
	Journeys  []model.Journey `json:"journeys"`
	Created   int             `json:"createdCount"`
}

type Service struct {
	Store         *store.Store
	RawArchiveDir string
}

func (s *Service) Import(ctx context.Context, raw []byte, source Source) (Result, error) {
	if len(raw) == 0 {
		return Result{}, fmt.Errorf("email is empty")
	}
	if len(raw) > MaxMessageBytes {
		return Result{}, fmt.Errorf("email exceeds the 25 MiB ingestion limit")
	}
	if s.Store == nil {
		return Result{}, fmt.Errorf("ingestion store is required")
	}
	hash := sha256.Sum256(raw)
	sourceHash := hex.EncodeToString(hash[:])
	messageID, subject := messageMetadata(raw)
	ingestion, err := s.Store.BeginIngestion(ctx, model.Ingestion{
		SourceHash:    sourceHash,
		MessageID:     messageID,
		SourceKind:    normalizedSourceKind(source.Kind),
		SourceName:    source.Name,
		EmailSubject:  subject,
		ParserVersion: emailparse.ParserVersion,
	})
	if err != nil {
		return Result{}, err
	}

	rawArchived := ingestion.RawArchived
	if strings.TrimSpace(s.RawArchiveDir) != "" && !rawArchived {
		if err := archiveRawMessage(s.RawArchiveDir, sourceHash, raw); err != nil {
			finished, finishErr := s.Store.FinishIngestion(ctx, ingestion.ID, "failed", 0, 0, err.Error(), false)
			if finishErr == nil {
				ingestion = finished
			}
			return Result{Ingestion: ingestion}, fmt.Errorf("archive raw email: %w", err)
		}
		rawArchived = true
	}

	parsed, parseErr := emailparse.ParseAll(raw, source.Name)
	if parseErr != nil {
		status := "failed"
		if !emailparse.IsSupportedSubject(subject) {
			status = "rejected"
		}
		finished, finishErr := s.Store.FinishIngestion(ctx, ingestion.ID, status, 0, 0, parseErr.Error(), rawArchived)
		if finishErr != nil {
			return Result{Ingestion: ingestion}, fmt.Errorf("parse email: %v; save ingestion result: %w", parseErr, finishErr)
		}
		return Result{Ingestion: finished}, parseErr
	}

	stored := make([]model.Journey, 0, len(parsed))
	createdCount := 0
	for _, journey := range parsed {
		journey.IngestionID = ingestion.ID
		item, created, insertErr := s.Store.InsertJourney(ctx, journey)
		if insertErr != nil {
			finished, _ := s.Store.FinishIngestion(ctx, ingestion.ID, "failed", len(parsed), createdCount, insertErr.Error(), rawArchived)
			return Result{Ingestion: finished, Journeys: stored, Created: createdCount}, insertErr
		}
		if created {
			createdCount++
		}
		stored = append(stored, item)
	}
	status := "imported"
	if createdCount == 0 {
		status = "duplicate"
	}
	finished, err := s.Store.FinishIngestion(ctx, ingestion.ID, status, len(stored), createdCount, "", rawArchived)
	if err != nil {
		return Result{Ingestion: ingestion, Journeys: stored, Created: createdCount}, err
	}
	return Result{Ingestion: finished, Journeys: stored, Created: createdCount}, nil
}

func messageMetadata(raw []byte) (string, string) {
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", ""
	}
	subject := message.Header.Get("Subject")
	if decoded, err := new(mime.WordDecoder).DecodeHeader(subject); err == nil {
		subject = decoded
	}
	return strings.TrimSpace(message.Header.Get("Message-ID")), strings.TrimSpace(subject)
}

func normalizedSourceKind(value string) string {
	switch strings.TrimSpace(value) {
	case "browser", "inbound", "eml", "mbox":
		return strings.TrimSpace(value)
	default:
		return "unknown"
	}
}

func archiveRawMessage(directory, sourceHash string, raw []byte) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	destination := filepath.Join(directory, sourceHash+".eml")
	if _, err := os.Stat(destination); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".billett-raw-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, destination)
}

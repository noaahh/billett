package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"billett/internal/store"
	"github.com/emersion/go-mbox"
)

func TestImportMBOXContinuesAndAuditsEveryMessage(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "billett.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	valid, err := os.ReadFile(filepath.Join("..", "emailparse", "testdata", "easyride.eml"))
	if err != nil {
		t.Fatal(err)
	}
	mboxPath := filepath.Join(t.TempDir(), "mail.mbox")
	file, err := os.Create(mboxPath)
	if err != nil {
		t.Fatal(err)
	}
	writer := mbox.NewWriter(file)
	for index, raw := range [][]byte{valid, []byte("From: test@example.test\nSubject: Other receipt\n\nNo journey\n"), valid} {
		message, createErr := writer.CreateMessage("test@example.test", time.Date(2026, 8, index+1, 12, 0, 0, 0, time.UTC))
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, writeErr := message.Write(raw); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	archiveDir := filepath.Join(t.TempDir(), "raw")
	service := &Service{Store: database, RawArchiveDir: archiveDir}
	report, err := service.ImportMBOX(context.Background(), mboxPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.Messages != 3 || report.Imported != 1 || report.Duplicates != 1 || report.Rejected != 1 || report.Failed != 0 || report.Journeys != 1 {
		t.Fatalf("report: %+v", report)
	}
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("raw archive entries: got %d, want 2 unique messages", len(entries))
	}
}

func TestImportRejectsOversizedMessage(t *testing.T) {
	service := &Service{}
	_, err := service.Import(context.Background(), []byte(strings.Repeat("x", MaxMessageBytes+1)), Source{})
	if err == nil || !strings.Contains(err.Error(), "25 MiB") {
		t.Fatalf("oversized error: %v", err)
	}
}

package emailparse

import (
	"os"
	"strings"
	"testing"
)

func TestParseEasyRideReceipt(t *testing.T) {
	raw, err := os.ReadFile("testdata/easyride.eml")
	if err != nil {
		t.Fatal(err)
	}
	journey, err := Parse(raw, "easyride.eml")
	if err != nil {
		t.Fatal(err)
	}

	assertEqual(t, "origin", journey.OriginName, "Zürich HB")
	assertEqual(t, "destination", journey.DestinationName, "Bern")
	assertEqual(t, "journey date", journey.JourneyDate, "2026-07-01")
	assertEqual(t, "valid from", journey.ValidFrom, "2026-07-01T10:17:00+02:00")
	assertEqual(t, "valid to", journey.ValidTo, "2026-07-01T11:15:00+02:00")
	assertEqual(t, "fare product", journey.FareProduct, "Einzelbillett")
	assertEqual(t, "fare reduction", journey.FareReduction, "2. Klasse, Halbtax-Abo")
	assertEqual(t, "payment", journey.PaymentMethod, "Halbtax PLUS")
	assertEqual(t, "message ID", journey.MessageID, "<sanitized-easyride@example.test>")
	if journey.PriceCents != 2550 {
		t.Fatalf("price: got %d, want 2550", journey.PriceCents)
	}
	if len(journey.SourceHash) != 64 {
		t.Fatalf("source hash length: got %d, want 64", len(journey.SourceHash))
	}
}

func TestRejectsUnsupportedEmail(t *testing.T) {
	raw := []byte("Subject: Unrelated receipt\n\nNothing to parse")
	if _, err := Parse(raw, "other.eml"); err == nil {
		t.Fatal("expected unsupported email error")
	}
}

func TestParseOnlinePurchaseReceipt(t *testing.T) {
	raw, err := os.ReadFile("testdata/online_purchase.eml")
	if err != nil {
		t.Fatal(err)
	}
	journey, err := Parse(raw, "online_purchase.eml")
	if err != nil {
		t.Fatal(err)
	}

	assertEqual(t, "origin", journey.OriginName, "Wilderswil")
	assertEqual(t, "destination", journey.DestinationName, "Schynige Platte")
	assertEqual(t, "journey date", journey.JourneyDate, "2026-08-01")
	assertEqual(t, "valid from", journey.ValidFrom, "2026-08-01T00:00:00+02:00")
	assertEqual(t, "valid to", journey.ValidTo, "2026-08-02T05:00:00+02:00")
	assertEqual(t, "fare product", journey.FareProduct, "Streckenbillett")
	assertEqual(t, "fare detail", journey.FareDetail, "einfache Fahrt")
	assertEqual(t, "fare reduction", journey.FareReduction, "GA 2. Klasse")
	assertEqual(t, "travel class", journey.TravelClass, "2. Klasse")
	assertEqual(t, "order number", journey.OrderNumber, "2468013579")
	assertEqual(t, "reference number", journey.ReferenceNumber, "9876543210")
	assertEqual(t, "message ID", journey.MessageID, "<sanitized-online-purchase@example.test>")
	if journey.PriceCents != 1700 {
		t.Fatalf("price: got %d, want 1700", journey.PriceCents)
	}
}

func TestParseAllOnlinePurchaseTickets(t *testing.T) {
	raw, err := os.ReadFile("testdata/online_purchase.eml")
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
		"1. Klasse, einfache Fahrt, Halbtax-Abo",
		"CHF 28.50",
		"Gültig:",
		"03.08.2026 00:00 -",
		"04.08.2026 05:00",
		"Referenz-Nr.:",
		"1122334455",
		"Bestell-Nr.:",
		"2468013579",
		"[Ende Abschnitt 3]",
		"++ Abschnitt 4:",
	}, "\n")
	raw = []byte(strings.Replace(string(raw), "++ Abschnitt 3:\n", secondTicket+"\n", 1))

	journeys, err := ParseAll(raw, "two_tickets.eml")
	if err != nil {
		t.Fatal(err)
	}
	if len(journeys) != 2 {
		t.Fatalf("journey count: got %d, want 2", len(journeys))
	}
	assertEqual(t, "second origin", journeys[1].OriginName, "Interlaken Ost")
	assertEqual(t, "second destination", journeys[1].DestinationName, "Bern")
	if journeys[0].SourceHash == journeys[1].SourceHash {
		t.Fatal("tickets from the same email have the same source hash")
	}
	if journeys[0].MessageID == journeys[1].MessageID {
		t.Fatal("tickets from the same email have the same message ID")
	}
}

func assertEqual(t *testing.T, name, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %q, want %q", name, got, want)
	}
}

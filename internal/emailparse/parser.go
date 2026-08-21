package emailparse

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"billett/internal/model"
)

var validityPattern = regexp.MustCompile(`(?i)^g.ltig:\s*(\d{2}\.\d{2}\.\d{4}\s+\d{2}:\d{2})\s*[–-]\s*(\d{2}\.\d{2}\.\d{4}\s+\d{2}:\d{2})$`)
var onlineTicketSectionPattern = regexp.MustCompile(`^\+\+ Abschnitt \d+: Billett \d+$`)

const routeExternalNotice = "Route matching sends origin, destination, journey date, and validity time to transport.opendata.ch. " +
	"If GEOPS_API_KEY is configured, matched stop coordinates are also sent to api.geops.io."

const ParserVersion = "2"

func IsSupportedSubject(subject string) bool {
	key := strings.ToLower(subject)
	return strings.Contains(key, "easyride kaufquittung") ||
		strings.Contains(key, "ihr online-kauf bei der sbb")
}

func Parse(raw []byte, sourceFilename string) (model.Journey, error) {
	journeys, err := ParseAll(raw, sourceFilename)
	if err != nil {
		return model.Journey{}, err
	}
	if len(journeys) != 1 {
		return model.Journey{}, fmt.Errorf("email contains %d journeys", len(journeys))
	}
	return journeys[0], nil
}

func ParseAll(raw []byte, sourceFilename string) ([]model.Journey, error) {
	header, bodyReader, err := readMessage(raw)
	if err != nil {
		return nil, fmt.Errorf("read email: %w", err)
	}

	subject, err := new(mime.WordDecoder).DecodeHeader(header.Get("Subject"))
	if err != nil {
		subject = header.Get("Subject")
	}

	plainParts, htmlParts, err := extractBodies(header, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("extract email body: %w", err)
	}
	body := strings.Join(plainParts, "\n")
	if strings.TrimSpace(body) == "" && len(htmlParts) > 0 {
		body = stripHTML(strings.Join(htmlParts, "\n"))
	}
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("email has no readable body")
	}

	var journeys []model.Journey
	subjectKey := strings.ToLower(subject)
	switch {
	case strings.Contains(subjectKey, "easyride kaufquittung"):
		journey, parseErr := parseEasyRideText(body)
		if parseErr != nil {
			return nil, parseErr
		}
		journeys = []model.Journey{journey}
	case strings.Contains(subjectKey, "ihr online-kauf bei der sbb"):
		journeys, err = parseOnlinePurchaseText(body)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported email subject %q", subject)
	}

	messageID := strings.TrimSpace(header.Get("Message-ID"))
	for index := range journeys {
		hash := sha256.New()
		_, _ = hash.Write(raw)
		if len(journeys) > 1 {
			_, _ = fmt.Fprintf(hash, "\x00billett-ticket-%d", index+1)
			if messageID != "" {
				journeys[index].MessageID = fmt.Sprintf("%s#ticket-%d", messageID, index+1)
			}
		} else {
			journeys[index].MessageID = messageID
		}
		journeys[index].SourceHash = hex.EncodeToString(hash.Sum(nil))
		journeys[index].SourceFilename = sourceFilename
		journeys[index].EmailSubject = subject
		journeys[index].Sender = strings.TrimSpace(header.Get("From"))
		journeys[index].RouteStatus = "pending"
		journeys[index].Currency = "CHF"
	}
	return journeys, nil
}

func parseEasyRideText(body string) (model.Journey, error) {
	lines := normalizedLines(body)
	journeyLine := findLine(lines, func(line string) bool {
		return strings.HasPrefix(line, "Ihre Fahrt vom ")
	})
	if journeyLine < 0 || journeyLine+3 >= len(lines) {
		return model.Journey{}, fmt.Errorf("EasyRide journey block not found")
	}

	journeyDateRaw := strings.TrimPrefix(lines[journeyLine], "Ihre Fahrt vom ")
	journeyDate, err := parseDate(journeyDateRaw)
	if err != nil {
		return model.Journey{}, fmt.Errorf("parse journey date: %w", err)
	}
	if !strings.HasPrefix(lines[journeyLine+1], "Reise von ") || !strings.HasPrefix(lines[journeyLine+2], "nach ") {
		return model.Journey{}, fmt.Errorf("EasyRide origin or destination not found")
	}

	validFrom, validTo := "", ""
	for _, line := range lines {
		matches := validityPattern.FindStringSubmatch(line)
		if len(matches) != 3 {
			continue
		}
		validFrom, err = parseLocalDateTime(matches[1])
		if err != nil {
			return model.Journey{}, fmt.Errorf("parse validity start: %w", err)
		}
		validTo, err = parseLocalDateTime(matches[2])
		if err != nil {
			return model.Journey{}, fmt.Errorf("parse validity end: %w", err)
		}
		break
	}
	if validFrom == "" || validTo == "" {
		return model.Journey{}, fmt.Errorf("EasyRide validity interval not found")
	}

	amount, err := parseCHF(valueAfter(lines, "Betrag"))
	if err != nil {
		return model.Journey{}, err
	}

	articleIndex := indexOf(lines, "Artikel")
	fareProduct, fareDetail := "", ""
	if articleIndex >= 0 && articleIndex+1 < len(lines) {
		fareProduct = lines[articleIndex+1]
		if articleIndex+2 < len(lines) && !strings.HasPrefix(lines[articleIndex+2], "Referenznummer") {
			fareDetail = lines[articleIndex+2]
		}
	}

	reference := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "Referenznummer ") {
			reference = strings.TrimSpace(strings.TrimPrefix(line, "Referenznummer "))
			break
		}
	}

	return model.Journey{
		OrderNumber:         inlineValue(lines, "Bestellnummer"),
		ReferenceNumber:     reference,
		JourneyDate:         journeyDate,
		OriginName:          strings.TrimSpace(strings.TrimPrefix(lines[journeyLine+1], "Reise von ")),
		DestinationName:     strings.TrimSpace(strings.TrimPrefix(lines[journeyLine+2], "nach ")),
		ValidFrom:           validFrom,
		ValidTo:             validTo,
		TravelClass:         strings.TrimSpace(strings.TrimPrefix(lines[journeyLine+3], "Klasse ")),
		FareProduct:         fareProduct,
		FareDetail:          fareDetail,
		FareReduction:       valueAfter(lines, "Tarif"),
		PaymentMethod:       valueAfter(lines, "Zahlungsmittel"),
		PriceCents:          amount,
		EvidenceType:        "observed",
		OccurrenceStatus:    "confirmed",
		TimeSource:          "recorded",
		RouteStatus:         "pending",
		RouteExternalNotice: routeExternalNotice,
	}, nil
}

func parseOnlinePurchaseText(body string) ([]model.Journey, error) {
	lines := normalizedLines(body)
	var sectionStarts []int
	for index, line := range lines {
		if onlineTicketSectionPattern.MatchString(line) {
			sectionStarts = append(sectionStarts, index)
		}
	}
	if len(sectionStarts) == 0 {
		return nil, fmt.Errorf("online purchase ticket block not found")
	}

	journeys := make([]model.Journey, 0, len(sectionStarts))
	for ticketIndex, start := range sectionStarts {
		end := len(lines)
		if ticketIndex+1 < len(sectionStarts) {
			end = sectionStarts[ticketIndex+1]
		}
		for index := start + 1; index < end; index++ {
			if strings.HasPrefix(lines[index], "[Ende Abschnitt ") {
				end = index
				break
			}
		}
		journey, err := parseOnlineTicketBlock(lines[start+1 : end])
		if err != nil {
			return nil, fmt.Errorf("parse online ticket %d: %w", ticketIndex+1, err)
		}
		journeys = append(journeys, journey)
	}
	return journeys, nil
}

func parseOnlineTicketBlock(lines []string) (model.Journey, error) {
	priceIndex := findLine(lines, func(line string) bool { return strings.HasPrefix(line, "CHF ") })
	if priceIndex < 4 {
		return model.Journey{}, fmt.Errorf("ticket product, route, or price not found")
	}
	amount, err := parseCHF(lines[priceIndex])
	if err != nil {
		return model.Journey{}, err
	}

	validityIndex := indexOf(lines, "Gültig:")
	if validityIndex < 0 || validityIndex+2 >= len(lines) {
		return model.Journey{}, fmt.Errorf("ticket validity interval not found")
	}
	validFromRaw := strings.TrimSpace(strings.TrimRight(lines[validityIndex+1], "-– "))
	validToRaw := strings.TrimSpace(lines[validityIndex+2])
	validFrom, err := parseLocalDateTime(validFromRaw)
	if err != nil {
		return model.Journey{}, fmt.Errorf("parse validity start: %w", err)
	}
	validTo, err := parseLocalDateTime(validToRaw)
	if err != nil {
		return model.Journey{}, fmt.Errorf("parse validity end: %w", err)
	}

	fareParts := strings.Split(lines[priceIndex-1], ",")
	for index := range fareParts {
		fareParts[index] = strings.TrimSpace(fareParts[index])
	}
	travelClass := fareParts[0]
	fareDetail, fareReduction := "", ""
	if len(fareParts) > 1 {
		fareDetail = fareParts[1]
	}
	if len(fareParts) > 2 {
		fareReduction = strings.Join(fareParts[2:], ", ")
	}

	return model.Journey{
		OrderNumber:         valueForLabel(lines, "Bestell-Nr.:"),
		ReferenceNumber:     valueForLabel(lines, "Referenz-Nr.:"),
		JourneyDate:         validFrom[:10],
		OriginName:          lines[priceIndex-3],
		DestinationName:     lines[priceIndex-2],
		ValidFrom:           validFrom,
		ValidTo:             validTo,
		TravelClass:         travelClass,
		FareProduct:         lines[priceIndex-4],
		FareDetail:          fareDetail,
		FareReduction:       fareReduction,
		PriceCents:          amount,
		EvidenceType:        "ticket",
		OccurrenceStatus:    "unconfirmed",
		TimeSource:          "validity_window",
		RouteStatus:         "pending",
		RouteTimeEstimated:  true,
		RouteExternalNotice: routeExternalNotice,
	}, nil
}

func extractBodies(header textproto.MIMEHeader, body io.Reader) ([]string, []string, error) {
	reader, err := transferDecodedReader(header.Get("Content-Transfer-Encoding"), body)
	if err != nil {
		return nil, nil, err
	}
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || mediaType == "" {
		mediaType = "text/plain"
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return nil, nil, fmt.Errorf("multipart email has no boundary")
		}
		multipartReader := multipart.NewReader(reader, boundary)
		var plainParts, htmlParts []string
		for {
			part, partErr := multipartReader.NextPart()
			if partErr == io.EOF {
				break
			}
			if partErr != nil {
				return nil, nil, partErr
			}
			plain, rich, nestedErr := extractBodies(part.Header, part)
			part.Close()
			if nestedErr != nil {
				continue
			}
			plainParts = append(plainParts, plain...)
			htmlParts = append(htmlParts, rich...)
		}
		return plainParts, htmlParts, nil
	}

	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, nil, err
	}
	switch mediaType {
	case "text/plain":
		return []string{string(content)}, nil, nil
	case "text/html":
		return nil, []string{string(content)}, nil
	case "message/rfc822":
		nestedHeader, nestedBody, nestedErr := readMessage(content)
		if nestedErr != nil {
			return nil, nil, nestedErr
		}
		return extractBodies(nestedHeader, nestedBody)
	default:
		return nil, nil, nil
	}
}

func readMessage(raw []byte) (textproto.MIMEHeader, io.Reader, error) {
	reader := bufio.NewReader(bytes.NewReader(raw))
	header, err := textproto.NewReader(reader).ReadMIMEHeader()
	if err != nil {
		return nil, nil, err
	}
	return header, reader, nil
}

func transferDecodedReader(encoding string, body io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		return quotedprintable.NewReader(body), nil
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, body), nil
	default:
		return body, nil
	}
}

func normalizedLines(body string) []string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func inlineValue(lines []string, label string) string {
	for _, line := range lines {
		if strings.HasPrefix(line, label+" ") {
			return strings.TrimSpace(strings.TrimPrefix(line, label+" "))
		}
	}
	return ""
}

func valueAfter(lines []string, label string) string {
	for index, line := range lines {
		if line == label && index+1 < len(lines) {
			return lines[index+1]
		}
		if strings.HasPrefix(line, label+" ") {
			return strings.TrimSpace(strings.TrimPrefix(line, label+" "))
		}
	}
	return ""
}

func valueForLabel(lines []string, label string) string {
	for index, line := range lines {
		if line == label && index+1 < len(lines) {
			return lines[index+1]
		}
		if strings.HasPrefix(line, label) {
			value := strings.TrimSpace(strings.TrimPrefix(line, label))
			if value != "" {
				return value
			}
			if index+1 < len(lines) {
				return lines[index+1]
			}
		}
	}
	return ""
}

func indexOf(lines []string, expected string) int {
	return findLine(lines, func(line string) bool { return line == expected })
}

func findLine(lines []string, predicate func(string) bool) int {
	for index, line := range lines {
		if predicate(line) {
			return index
		}
	}
	return -1
}

func parseDate(value string) (string, error) {
	parsed, err := time.Parse("02.01.2006", strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	return parsed.Format("2006-01-02"), nil
}

func parseLocalDateTime(value string) (string, error) {
	location, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		return "", err
	}
	parsed, err := time.ParseInLocation("02.01.2006 15:04", strings.TrimSpace(value), location)
	if err != nil {
		return "", err
	}
	return parsed.Format(time.RFC3339), nil
}

func parseCHF(value string) (int64, error) {
	value = strings.TrimSpace(strings.TrimPrefix(value, "CHF"))
	value = strings.ReplaceAll(value, "'", "")
	amount, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("parse CHF amount %q: %w", value, err)
	}
	return int64(amount*100 + 0.5), nil
}

var tagPattern = regexp.MustCompile(`<[^>]+>`)

func stripHTML(value string) string {
	value = strings.ReplaceAll(value, "<br>", "\n")
	value = strings.ReplaceAll(value, "<br/>", "\n")
	value = strings.ReplaceAll(value, "</p>", "\n")
	return html.UnescapeString(tagPattern.ReplaceAllString(value, " "))
}

package iiko

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

var correlationIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Only a UUID may leave the error response. Free-text messages/details can
// contain credentials or personal data and are deliberately never retained.
func responseCorrelationID(headers http.Header, payload []byte) string {
	var body struct {
		CorrelationID string `json:"correlationId"`
	}
	if json.Unmarshal(payload, &body) == nil && correlationIDPattern.MatchString(body.CorrelationID) {
		return body.CorrelationID
	}
	for _, name := range []string{"X-Correlation-ID", "Correlation-ID", "X-Request-ID"} {
		value := headers.Get(name)
		if correlationIDPattern.MatchString(value) {
			return value
		}
	}
	return ""
}
func responseKind(contentType string, payload []byte) string {
	value := bytes.TrimSpace(payload)
	if len(value) == 0 {
		return "empty"
	}
	if json.Valid(value) {
		return "json"
	}
	lower := bytes.ToLower(value)
	if bytes.HasPrefix(lower, []byte("<!doctype html")) || bytes.HasPrefix(lower, []byte("<html")) || strings.Contains(strings.ToLower(contentType), "text/html") {
		return "html"
	}
	if strings.Contains(strings.ToLower(contentType), "json") {
		return "invalid_json"
	}
	if strings.HasPrefix(strings.ToLower(contentType), "text/") {
		return "text"
	}
	return "unknown"
}

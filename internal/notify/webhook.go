package notify

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// PersonalShift follows the official Cloud OpenAPI PersonalShiftEvent schema.
// eventTime is an event timestamp in UTC, NOT an actual shift start/end time.
type PersonalShift struct {
	EventType      string `json:"eventType"`
	EventTime      string `json:"eventTime"`
	OrganizationID string `json:"organizationId"`
	CorrelationID  string `json:"correlationId"`
	EventInfo      struct {
		EmployeeID      string  `json:"id"`
		RoleID          *string `json:"roleId"`
		Opened          *bool   `json:"opened"`
		TerminalGroupID string  `json:"terminalGroupId"`
	} `json:"eventInfo"`
}

// Webhook is disabled unless main explicitly binds a loopback server. iiko's
// configured authToken is the literal Authorization header, with no added prefix.
func (s *Service) Webhook() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhooks/iiko", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if s.cfg.WebhookSecret == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(s.cfg.WebhookSecret)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		defer r.Body.Close()
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		var events []PersonalShift
		if err = json.Unmarshal(raw, &events); err != nil || len(events) > 100 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Validate the entire batch before delivery to avoid partially accepting an
		// invalid batch. Unknown event types are acknowledged and ignored.
		for _, event := range events {
			if event.EventType != "PersonalShift" {
				continue
			}
			if !s.validEvent(event) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		// A validated close event wakes the existing daily scheduler. The signal
		// carries no employee data; several events coalesce without a queue of
		// business records. HTTP acknowledgement never waits for OLAP/Telegram.
		if s.Enabled() {
			for _, event := range events {
				if event.EventType == "PersonalShift" && !*event.EventInfo.Opened {
					select {
					case s.wake <- struct{}{}:
					default:
					}
					break
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
func (s *Service) validEvent(event PersonalShift) bool {
	if event.OrganizationID != s.cfg.OrganizationID || event.EventInfo.EmployeeID == "" || event.EventInfo.TerminalGroupID == "" || event.EventInfo.Opened == nil || strings.TrimSpace(event.EventTime) == "" {
		return false
	}
	_, err := time.Parse("2006-01-02 15:04:05.999", event.EventTime)
	return err == nil
}

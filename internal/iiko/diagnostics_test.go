package iiko_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"greenroom-ai/internal/analytics"
	"greenroom-ai/internal/iiko"
)

var _ = Describe("Safe OLAP diagnosis", func() {
	var client *iiko.Client
	var period analytics.Period
	var normal http.HandlerFunc
	var log bytes.Buffer
	var loc *time.Location
	var requests int
	BeforeEach(func() {
		var err error
		loc, err = time.LoadLocation("Europe/Moscow")
		Expect(err).NotTo(HaveOccurred())
		period, err = analytics.NewPeriod("2025-11-10", "2025-11-10", loc)
		Expect(err).NotTo(HaveOccurred())
		log.Reset()
		requests = 0
		normal = func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v2/access_token":
				writeJSON(w, map[string]string{"token": token(time.Now().Add(time.Hour))})
			case "/api/1/organizations":
				writeJSON(w, map[string]any{"organizations": []any{map[string]string{"id": "org"}}})
			case "/api/reporting/v1/olap/columns/get":
				fixture(w, "columns.json")
			case "/api/reporting/v1/olap/get":
				requests++
				writeJSON(w, map[string]any{"data": []any{}})
			default:
				Fail("unexpected operation " + r.URL.Path)
			}
		}
		handler = normal
		client, err = iiko.New(iiko.Config{BaseURL: server.URL, AppID: "app", OrganizationID: "org", ClientSecret: "super-secret", APIKey: "secret-api-key", Location: loc}, server.Client(), slog.New(slog.NewTextHandler(&log, nil)))
		Expect(err).NotTo(HaveOccurred())
	})
	When("the baseline request fails", func() {
		It("performs one heavy request and stops without exposing body secrets", func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/reporting/v1/olap/get" {
					normal(w, r)
					return
				}
				requests++
				Expect(r.Header.Get("Timeout")).To(BeEmpty())
				var req map[string]json.RawMessage
				Expect(json.NewDecoder(r.Body).Decode(&req)).To(Succeed())
				Expect(req).NotTo(HaveKey("buildSummary"))
				var filters []map[string]json.RawMessage
				Expect(json.Unmarshal(req["filters"], &filters)).To(Succeed())
				Expect(filters[0]).NotTo(HaveKey("includeLow"))
				Expect(filters[0]).NotTo(HaveKey("includeHigh"))
				w.WriteHeader(503)
				writeJSON(w, map[string]string{"message": "super-secret secret-api-key customer-data", "correlationId": "12345678-1234-1234-1234-123456789abc"})
			}
			probes, err := client.DiagnoseSales(context.Background(), period)
			Expect(err).NotTo(HaveOccurred())
			Expect(probes).To(HaveLen(1))
			Expect(probes[0].Stage).To(Equal("baseline"))
			Expect(probes[0].Status).To(Equal(503))
			Expect(probes[0].CorrelationID).To(Equal("12345678-1234-1234-1234-123456789abc"))
			Expect(requests).To(Equal(1))
			Expect(probes[0].Error).NotTo(ContainSubstring("super-secret"))
			Expect(probes[0].Error).NotTo(ContainSubstring("customer-data"))
			Expect(log.String()).NotTo(ContainSubstring("secret-api-key"))
		})
	})
	When("both product and real order identities are available", func() {
		It("progressively probes fields and finishes with exact application requests", func() {
			bodies := []map[string]json.RawMessage{}
			timeouts := []string{}
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/reporting/v1/olap/columns/get" {
					data, err := os.ReadFile("testdata/columns.json")
					Expect(err).NotTo(HaveOccurred())
					var columns []iiko.Column
					Expect(json.Unmarshal(data, &columns)).To(Succeed())
					columns = append(columns, iiko.Column{Name: "DishId", Type: "ID", CanGroup: true}, iiko.Column{Name: "UniqOrderId.Id", Type: "ID", DisplayName: "Order ID", CanGroup: true})
					writeJSON(w, columns)
					return
				}
				if r.URL.Path != "/api/reporting/v1/olap/get" {
					normal(w, r)
					return
				}
				requests++
				var req map[string]json.RawMessage
				Expect(json.NewDecoder(r.Body).Decode(&req)).To(Succeed())
				bodies = append(bodies, req)
				timeouts = append(timeouts, r.Header.Get("Timeout"))
				writeJSON(w, map[string]any{"data": []any{map[string]string{"private": "business-data-never-retained"}}})
			}
			probes, err := client.DiagnoseSales(context.Background(), period)
			Expect(err).NotTo(HaveOccurred())
			Expect(probes).To(HaveLen(8))
			Expect(requests).To(Equal(8))
			stages := []string{}
			for _, probe := range probes {
				stages = append(stages, probe.Stage)
				Expect(probe.Error).To(BeEmpty())
				Expect(probe.Status).To(Equal(200))
				Expect(probe.Rows).To(Equal(1))
			}
			Expect(stages).To(Equal([]string{"baseline", "quantity", "menu_groups", "product_identity", "order_identity", "receipt_aggregate", "application_detail", "application_receipts"}))
			Expect(probes[0].GroupFields).To(Equal([]string{"OpenDate.Typed"}))
			Expect(probes[3].GroupFields).To(ContainElement("DishId"))
			Expect(probes[4].GroupFields).To(ContainElement("UniqOrderId.Id"))
			Expect(probes[5].GroupFields).To(BeEmpty())
			Expect(probes[6].GroupFields).To(Equal([]string{"OpenDate.Typed", "DishGroup", "DishName", "UniqOrderId.Id", "DishId"}))
			Expect(probes[6].AggregateFields).To(Equal([]string{"DishAmountInt", "DishDiscountSumInt"}))
			Expect(timeouts).To(Equal([]string{"", "", "", "", "", "", "40", "40"}))
			Expect(bodies[6]["buildSummary"]).To(MatchJSON("false"))
			var filters []map[string]json.RawMessage
			Expect(json.Unmarshal(bodies[6]["filters"], &filters)).To(Succeed())
			Expect(filters[0]["includeLow"]).To(MatchJSON("true"))
			Expect(filters[0]["includeHigh"]).To(MatchJSON("false"))
			Expect(log.String()).NotTo(ContainSubstring("business-data-never-retained"))
		})
	})
	When("a nonbaseline stage fails temporarily", func() {
		It("records one failure without retrying the heavy probe", func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/reporting/v1/olap/get" {
					normal(w, r)
					return
				}
				requests++
				if requests == 2 {
					w.WriteHeader(503)
					return
				}
				writeJSON(w, map[string]any{"data": []any{}})
			}
			probes, err := client.DiagnoseSales(context.Background(), period)
			Expect(err).NotTo(HaveOccurred())
			Expect(probes).To(HaveLen(6))
			Expect(probes[1].Status).To(Equal(503))
			Expect(probes[1].Error).NotTo(BeEmpty())
			Expect(requests).To(Equal(6))
		})
	})
	When("the supplied day is incomplete or wider than a day", func() {
		It("rejects it without querying iiko", func() {
			today := time.Now().In(loc)
			day := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
			for _, p := range []analytics.Period{{From: day, To: day.AddDate(0, 0, 1)}, {From: period.From, To: period.To.AddDate(0, 0, 1)}} {
				probes, err := client.DiagnoseSales(context.Background(), p)
				Expect(errors.Is(err, iiko.ErrInvalidDiagnosticPeriod)).To(BeTrue())
				Expect(probes).To(BeNil())
			}
			Expect(requests).To(BeZero())
		})
	})
	When("a response supplies correlation metadata", func() {
		It("accepts only UUIDs from the body or known response headers", func() {
			cases := []struct{ body, header, want string }{
				{body: `{"correlationId":"12345678-1234-1234-1234-123456789abc"}`, want: "12345678-1234-1234-1234-123456789abc"},
				{body: `{"correlationId":"secret-api-key"}`, header: "abcdefab-1234-1234-1234-123456789abc", want: "abcdefab-1234-1234-1234-123456789abc"},
				{body: `{"correlationId":"super-secret"}`, header: "invalid-customer-secret", want: ""},
			}
			for _, tc := range cases {
				// A fresh client avoids successful capability caches between cases.
				current, err := iiko.New(iiko.Config{BaseURL: server.URL, AppID: "app", OrganizationID: "org", ClientSecret: "super-secret", APIKey: "secret-api-key", Location: loc}, server.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
				Expect(err).NotTo(HaveOccurred())
				handler = func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/api/reporting/v1/olap/get" {
						normal(w, r)
						return
					}
					w.Header().Set("X-Correlation-ID", tc.header)
					w.WriteHeader(503)
					_, err := io.WriteString(w, tc.body)
					Expect(err).NotTo(HaveOccurred())
				}
				probes, err := current.DiagnoseSales(context.Background(), period)
				Expect(err).NotTo(HaveOccurred())
				Expect(probes).To(HaveLen(1))
				Expect(probes[0].CorrelationID).To(Equal(tc.want))
				Expect(probes[0].Error).NotTo(ContainSubstring("secret"))
			}
		})
	})
	When("the diagnostic token is rejected", func() {
		It("refreshes it once and never loops indefinitely on 401", func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/reporting/v1/olap/get" {
					normal(w, r)
					return
				}
				requests++
				w.WriteHeader(401)
			}
			probes, err := client.DiagnoseSales(context.Background(), period)
			Expect(err).NotTo(HaveOccurred())
			Expect(probes).To(HaveLen(1))
			Expect(probes[0].Status).To(Equal(401))
			Expect(requests).To(Equal(2))
		})
	})
	When("the normal report fails", func() {
		It("keeps bounded retries and identifies the detailed request in its safe logs", func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/reporting/v1/olap/get" {
					normal(w, r)
					return
				}
				requests++
				w.Header().Set("X-Request-ID", "12345678-1234-1234-1234-123456789abc")
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(503)
				_, err := io.WriteString(w, "<html>super-secret customer</html>")
				Expect(err).NotTo(HaveOccurred())
			}
			_, err := client.Sales(context.Background(), period)
			Expect(errors.Is(err, iiko.ErrUnavailable)).To(BeTrue())
			Expect(requests).To(Equal(4))
			Expect(err.Error()).To(ContainSubstring("sales_detail"))
			Expect(log.String()).To(ContainSubstring("status=503"))
			Expect(log.String()).To(ContainSubstring("backoff_ms="))
			Expect(log.String()).To(ContainSubstring("response_kind=html"))
			Expect(strings.Contains(log.String(), "super-secret")).To(BeFalse())
		})
	})
})

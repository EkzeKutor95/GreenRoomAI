package iiko_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"greenroom-ai/internal/analytics"
	"greenroom-ai/internal/iiko"
)

func workforceColumns() []iiko.Column {
	data, err := os.ReadFile("testdata/columns.json")
	Expect(err).NotTo(HaveOccurred())
	var columns []iiko.Column
	Expect(json.Unmarshal(data, &columns)).To(Succeed())
	for _, column := range []iiko.Column{
		{Name: "Cashier.Id", DisplayName: "ID кассира", Type: "ID_STRING", CanGroup: true},
		{Name: "Cashier", DisplayName: "Кассир", Type: "STRING", CanGroup: true},
		{Name: "SessionID", DisplayName: "ID кассовой смены", Type: "ID", CanGroup: true},
		{Name: "SessionNum", DisplayName: "Номер смены", Type: "INTEGER", CanGroup: true},
		{Name: "OpenTime", DisplayName: "Время открытия", Type: "DATETIME", CanGroup: true},
		{Name: "CloseTime", DisplayName: "Время закрытия", Type: "DATETIME", CanGroup: true},
		{Name: "UniqOrderId.Id", DisplayName: "ID заказа", Type: "ID", CanGroup: true},
	} {
		columns = append(columns, column)
	}
	return columns
}
func workforceRow() map[string]any {
	return map[string]any{"OpenDate.Typed": "2026-11-10", "Cashier.Id": "cashier-1", "Cashier": "Бариста", "SessionID": "session-1", "SessionNum": 25, "OpenTime": "2026-11-10T18:00:00.123", "CloseTime": "2026-11-11T00:55:00.456", "UniqOrderId.Id": "order-1", "UniqOrderId": 1, "DishAmountInt": "1.25", "DishDiscountSumInt": "12.10"}
}

var _ = Describe("Cashier and cash-session sales", func() {
	var client *iiko.Client
	var period analytics.Period
	var loc *time.Location
	var normal http.HandlerFunc
	var rows []map[string]any
	var columns []iiko.Column
	var olapCalls int
	BeforeEach(func() {
		var err error
		loc, err = time.LoadLocation("Europe/Moscow")
		Expect(err).NotTo(HaveOccurred())
		period, err = analytics.NewPeriod("2026-11-10", "2026-11-10", loc)
		Expect(err).NotTo(HaveOccurred())
		columns = workforceColumns()
		rows = []map[string]any{workforceRow()}
		olapCalls = 0
		normal = func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v2/access_token":
				writeJSON(w, map[string]string{"token": token(time.Now().Add(time.Hour))})
			case "/api/1/organizations":
				writeJSON(w, map[string]any{"organizations": []any{map[string]string{"id": "org"}}})
			case "/api/reporting/v1/olap/columns/get":
				writeJSON(w, columns)
			case "/api/reporting/v1/olap/get":
				olapCalls++
				var req struct {
					Groups []string `json:"groupByRowFields"`
				}
				Expect(json.NewDecoder(r.Body).Decode(&req)).To(Succeed())
				Expect(req.Groups).To(ContainElements("Cashier.Id", "SessionID", "OpenTime", "CloseTime", "UniqOrderId.Id"))
				Expect(req.Groups).NotTo(ContainElement("DishName"))
				writeJSON(w, map[string]any{"data": rows})
			default:
				Fail("unexpected per-employee or writable operation " + r.URL.Path)
			}
		}
		handler = normal
		client, err = iiko.New(iiko.Config{BaseURL: server.URL, AppID: "app", OrganizationID: "org", ClientSecret: "secret", APIKey: "key", Location: loc}, server.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		Expect(err).NotTo(HaveOccurred())
	})
	When("different concrete orders include signed return amounts", func() {
		It("adds exact signed amounts and counts concrete orders without per-item sums", func() {
			adjustment := workforceRow()
			adjustment["UniqOrderId.Id"] = "order-2"
			adjustment["DishAmountInt"] = "0.25"
			adjustment["DishDiscountSumInt"] = "0.10"
			returned := workforceRow()
			returned["UniqOrderId.Id"] = "order-3"
			returned["DishAmountInt"] = "-0.5"
			returned["DishDiscountSumInt"] = "-2.10"
			rows = append(rows, adjustment, returned)
			report, err := client.Workforce(context.Background(), period)
			Expect(err).NotTo(HaveOccurred())
			Expect(report.Employees).To(HaveLen(1))
			Expect(report.CashShifts).To(HaveLen(1))
			Expect(report.Employees[0].Receipts).To(Equal(3))
			Expect(report.CashShifts[0].Receipts).To(Equal(3))
			Expect(report.Employees[0].Revenue.String()).To(Equal("10.1"))
			Expect(report.Employees[0].Quantity.String()).To(Equal("1"))
			Expect(report.CashShifts[0].Revenue.String()).To(Equal("10.1"))
			Expect(report.Role).To(Equal("Кассир"))
			Expect(olapCalls).To(Equal(1))
		})
	})
	When("a cash session contains orders after midnight", func() {
		It("retains the real order timestamps in the venue timezone without claiming shift boundaries", func() {
			report, err := client.Workforce(context.Background(), period)
			Expect(err).NotTo(HaveOccurred())
			shift := report.CashShifts[0]
			Expect(shift.FirstOrderOpened.In(loc).Day()).To(Equal(10))
			Expect(shift.LastOrderClosed.In(loc).Day()).To(Equal(11))
			Expect(shift.LastOrderClosed.In(loc).Hour()).To(Equal(0))
			Expect(shift.LastOrderClosed.Sub(*shift.FirstOrderOpened)).To(Equal(6*time.Hour + 55*time.Minute + 333*time.Millisecond))
			Expect(report.Warnings).To(ContainElement(ContainSubstring("не является временем")))
		})
	})
	When("both menus load the same period", func() {
		It("shares one cached batch snapshot and isolates returned collections and timestamp pointers", func() {
			first, err := client.Workforce(context.Background(), period)
			Expect(err).NotTo(HaveOccurred())
			first.Employees[0].Name = "changed"
			first.CashShifts[0].FirstOrderOpened = first.CashShifts[0].LastOrderClosed
			*first.CashShifts[0].LastOrderClosed = time.Now()
			first.Warnings[0] = "changed"
			second, err := client.Workforce(context.Background(), period)
			Expect(err).NotTo(HaveOccurred())
			Expect(olapCalls).To(Equal(1))
			Expect(second.Employees[0].Name).To(Equal("Бариста"))
			Expect(second.CashShifts[0].FirstOrderOpened.In(loc).Day()).To(Equal(10))
			Expect(second.CashShifts[0].LastOrderClosed.In(loc).Format("2006-01-02T15:04:05.000")).To(Equal("2026-11-11T00:55:00.456"))
			Expect(second.Warnings[0]).NotTo(Equal("changed"))
		})
	})
	When("two handlers request a snapshot concurrently", func() {
		It("performs one external batch query", func() {
			var wg sync.WaitGroup
			errorsFound := make(chan error, 2)
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); _, err := client.Workforce(context.Background(), period); errorsFound <- err }()
			}
			wg.Wait()
			close(errorsFound)
			for err := range errorsFound {
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(olapCalls).To(Equal(1))
		})
	})
	When("an accounting period has no sales", func() {
		It("returns empty collections without inventing employees or sessions", func() {
			rows = []map[string]any{}
			report, err := client.Workforce(context.Background(), period)
			Expect(err).NotTo(HaveOccurred())
			Expect(report.Employees).To(BeEmpty())
			Expect(report.CashShifts).To(BeEmpty())
		})
	})
	When("a receipt has no assigned cashier", func() {
		It("retains its revenue in a separately labelled row without inventing an employee ID", func() {
			rows[0]["Cashier.Id"] = nil
			rows[0]["Cashier"] = nil
			report, err := client.Workforce(context.Background(), period)
			Expect(err).NotTo(HaveOccurred())
			Expect(report.Employees).To(HaveLen(1))
			Expect(report.Employees[0].ID).To(BeEmpty())
			Expect(report.Employees[0].Name).To(Equal("Кассир не указан"))
			Expect(report.Employees[0].Revenue.String()).To(Equal("12.1"))
			Expect(report.Warnings).To(ContainElement(ContainSubstring("без ID кассира")))
		})
	})
	When("an order timestamp is explicitly unavailable", func() {
		It("does not present a partial observed timestamp as the session's complete order range", func() {
			rows[0]["CloseTime"] = nil
			report, err := client.Workforce(context.Background(), period)
			Expect(err).NotTo(HaveOccurred())
			Expect(report.CashShifts[0].LastOrderClosed).To(BeNil())
			Expect(report.Warnings).To(ContainElement(ContainSubstring("закрытия части заказов отсутствует")))
		})
	})
	When("the metadata cannot prove concrete order identity", func() {
		It("refuses to infer receipts from aggregate counts or row numbers", func() {
			columns = columns[:len(columns)-1]
			_, err := client.Workforce(context.Background(), period)
			Expect(errors.Is(err, iiko.ErrMissingColumns)).To(BeTrue())
			Expect(olapCalls).To(BeZero())
		})
	})
	When("one concrete order has conflicting assignments", func() {
		It("rejects the whole report rather than attributing the same receipt to different staff", func() {
			other := workforceRow()
			other["Cashier.Id"] = "cashier-2"
			rows = append(rows, other)
			report, err := client.Workforce(context.Background(), period)
			Expect(errors.Is(err, iiko.ErrInvalidResponse)).To(BeTrue())
			Expect(report.Employees).To(BeNil())
		})
	})
	When("the API repeats an identical concrete-order grouping", func() {
		It("rejects the report instead of doubling money while only deduplicating checks", func() {
			rows = append(rows, workforceRow())
			report, err := client.Workforce(context.Background(), period)
			Expect(errors.Is(err, iiko.ErrInvalidResponse)).To(BeTrue())
			Expect(report.Employees).To(BeNil())
		})
	})
	When("some cash sessions have no opening timestamp", func() {
		It("orders known times before unknown times with a stable identity tie-breaker", func() {
			rows[0]["SessionID"] = "z"
			rows[0]["OpenTime"] = "2026-11-10T10:00:00"
			rows[0]["CloseTime"] = "2026-11-10T11:00:00"
			unknown := workforceRow()
			unknown["UniqOrderId.Id"] = "order-2"
			unknown["SessionID"] = "a"
			unknown["OpenTime"] = nil
			later := workforceRow()
			later["UniqOrderId.Id"] = "order-3"
			later["SessionID"] = "m"
			later["OpenTime"] = "2026-11-10T20:00:00"
			rows = append(rows, unknown, later)
			report, err := client.Workforce(context.Background(), period)
			Expect(err).NotTo(HaveOccurred())
			ids := []string{}
			for _, shift := range report.CashShifts {
				ids = append(ids, shift.ID)
			}
			Expect(ids).To(Equal([]string{"m", "z", "a"}))
		})
	})

	When("the aggregate contradicts its concrete order grouping", func() {
		It("rejects a count larger than one instead of multiplying receipts", func() {
			rows[0]["UniqOrderId"] = 2
			_, err := client.Workforce(context.Background(), period)
			Expect(errors.Is(err, iiko.ErrInvalidResponse)).To(BeTrue())
		})
	})
	When("an expected grouped field is missing", func() {
		It("returns an error instead of treating missing data as an unassigned cashier", func() {
			delete(rows[0], "Cashier.Id")
			_, err := client.Workforce(context.Background(), period)
			Expect(errors.Is(err, iiko.ErrInvalidResponse)).To(BeTrue())
		})
	})
	When("the API denies this sales section", func() {
		It("propagates the authorization error without probing a different source", func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/reporting/v1/olap/get" {
					w.WriteHeader(403)
					return
				}
				normal(w, r)
			}
			report, err := client.Workforce(context.Background(), period)
			Expect(errors.Is(err, iiko.ErrPermissionDenied)).To(BeTrue())
			Expect(report.Employees).To(BeNil())
		})
	})
	When("a later date chunk fails", func() {
		It("returns the dependency error without a partial staff report", func() {
			period.To = period.From.AddDate(0, 0, 35)
			calls := 0
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/reporting/v1/olap/get" {
					normal(w, r)
					return
				}
				calls++
				if calls == 2 {
					w.WriteHeader(403)
					return
				}
				writeJSON(w, map[string]any{"data": rows})
			}
			report, err := client.Workforce(context.Background(), period)
			Expect(errors.Is(err, iiko.ErrPermissionDenied)).To(BeTrue())
			Expect(report.Employees).To(BeNil())
			Expect(calls).To(Equal(2))
		})
	})
	When("the request is not a calendar-day interval", func() {
		It("refuses to widen an employee's work interval into a daily report", func() {
			period.From = period.From.Add(time.Hour)
			_, err := client.Workforce(context.Background(), period)
			Expect(errors.Is(err, iiko.ErrUnsupportedInterval)).To(BeTrue())
			Expect(olapCalls).To(BeZero())
		})
	})
})

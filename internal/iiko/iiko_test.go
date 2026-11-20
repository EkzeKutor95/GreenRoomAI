package iiko_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"greenroom-ai/internal/analytics"
	"greenroom-ai/internal/iiko"
)

func token(exp time.Time) string {
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix()))) + ".signature"
}
func writeJSON(w http.ResponseWriter, value any) {
	Expect(json.NewEncoder(w).Encode(value)).To(Succeed())
}
func fixture(w http.ResponseWriter, name string) {
	data, err := os.ReadFile("testdata/" + name)
	Expect(err).NotTo(HaveOccurred())
	_, err = w.Write(data)
	Expect(err).NotTo(HaveOccurred())
}

var server *httptest.Server
var handler http.HandlerFunc
var _ = BeforeSuite(func() {
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer GinkgoRecover(); handler(w, r) }))
})
var _ = AfterSuite(func() { server.Close() })

var _ = Describe("Authenticated read-only requests", func() {
	var client *iiko.Client
	var loc *time.Location
	var authCount int
	var normal func(http.ResponseWriter, *http.Request)
	BeforeEach(func() {
		var err error
		loc, err = time.LoadLocation("Europe/Moscow")
		Expect(err).NotTo(HaveOccurred())
		authCount = 0
		normal = func(w http.ResponseWriter, r *http.Request) {
			Expect(r.Method).To(Equal(http.MethodPost))
			switch r.URL.Path {
			case "/api/v2/access_token":
				authCount++
				writeJSON(w, map[string]string{"token": token(time.Now().Add(time.Hour))})
			case "/api/1/organizations":
				writeJSON(w, map[string]any{"organizations": []any{map[string]string{"id": "org"}}})
			case "/api/reporting/v1/olap/columns/get":
				fixture(w, "columns.json")
			case "/api/employees/v1/employee/list":
				writeJSON(w, map[string]any{"items": []any{}, "totalCount": 0})
			default:
				Fail("unexpected API operation " + r.URL.Path)
			}
		}
		handler = normal
		client, err = iiko.New(iiko.Config{BaseURL: server.URL, AppID: "app", OrganizationID: "org", ClientSecret: "test-secret", APIKey: "test-key", Location: loc}, server.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		Expect(err).NotTo(HaveOccurred())
	})
	When("the token is still valid", func() {
		It("reuses authorization across separate external requests", func() {
			_, err := client.Employees(context.Background())
			Expect(err).NotTo(HaveOccurred())
			_, err = client.Employees(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(authCount).To(Equal(1))
		})
	})
	When("the API rejects an existing token", func() {
		It("authenticates again and retries the read once", func() {
			calls := 0
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/employees/v1/employee/list" {
					calls++
					if calls == 1 {
						w.WriteHeader(401)
						return
					}
				}
				normal(w, r)
			}
			_, err := client.Employees(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(authCount).To(Equal(2))
			Expect(calls).To(Equal(2))
		})
	})
	When("the authorization response contains an expired JWT", func() {
		It("rejects it before using it on business requests", func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, map[string]string{"token": token(time.Now().Add(-time.Hour))})
			}
			_, err := client.Employees(context.Background())
			Expect(errors.Is(err, iiko.ErrInvalidResponse)).To(BeTrue())
		})
	})
	When("a permission error contains confidential upstream text", func() {
		It("preserves the permission classification without exposing response bodies", func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/employees/v1/employee/list" {
					w.WriteHeader(403)
					_, err := w.Write([]byte(`{"message":"test-secret test-key customer"}`))
					Expect(err).NotTo(HaveOccurred())
					return
				}
				normal(w, r)
			}
			_, err := client.Employees(context.Background())
			Expect(errors.Is(err, iiko.ErrPermissionDenied)).To(BeTrue())
			Expect(err.Error()).NotTo(ContainSubstring("test-secret"))
			Expect(err.Error()).NotTo(ContainSubstring("customer"))
			Expect(authCount).To(Equal(1))
		})
	})
	When("a temporary failure clears", func() {
		It("retries the same read and returns the recovered result", func() {
			calls := 0
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/employees/v1/employee/list" {
					calls++
					if calls == 1 {
						w.WriteHeader(503)
						return
					}
				}
				normal(w, r)
			}
			_, err := client.Employees(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(calls).To(Equal(2))
		})
	})
	When("a rate limit provides Retry-After", func() {
		It("honors the server delay and context cancellation", func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/employees/v1/employee/list" {
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(429)
					return
				}
				normal(w, r)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			_, err := client.Employees(ctx)
			Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue())
		})
	})
	When("a permanent validation error occurs", func() {
		It("returns the error without repeated requests", func() {
			calls := 0
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/employees/v1/employee/list" {
					calls++
					w.WriteHeader(400)
					return
				}
				normal(w, r)
			}
			_, err := client.Employees(context.Background())
			Expect(errors.Is(err, iiko.ErrValidation)).To(BeTrue())
			Expect(calls).To(Equal(1))
		})
	})
	When("metadata lacks row-level order identifiers", func() {
		It("loads receipt totals separately and caches the resulting report", func() {
			calls := 0
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/reporting/v1/olap/get" {
					calls++
					var req struct {
						Aggregates []string `json:"aggregateFields"`
						Groups     []string `json:"groupByRowFields"`
					}
					Expect(json.NewDecoder(r.Body).Decode(&req)).To(Succeed())
					if req.Aggregates[0] == "UniqOrderId" {
						Expect(req.Groups).To(BeEmpty())
						writeJSON(w, map[string]any{"data": []any{map[string]int{"UniqOrderId": 2}}})
						return
					}
					fixture(w, "sales.json")
					return
				}
				normal(w, r)
			}
			p, err := analytics.NewPeriod("2026-11-10", "2026-11-10", loc)
			Expect(err).NotTo(HaveOccurred())
			data, err := client.Sales(context.Background(), p)
			Expect(err).NotTo(HaveOccurred())
			Expect(data.ReceiptCount).NotTo(BeNil())
			Expect(*data.ReceiptCount).To(Equal(2))
			Expect(data.Rows[0].Quantity.String()).To(Equal("2.5"))
			Expect(data.Rows[0].Revenue.String()).To(Equal("525.25"))
			Expect(data.OrderIDsAvailable).To(BeFalse())
			data.Rows[0].Name = "changed by caller"
			again, err := client.Sales(context.Background(), p)
			Expect(err).NotTo(HaveOccurred())
			Expect(again.Rows[0].Name).To(Equal("Капучино"))
			Expect(calls).To(Equal(2))
		})
	})
	When("live metadata identifies a real groupable order ID", func() {
		It("retains order identity instead of adding per-item counts", func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/reporting/v1/olap/columns/get":
					data, err := os.ReadFile("testdata/columns.json")
					Expect(err).NotTo(HaveOccurred())
					var columns []iiko.Column
					Expect(json.Unmarshal(data, &columns)).To(Succeed())
					columns = append(columns, iiko.Column{Name: "UniqOrderId.Id", DisplayName: "Order ID", Type: "ID", CanGroup: true})
					writeJSON(w, columns)
				case "/api/reporting/v1/olap/get":
					var request struct {
						Groups     []string `json:"groupByRowFields"`
						Aggregates []string `json:"aggregateFields"`
					}
					Expect(json.NewDecoder(r.Body).Decode(&request)).To(Succeed())
					if len(request.Aggregates) == 1 && request.Aggregates[0] == "UniqOrderId" {
						writeJSON(w, map[string]any{"data": []any{map[string]int{"UniqOrderId": 1}}})
						return
					}
					Expect(request.Groups).To(ContainElement("UniqOrderId.Id"))
					writeJSON(w, map[string]any{"data": []any{map[string]any{"OpenDate.Typed": "2026-11-10T00:00:00", "DishGroup": "Десерты", "DishName": "Брауни", "DishAmountInt": 3, "DishDiscountSumInt": "600.10", "UniqOrderId.Id": "order-a"}}})
				default:
					normal(w, r)
				}
			}
			p, err := analytics.NewPeriod("2026-11-10", "2026-11-10", loc)
			Expect(err).NotTo(HaveOccurred())
			data, err := client.Sales(context.Background(), p)
			Expect(err).NotTo(HaveOccurred())
			Expect(data.OrderIDsAvailable).To(BeTrue())
			Expect(data.Rows[0].OrderID).To(Equal("order-a"))
			Expect(data.ReceiptCount).NotTo(BeNil())
			Expect(*data.ReceiptCount).To(Equal(1))
		})
	})

	When("a result exceeds the documented per-query date window", func() {
		It("partitions it into disjoint windows while retaining total checks", func() {
			calls := 0
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/reporting/v1/olap/get" {
					calls++
					var req struct {
						Filters    []struct{ From, To string }
						Aggregates []string `json:"aggregateFields"`
					}
					Expect(json.NewDecoder(r.Body).Decode(&req)).To(Succeed())
					from, err := time.Parse("2006-01-02", req.Filters[0].From)
					Expect(err).NotTo(HaveOccurred())
					to, err := time.Parse("2006-01-02", req.Filters[0].To)
					Expect(err).NotTo(HaveOccurred())
					Expect(to.Sub(from)).To(BeNumerically("<=", 30*24*time.Hour))
					if req.Aggregates[0] == "UniqOrderId" {
						writeJSON(w, map[string]any{"data": []any{map[string]int{"UniqOrderId": 1}}})
					} else {
						writeJSON(w, map[string]any{"data": []any{map[string]any{"OpenDate.Typed": req.Filters[0].From, "DishGroup": "Напитки", "DishName": "Кофе", "DishAmountInt": 1, "DishDiscountSumInt": 100}}})
					}
					return
				}
				normal(w, r)
			}
			p, err := analytics.NewPeriod("2026-09-01", "2026-10-05", loc)
			Expect(err).NotTo(HaveOccurred())
			data, err := client.Sales(context.Background(), p)
			Expect(err).NotTo(HaveOccurred())
			Expect(*data.ReceiptCount).To(Equal(2))
			Expect(calls).To(Equal(4))
		})
	})
	When("multiple organizations are accessible with one key", func() {
		It("refuses unscoped OLAP rather than mixing venue sales", func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/1/organizations" {
					writeJSON(w, map[string]any{"organizations": []any{map[string]string{"id": "org"}, map[string]string{"id": "other"}}})
					return
				}
				normal(w, r)
			}
			p, err := analytics.NewPeriod("2026-11-10", "2026-11-10", loc)
			Expect(err).NotTo(HaveOccurred())
			_, err = client.Sales(context.Background(), p)
			Expect(errors.Is(err, iiko.ErrOrganizationScope)).To(BeTrue())
		})
	})
	When("an employee interval begins inside a day", func() {
		It("does not widen the interval or issue a misleading daily report", func() {
			p := analytics.ScheduledShift(time.Date(2026, 11, 10, 0, 0, 0, 0, loc), true, loc)
			_, err := client.Sales(context.Background(), p)
			Expect(errors.Is(err, iiko.ErrUnsupportedInterval)).To(BeTrue())
			Expect(authCount).To(BeZero())
		})
	})
	When("a successful HTTP response omits the data array", func() {
		It("rejects malformed reports instead of pretending that sales are zero", func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/reporting/v1/olap/get" {
					writeJSON(w, map[string]any{})
					return
				}
				normal(w, r)
			}
			p, err := analytics.NewPeriod("2026-11-10", "2026-11-10", loc)
			Expect(err).NotTo(HaveOccurred())
			_, err = client.Sales(context.Background(), p)
			Expect(errors.Is(err, iiko.ErrInvalidResponse)).To(BeTrue())
		})
	})
	When("attendance ends after midnight", func() {
		It("retains both confirmed and POS timestamps with their offset", func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/employees/v1/attendance/list" {
					fixture(w, "attendances.json")
					return
				}
				normal(w, r)
			}
			p, err := analytics.NewPeriod("2026-11-10", "2026-11-11", loc)
			Expect(err).NotTo(HaveOccurred())
			rows, err := client.Attendances(context.Background(), "barista-1", p)
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1))
			Expect(rows[0].End.Sub(rows[0].Start)).To(Equal(7*time.Hour + 5*time.Minute))
			Expect(rows[0].Start.In(loc).Day()).To(Equal(10))
			Expect(rows[0].PersonalEnd).NotTo(BeNil())
		})
	})
	When("the employees endpoint paginates", func() {
		It("follows offset and deduplicates identities across pages", func() {
			offsets := []int{}
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/employees/v1/employee/list" {
					var req struct{ Offset int }
					Expect(json.NewDecoder(r.Body).Decode(&req)).To(Succeed())
					offsets = append(offsets, req.Offset)
					items := []iiko.Employee{}
					if req.Offset == 0 {
						for i := 0; i < 100; i++ {
							items = append(items, iiko.Employee{ID: fmt.Sprint(i), Name: "Employee"})
						}
					} else {
						items = append(items, iiko.Employee{ID: "0", Name: "Employee"}, iiko.Employee{ID: "last", Name: "Employee"})
					}
					scoped := make([]map[string]string, 0, len(items))
					for _, item := range items {
						scoped = append(scoped, map[string]string{"id": item.ID, "name": item.Name, "mainOrganizationId": "org"})
					}
					writeJSON(w, map[string]any{"items": scoped, "totalCount": 102})
					return
				}
				normal(w, r)
			}
			employees, err := client.Employees(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(employees).To(HaveLen(101))
			Expect(offsets).To(Equal([]int{0, 100}))
		})
	})
	When("the API caps pages below the requested limit", func() {
		It("uses totalCount to retrieve the remaining employees and includes staff assigned from another primary organization", func() {
			offsets := []int{}
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/employees/v1/employee/list" {
					normal(w, r)
					return
				}
				var req struct{ Offset int }
				Expect(json.NewDecoder(r.Body).Decode(&req)).To(Succeed())
				offsets = append(offsets, req.Offset)
				writeJSON(w, map[string]any{"items": []any{map[string]any{"id": fmt.Sprint(req.Offset), "name": "Barista", "mainOrganizationId": "other", "organizationIds": []string{"org", "other"}}}, "totalCount": 3})
			}
			employees, err := client.Employees(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(employees).To(HaveLen(3))
			Expect(offsets).To(Equal([]int{0, 1, 2}))
		})
	})
	When("a later page fails", func() {
		It("returns the dependency error instead of earlier employees as a complete list", func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/employees/v1/employee/list" {
					normal(w, r)
					return
				}
				var req struct{ Offset int }
				Expect(json.NewDecoder(r.Body).Decode(&req)).To(Succeed())
				if req.Offset > 0 {
					w.WriteHeader(403)
					return
				}
				writeJSON(w, map[string]any{"items": []any{map[string]string{"id": "one", "name": "Barista", "mainOrganizationId": "org"}}, "totalCount": 2})
			}
			employees, err := client.Employees(context.Background())
			Expect(errors.Is(err, iiko.ErrPermissionDenied)).To(BeTrue())
			Expect(employees).To(BeNil())
		})
	})
	When("a paginated endpoint repeats a page", func() {
		It("rejects an incomplete duplicate list", func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/employees/v1/employee/list" {
					normal(w, r)
					return
				}
				writeJSON(w, map[string]any{"items": []any{map[string]string{"id": "one", "name": "Barista", "mainOrganizationId": "org"}}, "totalCount": 2})
			}
			employees, err := client.Employees(context.Background())
			Expect(errors.Is(err, iiko.ErrInvalidResponse)).To(BeTrue())
			Expect(employees).To(BeNil())
		})
	})
	When("a typed date has an invalid suffix or a non-midnight time", func() {
		It("rejects both rather than silently truncating them to a date", func() {
			for _, value := range []string{"2026-11-10garbage", "2026-11-10T18:00:00"} {
				handler = func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/api/reporting/v1/olap/get" {
						normal(w, r)
						return
					}
					writeJSON(w, map[string]any{"data": []any{map[string]any{"OpenDate.Typed": value, "DishGroup": "Напитки", "DishName": "Кофе", "DishAmountInt": 1, "DishDiscountSumInt": 100}}})
				}
				p, err := analytics.NewPeriod("2026-11-10", "2026-11-10", loc)
				Expect(err).NotTo(HaveOccurred())
				_, err = client.Sales(context.Background(), p)
				Expect(errors.Is(err, iiko.ErrInvalidResponse)).To(BeTrue())
			}
		})
	})
	When("calendar bounds are supplied in UTC as the same venue instants", func() {
		It("preserves those instants and queries local calendar dates", func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/reporting/v1/olap/get" {
					normal(w, r)
					return
				}
				var req struct {
					Aggregates []string `json:"aggregateFields"`
					Filters    []struct{ From, To string }
				}
				Expect(json.NewDecoder(r.Body).Decode(&req)).To(Succeed())
				Expect(req.Filters[0].From).To(Equal("2026-11-10"))
				Expect(req.Filters[0].To).To(Equal("2026-11-11"))
				if req.Aggregates[0] == "UniqOrderId" {
					writeJSON(w, map[string]any{"data": []any{map[string]int{"UniqOrderId": 2}}})
				} else {
					fixture(w, "sales.json")
				}
			}
			p, err := analytics.NewPeriod("2026-11-10", "2026-11-10", loc)
			Expect(err).NotTo(HaveOccurred())
			p.From = p.From.UTC()
			p.To = p.To.UTC()
			data, err := client.Sales(context.Background(), p)
			Expect(err).NotTo(HaveOccurred())
			Expect(data.Rows).To(HaveLen(2))
		})
	})

	When("row identities disagree with the independent receipt aggregate", func() {
		It("rejects the report instead of choosing a convenient count", func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/reporting/v1/olap/columns/get" {
					data, err := os.ReadFile("testdata/columns.json")
					Expect(err).NotTo(HaveOccurred())
					var columns []iiko.Column
					Expect(json.Unmarshal(data, &columns)).To(Succeed())
					columns = append(columns, iiko.Column{Name: "actualOrderKey", DisplayName: "ID заказа", Type: "ID", CanGroup: true})
					writeJSON(w, columns)
					return
				}
				if r.URL.Path == "/api/reporting/v1/olap/get" {
					var req struct {
						Aggregates []string `json:"aggregateFields"`
					}
					Expect(json.NewDecoder(r.Body).Decode(&req)).To(Succeed())
					if req.Aggregates[0] == "UniqOrderId" {
						writeJSON(w, map[string]any{"data": []any{map[string]int{"UniqOrderId": 2}}})
					} else {
						writeJSON(w, map[string]any{"data": []any{map[string]any{"OpenDate.Typed": "2026-11-10", "DishGroup": "Напитки", "DishName": "Кофе", "DishAmountInt": 1, "DishDiscountSumInt": 100, "actualOrderKey": "order-a"}}})
					}
					return
				}
				normal(w, r)
			}
			p, err := analytics.NewPeriod("2026-11-10", "2026-11-10", loc)
			Expect(err).NotTo(HaveOccurred())
			_, err = client.Sales(context.Background(), p)
			Expect(errors.Is(err, iiko.ErrInvalidResponse)).To(BeTrue())
		})
	})
	When("an otherwise valid row omits its monetary field", func() {
		It("propagates an invalid-response error instead of zero revenue", func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/reporting/v1/olap/get" {
					normal(w, r)
					return
				}
				writeJSON(w, map[string]any{"data": []any{map[string]any{"OpenDate.Typed": "2026-11-10", "DishGroup": "Напитки", "DishName": "Кофе", "DishAmountInt": 1}}})
			}
			p, err := analytics.NewPeriod("2026-11-10", "2026-11-10", loc)
			Expect(err).NotTo(HaveOccurred())
			_, err = client.Sales(context.Background(), p)
			Expect(errors.Is(err, iiko.ErrInvalidResponse)).To(BeTrue())
		})
	})
	When("a paginated result ends before its advertised total", func() {
		It("rejects incomplete attendances", func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/employees/v1/attendance/list" {
					normal(w, r)
					return
				}
				writeJSON(w, map[string]any{"items": []any{}, "totalCount": 1})
			}
			p, err := analytics.NewPeriod("2026-11-10", "2026-11-10", loc)
			Expect(err).NotTo(HaveOccurred())
			_, err = client.Attendances(context.Background(), "barista-1", p)
			Expect(errors.Is(err, iiko.ErrInvalidResponse)).To(BeTrue())
		})
	})

	When("a transport redirects credentials", func() {
		It("refuses to follow the redirect", func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "employee/list") {
					w.Header().Set("Location", server.URL+"/untrusted")
					w.WriteHeader(307)
					return
				}
				normal(w, r)
			}
			_, err := client.Employees(context.Background())
			Expect(errors.Is(err, iiko.ErrInvalidResponse)).To(BeTrue())
		})
	})
})

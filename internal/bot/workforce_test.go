package bot_test

import (
	"context"
	"fmt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"greenroom-ai/internal/analytics"
	"greenroom-ai/internal/bot"
	"greenroom-ai/internal/iiko"
	"greenroom-ai/internal/telegram"
	"io"
	"log/slog"
	"strings"
	"time"
)

type workforceSource struct {
	calls      int
	report     iiko.WorkforceReport
	err        error
	keepPeriod bool
}

func (w *workforceSource) Workforce(_ context.Context, p analytics.Period) (iiko.WorkforceReport, error) {
	w.calls++
	report := w.report
	if !w.keepPeriod {
		report.Period = p
	}
	return report, w.err
}
func buttonData(k telegram.Keyboard) []string {
	var result []string
	for _, row := range k {
		for _, button := range row {
			result = append(result, button.Data)
		}
	}
	return result
}

var _ = Describe("Workforce sales navigation", func() {
	var m *messenger
	var d *data
	var w *workforceSource
	var app *bot.Bot
	act := func(actions ...string) {
		for _, action := range actions {
			Expect(app.Handle(context.Background(), callback(12, action))).To(Succeed())
		}
	}
	BeforeEach(func() {
		m = &messenger{}
		d = &data{}
		opened := time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC)
		closed := time.Date(2026, 11, 13, 0, 30, 0, 0, time.UTC)
		w = &workforceSource{report: iiko.WorkforceReport{Role: "Кассир", Employees: []iiko.SalesEmployee{{ID: "cashier-1", Name: "Анна", Role: "Кассир", Revenue: analytics.DecimalFromInt(300), Quantity: analytics.DecimalFromInt(3), Receipts: 2}}, CashShifts: []iiko.CashShift{{ID: "cash-1", Number: "42", Revenue: analytics.DecimalFromInt(300), Quantity: analytics.DecimalFromInt(3), Receipts: 2, FirstOrderOpened: &opened, LastOrderClosed: &closed}}, Warnings: []string{"Проверочная оговорка", "Вторая оговорка"}}}
		app = create(m, d, &settings{}, bot.WithSalesWorkforce(w))
	})
	It("offers useful sales and separate personnel paths without querying either on menu entry", func() {
		act("employees")
		Expect(buttonData(m.sent[len(m.sent)-1].keyboard)).To(ContainElements("workforce:employees", "employees:directory"))
		Expect(w.calls).To(BeZero())
		Expect(d.calls).To(BeZero())
		act("shifts")
		Expect(buttonData(m.sent[len(m.sent)-1].keyboard)).To(ContainElements("workforce:shifts", "shifts:personal"))
		Expect(w.calls).To(BeZero())
	})
	It("uses one snapshot for cashier list and details with exact selected dates and role semantics", func() {
		act("employees", "workforce:employees", "custom", "date:2026-11-10", "date:2026-11-12", "staff:employee:cashier-1", "staff:page:0")
		Expect(w.calls).To(Equal(1))
		Expect(d.calls).To(BeZero())
		Expect(d.attendanceCalls).To(BeZero())
		detail := m.sent[len(m.sent)-2].text
		Expect(detail).To(ContainSubstring("10.11.2026 — 12.11.2026"))
		Expect(detail).To(ContainSubstring("Продажи, зарегистрированные кассиром: 300.00 ₽"))
		Expect(detail).To(ContainSubstring("Роль в отчёте: Кассир"))
		Expect(detail).To(ContainSubstring("150.00 ₽"))
		Expect(detail).To(ContainSubstring("не выручка за время личной смены"))
		for _, message := range m.sent[len(m.sent)-3:] {
			Expect(message.text).To(ContainSubstring("Проверочная оговорка"))
			Expect(message.text).To(ContainSubstring("Вторая оговорка"))
		}
	})
	It("shows cash-session totals only for the selected period and labels order timestamps accurately", func() {
		act("shifts", "workforce:shifts", "custom", "date:2026-11-10", "date:2026-11-12", "cash:shift:cash-1", "cash:page:0")
		Expect(w.calls).To(Equal(1))
		detail := m.sent[len(m.sent)-2].text
		Expect(detail).To(ContainSubstring("Кассовая смена №42"))
		Expect(detail).To(ContainSubstring("Период: 10.11.2026 — 12.11.2026"))
		Expect(detail).To(ContainSubstring("Первое открытие заказа в выборке: 12.11.2026 10:00"))
		Expect(detail).To(ContainSubstring("Последнее закрытие заказа в выборке: 13.11.2026 03:30"))
		Expect(detail).To(ContainSubstring("Это время заказов"))
		Expect(detail).To(ContainSubstring("полный итог смены может отличаться"))
		Expect(detail).To(ContainSubstring("Вторая оговорка"))
	})
	It("keeps unassigned cashier sales visible without a fabricated employee selection", func() {
		w.report.Employees = append(w.report.Employees, iiko.SalesEmployee{Name: "Кассир не указан", Role: "Кассир", Revenue: analytics.DecimalFromInt(10), Receipts: 1})
		act("workforce:employees", "p:yesterday")
		last := m.sent[len(m.sent)-1]
		Expect(last.text).To(ContainSubstring("Кассир не указан — 10.00 ₽"))
		Expect(buttonData(last.keyboard)).NotTo(ContainElement("staff:employee:"))
		Expect(w.calls).To(Equal(1))
	})
	It("rejects a forged employee ID without querying per employee", func() {
		act("workforce:employees", "p:yesterday", "staff:employee:foreign")
		Expect(w.calls).To(Equal(1))
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("устарел"))
	})
	It("rejects a forged cash-session ID without querying per session", func() {
		act("workforce:shifts", "p:yesterday", "cash:shift:foreign")
		Expect(w.calls).To(Equal(1))
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("устарел"))
	})
	It("does not expose the cashier snapshot through a cash-session callback", func() {
		act("workforce:employees", "p:yesterday", "cash:shift:cash-1")
		Expect(w.calls).To(Equal(1))
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("устарел"))
	})
	It("paginates loaded snapshots without additional external requests", func() {
		w.report.Employees = nil
		w.report.CashShifts = nil
		for i := 0; i < 12; i++ {
			w.report.Employees = append(w.report.Employees, iiko.SalesEmployee{ID: fmt.Sprintf("staff-%d", i), Name: fmt.Sprintf("Кассир %d", i), Role: "Кассир"})
			w.report.CashShifts = append(w.report.CashShifts, iiko.CashShift{ID: fmt.Sprintf("shift-%d", i), Number: fmt.Sprint(i)})
		}
		act("workforce:employees", "p:yesterday", "staff:page:1")
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("12. Кассир 11"))
		Expect(w.calls).To(Equal(1))
		act("workforce:shifts", "p:yesterday", "cash:page:1")
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("12. Кассовая смена №11"))
		Expect(w.calls).To(Equal(2))
	})
	DescribeTable("rejects invalid pages safely before integer multiplication", func(section, pageAction string) {
		act(section, "p:yesterday", pageAction)
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("устарел"))
		Expect(w.calls).To(Equal(1))
	},
		Entry("negative staff page", "workforce:employees", "staff:page:-1"), Entry("huge staff page", "workforce:employees", "staff:page:9223372036854775807"), Entry("huge cash page", "workforce:shifts", "cash:page:9223372036854775807"), Entry("outside cash page", "workforce:shifts", "cash:page:1"))
	DescribeTable("returns classified source failures without empty-success data or private error text", func(cause error, expected string) {
		w.err = fmt.Errorf("private-staff-upstream: %w", cause)
		act("workforce:employees", "p:yesterday")
		text := m.sent[len(m.sent)-1].text
		Expect(text).To(ContainSubstring(expected))
		Expect(text).NotTo(ContainSubstring("private-staff-upstream"))
		Expect(text).NotTo(ContainSubstring("не найдено"))
		Expect(text).NotTo(ContainSubstring("Анна"))
		Expect(buttonData(m.sent[len(m.sent)-1].keyboard)).To(ContainElement("workforce:employees"))
	}, Entry("authorization", &iiko.APIError{Status: 401, Cause: iiko.ErrUnauthorized}, "HTTP 401"), Entry("permission", iiko.ErrPermissionDenied, "отказала в доступе"), Entry("source outage", &iiko.APIError{Status: 503, Cause: iiko.ErrUnavailable}, "HTTP 503"))
	It("invalidates a successful snapshot before a later period fails", func() {
		act("workforce:employees", "p:yesterday")
		w.err = iiko.ErrUnavailable
		act("p:today", "staff:employee:cashier-1")
		Expect(w.calls).To(Equal(2))
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("устарел"))
	})
	It("does not mix waiter and cashier roles in a cashier report", func() {
		w.report.Employees[0].Role = "Официант"
		act("workforce:employees", "p:yesterday")
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("не соответствует ожидаемому контракту"))
		Expect(m.sent[len(m.sent)-1].text).NotTo(ContainSubstring("Анна"))
	})
	It("returns a clear empty result for a successful source response with no sales", func() {
		w.report.Employees = nil
		act("workforce:employees", "p:yesterday")
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("Продаж по кассирам за период не найдено"))
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("Проверочная оговорка"))
	})
	It("applies private-chat access checks before loading workforce sales", func() {
		Expect(app.Handle(context.Background(), callback(99, "p:yesterday"))).To(Succeed())
		u := callback(12, "workforce:employees")
		u.CallbackQuery.Message.Chat = telegram.Chat{ID: -100, Type: "supergroup"}
		Expect(app.Handle(context.Background(), u)).To(Succeed())
		Expect(w.calls).To(BeZero())
	})
	It("provides useful navigation after a personnel API authorization failure", func() {
		d.err = &iiko.APIError{Operation: "/api/employees/v1/employee/list", Status: 401, Cause: iiko.ErrUnauthorized}
		act("employees:directory")
		last := m.sent[len(m.sent)-1]
		Expect(last.text).To(ContainSubstring("HTTP 401"))
		Expect(buttonData(last.keyboard)).To(ContainElement("workforce:employees"))
		act("employees:0")
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("устарел"))
	})
	DescribeTable("invalidates old directory selections after leaving that section", func(destination string) {
		act("employees:directory", destination, "emp:employee-1")
		Expect(d.attendanceCalls).To(BeZero())
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("устарел"))
	}, Entry("main menu", "home"), Entry("shifts menu", "shifts"), Entry("sales report", "section:reports"), Entry("comparison menu", "compare"), Entry("workforce sales", "workforce:employees"))
	It("invalidates old directory selections after a failed refresh", func() {
		act("employees:directory")
		d.err = iiko.ErrUnavailable
		act("employees:directory", "emp:employee-1")
		Expect(d.attendanceCalls).To(BeZero())
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("устарел"))
	})
	It("rejects huge legacy directory page callbacks without overflowing", func() {
		act("employees:directory", "employees:9223372036854775807")
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("устарел"))
	})
	It("describes one completed-day report and includes actual scheduler diagnostics", func() {
		app = create(m, d, &settings{}, bot.WithSalesWorkforce(w), bot.WithDiagnostics(func() string { return "Отправка после 01:30 Europe/Moscow" }))
		act("settings")
		text := m.sent[len(m.sent)-1].text
		Expect(text).To(ContainSubstring("Один итог за завершённый учётный день"))
		Expect(text).To(ContainSubstring("полный список проданных позиций"))
		Expect(text).To(ContainSubstring("01:30"))
		Expect(strings.ToLower(text)).NotTo(ContainSubstring("планов"))
	})
	It("rejects an explicitly configured nil workforce dependency", func() {
		configured, err := bot.New(bot.Config{AllowedUserIDs: []int64{12}, Location: time.UTC}, m, d, &settings{}, slog.New(slog.NewTextHandler(io.Discard, nil)), bot.WithSalesWorkforce(nil))
		Expect(err).To(MatchError(bot.ErrConfig))
		Expect(configured).To(BeNil())
		Expect(w.calls).To(BeZero())
	})
	It("rejects a snapshot for a different period before exposing its rows", func() {
		w.keepPeriod = true
		act("workforce:employees", "p:yesterday")
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("не соответствует ожидаемому контракту"))
		Expect(m.sent[len(m.sent)-1].text).NotTo(ContainSubstring("Анна"))
	})

})

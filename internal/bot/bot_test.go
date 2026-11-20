package bot_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"greenroom-ai/internal/analytics"
	"greenroom-ai/internal/bot"
	"greenroom-ai/internal/iiko"
	"greenroom-ai/internal/telegram"
)

type message struct {
	text     string
	keyboard telegram.Keyboard
}
type messenger struct {
	sent      []message
	answers   []string
	documents [][]byte
	updates   []telegram.Update
	cancel    context.CancelFunc
}

func (m *messenger) Updates(context.Context, int64) ([]telegram.Update, error) {
	if m.updates != nil {
		updates := m.updates
		m.updates = nil
		return updates, nil
	}
	if m.cancel != nil {
		m.cancel()
	}
	return nil, context.Canceled
}
func (m *messenger) Send(_ context.Context, _ int64, text string, k telegram.Keyboard) error {
	m.sent = append(m.sent, message{text, k})
	return nil
}
func (m *messenger) Document(_ context.Context, _ int64, _ string, data []byte) error {
	m.documents = append(m.documents, data)
	return nil
}
func (m *messenger) Answer(_ context.Context, _ string, text string) error {
	m.answers = append(m.answers, text)
	return nil
}
func (m *messenger) Check(context.Context) error { return nil }

type data struct {
	calls           int
	periods         []analytics.Period
	err             error
	attendanceCalls int
}

func (d *data) Sales(_ context.Context, p analytics.Period) (analytics.Dataset, error) {
	d.calls++
	d.periods = append(d.periods, p)
	count := 1
	return analytics.Dataset{ReceiptCount: &count, Rows: []analytics.Row{{Date: p.From.Format("2006-01-02"), Group: "Напитки", Name: "Кофе", Quantity: analytics.DecimalFromInt(1), Revenue: analytics.DecimalFromInt(100)}}}, d.err
}
func (d *data) Employees(context.Context) ([]iiko.Employee, error) {
	return []iiko.Employee{{ID: "employee-1", Name: "Анна"}}, d.err
}
func (d *data) Attendances(context.Context, string, analytics.Period) ([]iiko.Attendance, error) {
	d.attendanceCalls++
	return nil, d.err
}
func (d *data) Check(context.Context) error { return d.err }

type settings struct{ enabled bool }

func (s *settings) Enabled() bool                 { return s.enabled }
func (s *settings) SetEnabled(enabled bool) error { s.enabled = enabled; return nil }

type cursor struct{ offset int64 }

func (c *cursor) Offset() int64           { return c.offset }
func (c *cursor) SetOffset(v int64) error { c.offset = v; return nil }
func callback(user int64, action string) telegram.Update {
	return telegram.Update{ID: 1, CallbackQuery: &telegram.CallbackQuery{ID: "callback", From: telegram.User{ID: user}, Message: &telegram.Message{Chat: telegram.Chat{ID: user, Type: "private"}}, Data: action}}
}
func create(m *messenger, d *data, s *settings, options ...bot.Option) *bot.Bot {
	loc, err := time.LoadLocation("Europe/Moscow")
	Expect(err).NotTo(HaveOccurred())
	b, err := bot.New(bot.Config{AllowedUserIDs: []int64{12}, Location: loc}, m, d, s, slog.New(slog.NewTextHandler(io.Discard, nil)), options...)
	Expect(err).NotTo(HaveOccurred())
	return b
}

var _ = Describe("Authorized business navigation", func() {
	var m *messenger
	var d *data
	var s *settings
	var b *bot.Bot
	BeforeEach(func() { m = &messenger{}; d = &data{}; s = &settings{}; b = create(m, d, s) })
	When("a user is not allowed", func() {
		It("acknowledges the callback without querying or sending business data", func() {
			Expect(b.Handle(context.Background(), callback(99, "p:today"))).To(Succeed())
			Expect(d.calls).To(BeZero())
			Expect(m.sent).To(BeEmpty())
			Expect(m.answers).To(Equal([]string{"Доступ запрещён."}))
		})
	})
	When("an allowed user interacts from a group", func() {
		It("keeps business reports confined to the private chat", func() {
			u := callback(12, "p:today")
			u.CallbackQuery.Message.Chat = telegram.Chat{ID: -100, Type: "supergroup"}
			Expect(b.Handle(context.Background(), u)).To(Succeed())
			Expect(d.calls).To(BeZero())
			Expect(m.sent).To(BeEmpty())
		})
	})
	It("renders all requested Russian menu sections", func() {
		u := telegram.Update{Message: &telegram.Message{From: &telegram.User{ID: 12}, Chat: telegram.Chat{ID: 12, Type: "private"}, Text: "/start"}}
		Expect(b.Handle(context.Background(), u)).To(Succeed())
		Expect(m.sent).To(HaveLen(1))
		var labels []string
		for _, row := range m.sent[0].keyboard {
			for _, button := range row {
				labels = append(labels, button.Text)
			}
		}
		Expect(labels).To(ContainElements("Отчёты", "Сотрудники", "Смены", "Проданные позиции", "Десерты", "Сравнение периодов", "Настройки автоматических отчётов", "Распознать фото", "Состояние системы"))
	})
	It("queries the exact inclusive custom dates and exports their full CSV", func() {
		for _, action := range []string{"section:reports", "custom", "date:2026-11-10", "date:2026-11-13", "csv"} {
			Expect(b.Handle(context.Background(), callback(12, action))).To(Succeed())
		}
		Expect(d.calls).To(Equal(1))
		Expect(d.periods[0].From.Format(time.RFC3339)).To(Equal("2026-11-10T00:00:00+03:00"))
		Expect(d.periods[0].To.Format(time.RFC3339)).To(Equal("2026-11-14T00:00:00+03:00"))
		Expect(m.documents).To(HaveLen(1))
		Expect(string(m.documents[0])).To(ContainSubstring("Кофе"))
	})
	It("selects two independent calendar periods for comparison", func() {
		for _, action := range []string{"compare", "cmp:custom", "date:2026-10-01", "date:2026-10-05", "date:2026-11-01", "date:2026-11-05"} {
			Expect(b.Handle(context.Background(), callback(12, action))).To(Succeed())
		}
		Expect(d.calls).To(Equal(2))
		Expect(d.periods[0].From.Month()).To(Equal(time.October))
		Expect(d.periods[1].From.Month()).To(Equal(time.November))
	})
	It("rejects an employee ID that was never offered to the user", func() {
		Expect(b.Handle(context.Background(), callback(12, "emp:foreign"))).To(Succeed())
		Expect(d.attendanceCalls).To(BeZero())
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("устарел"))
	})
	It("labels attendance records separately from confirmed personal shifts", func() {
		for _, action := range []string{"employees:directory", "emp:employee-1", "p:month"} {
			Expect(b.Handle(context.Background(), callback(12, action))).To(Succeed())
		}
		Expect(d.attendanceCalls).To(Equal(1))
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("не подтверждают закрытие личной смены"))
		Expect(d.calls).To(BeZero())
	})
	It("explains missing intraday capability without inventing shift totals", func() {
		d.err = iiko.ErrUnsupportedInterval
		Expect(b.Handle(context.Background(), callback(12, "cmp:shifts"))).To(Succeed())
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("Они не получены"))
		Expect(m.sent[len(m.sent)-1].text).NotTo(ContainSubstring("0 ₽"))
	})
	It("keeps API error text out of user-facing diagnostics", func() {
		d.err = iiko.ErrPermissionDenied
		Expect(b.Handle(context.Background(), callback(12, "employees:directory"))).To(Succeed())
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("Проверьте права"))
	})
	DescribeTable("explains classified dependency failures without exposing wrapped text",
		func(cause error, expected string) {
			d.err = fmt.Errorf("secret-upstream-value: %w", cause)
			Expect(b.Handle(context.Background(), callback(12, "p:today"))).To(Succeed())
			text := m.sent[len(m.sent)-1].text
			Expect(text).To(ContainSubstring(expected))
			Expect(text).NotTo(ContainSubstring("secret-upstream-value"))
			Expect(m.documents).To(BeEmpty())
		},
		Entry("reporting service failure", &iiko.APIError{Operation: "/api/reporting/v1/olap/columns/get", Status: 503, Cause: iiko.ErrUnavailable}, "Сервис отчётности iiko недоступен (HTTP 503)"),
		Entry("authorization rejected after refresh", &iiko.APIError{Operation: "/api/employees/v1/employee/list", Status: 401, Cause: iiko.ErrUnauthorized}, "после повторного получения токена"),
		Entry("token credentials rejected", &iiko.APIError{Operation: "/api/v2/access_token", Status: 401, Cause: iiko.ErrUnauthorized}, "отклонила получение токена"),
		Entry("request rate limited", &iiko.APIError{Operation: "/api/reporting/v1/olap/get", Status: 429, Cause: iiko.ErrUnavailable}, "HTTP 429"),
		Entry("network unavailable", iiko.ErrUnavailable, "соединение с интернетом"),
		Entry("columns unavailable", iiko.ErrMissingColumns, "обязательные поля OLAP"),
		Entry("response contract mismatch", iiko.ErrInvalidResponse, "не соответствует ожидаемому контракту"),
		Entry("deadline exceeded", context.DeadlineExceeded, "за отведённое время"),
	)
	It("limits a successful health check to the organization endpoint actually checked", func() {
		u := telegram.Update{Message: &telegram.Message{From: &telegram.User{ID: 12}, Chat: telegram.Chat{ID: 12, Type: "private"}, Text: "/status"}}
		Expect(b.Handle(context.Background(), u)).To(Succeed())
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("Доступ к OLAP и сотрудникам проверяется отдельно"))
		Expect(d.calls).To(BeZero())
	})
	It("shows a classified failure when the organization health check fails", func() {
		d.err = &iiko.APIError{Operation: "/api/1/organizations", Status: 503, Cause: iiko.ErrUnavailable}
		u := telegram.Update{Message: &telegram.Message{From: &telegram.User{ID: 12}, Chat: telegram.Chat{ID: 12, Type: "private"}, Text: "/status"}}
		Expect(b.Handle(context.Background(), u)).To(Succeed())
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("HTTP 503"))
	})

	It("persists automatic report preferences through the settings dependency", func() {
		Expect(b.Handle(context.Background(), callback(12, "settings:on"))).To(Succeed())
		Expect(s.enabled).To(BeTrue())
		Expect(b.Handle(context.Background(), callback(12, "settings:off"))).To(Succeed())
		Expect(s.enabled).To(BeFalse())
	})
	It("advances only the technical polling cursor after processing an update", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m.cancel = cancel
		m.updates = []telegram.Update{callback(12, "home")}
		c := &cursor{offset: 1}
		b = create(m, d, s, bot.WithCursor(c))
		Expect(b.Run(ctx)).To(Succeed())
		Expect(c.offset).To(Equal(int64(2)))
		Expect(strings.Join(m.answers, "")).To(BeEmpty())
	})
	It("does not export a stale report after a later source failure", func() {
		Expect(b.Handle(context.Background(), callback(12, "p:today"))).To(Succeed())
		d.err = iiko.ErrUnavailable
		Expect(b.Handle(context.Background(), callback(12, "p:yesterday"))).To(Succeed())
		Expect(b.Handle(context.Background(), callback(12, "csv"))).To(Succeed())
		Expect(m.documents).To(BeEmpty())
		Expect(m.sent[len(m.sent)-1].text).To(ContainSubstring("устарел"))
	})

})

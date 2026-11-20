package notify_test

import (
	"context"
	"errors"
	"fmt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"greenroom-ai/internal/analytics"
	"greenroom-ai/internal/notify"
	"greenroom-ai/internal/state"
	"greenroom-ai/internal/telegram"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type sales struct {
	fail    error
	periods []analytics.Period
	dataset *analytics.Dataset
}

func (s *sales) Sales(_ context.Context, p analytics.Period) (analytics.Dataset, error) {
	s.periods = append(s.periods, p)
	if s.fail != nil {
		return analytics.Dataset{}, s.fail
	}
	if s.dataset != nil {
		return *s.dataset, nil
	}
	count := 0
	return analytics.Dataset{ReceiptCount: &count}, nil
}

type sender struct {
	calls    int
	fail     bool
	failCall int
	texts    []string
	onSend   func()
}

func (s *sender) Send(_ context.Context, _ int64, text string, _ telegram.Keyboard) error {
	s.calls++
	s.texts = append(s.texts, text)
	if s.onSend != nil {
		s.onSend()
	}
	if s.fail || s.calls == s.failCall {
		return errors.New("network unavailable")
	}
	return nil
}

var _ = Describe("Automatic notification delivery", func() {
	var statePath string
	var store *state.Store
	var sent *sender
	var data *sales
	var svc *notify.Service
	BeforeEach(func() {
		var err error
		statePath = filepath.Join(GinkgoT().TempDir(), "state.json")
		store, err = state.Open(statePath, true)
		Expect(err).NotTo(HaveOccurred())
		sent = &sender{}
		data = &sales{}
		svc, err = notify.New(notify.Config{ChatID: 1, Location: time.UTC, PollInterval: time.Minute, DailyEnabled: true, DailyHour: 1, RecoveryDays: 1, OrganizationID: "org", WebhookSecret: strings.Repeat("s", 32)}, data, sent, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
		Expect(err).NotTo(HaveOccurred())
	})
	It("sends a notification once even when concurrent triggers compete", func() {
		var wg sync.WaitGroup
		errs := make(chan error, 10)
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); errs <- svc.SendOnce(context.Background(), "key", "report") }()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(sent.calls).To(Equal(1))
	})
	It("retries a failed delivery without persisting false success", func() {
		sent.fail = true
		Expect(svc.SendOnce(context.Background(), "key", "report")).To(HaveOccurred())
		Expect(store.Sent("key/done")).To(BeFalse())
		sent.fail = false
		Expect(svc.SendOnce(context.Background(), "key", "report")).To(Succeed())
		Expect(sent.calls).To(Equal(2))
	})
	It("sends only the latest due daily report without duplicates", func() {
		now := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
		Expect(svc.Tick(context.Background(), now)).To(Succeed())
		calls := sent.calls
		Expect(calls).To(Equal(1))
		Expect(data.periods).To(HaveLen(1))
		Expect(data.periods[0].From.Format("2006-01-02")).To(Equal("2026-11-12"))
		Expect(svc.Tick(context.Background(), now)).To(Succeed())
		Expect(sent.calls).To(Equal(calls))
	})
	It("propagates a source outage without sending a fabricated report", func() {
		data.fail = errors.New("iiko unavailable")
		Expect(svc.Tick(context.Background(), time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC))).To(HaveOccurred())
		Expect(sent.calls).To(Equal(0))
	})
	It("acknowledges authorized closure events without sending a separate shift report", func() {
		body := `[{"eventType":"PersonalShift","eventTime":"2026-11-13 01:00:00.000","organizationId":"org","correlationId":"event","eventInfo":{"id":"employee","opened":false,"terminalGroupId":"terminal"}}]`
		handler := svc.Webhook()
		r := httptest.NewRequest("POST", "/webhooks/iiko", strings.NewReader(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		Expect(w.Code).To(Equal(401))
		Expect(sent.calls).To(Equal(0))
		for i := 0; i < 2; i++ {
			r = httptest.NewRequest("POST", "/webhooks/iiko", strings.NewReader(body))
			r.Header.Set("Authorization", strings.Repeat("s", 32))
			w = httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			Expect(w.Code).To(Equal(200))
		}
		Expect(sent.calls).To(BeZero())
		Expect(data.periods).To(BeEmpty())
	})
	It("does not report closure when the opened flag is missing", func() {
		r := httptest.NewRequest("POST", "/webhooks/iiko", strings.NewReader(`[{"eventType":"PersonalShift","organizationId":"org","eventTime":"2026-11-13 01:00:00.000","eventInfo":{"id":"employee","terminalGroupId":"terminal"}}]`))
		r.Header.Set("Authorization", strings.Repeat("s", 32))
		w := httptest.NewRecorder()
		svc.Webhook().ServeHTTP(w, r)
		Expect(w.Code).To(Equal(400))
		Expect(sent.calls).To(Equal(0))
	})
	It("does not deliver manual or webhook notifications while disabled", func() {
		Expect(svc.SetEnabled(false)).To(Succeed())
		Expect(svc.SendOnce(context.Background(), "disabled", "must not send")).To(Succeed())
		Expect(svc.Tick(context.Background(), time.Now())).To(Succeed())
		body := `[{"eventType":"PersonalShift","eventTime":"2026-11-13 01:00:00.000","organizationId":"org","eventInfo":{"id":"employee","opened":false,"terminalGroupId":"terminal"}}]`
		r := httptest.NewRequest("POST", "/webhooks/iiko", strings.NewReader(body))
		r.Header.Set("Authorization", strings.Repeat("s", 32))
		w := httptest.NewRecorder()
		svc.Webhook().ServeHTTP(w, r)
		Expect(w.Code).To(Equal(200))
		Expect(sent.calls).To(BeZero())
		Expect(data.periods).To(BeEmpty())
		Expect(store.Sent("disabled/done")).To(BeFalse())
	})
	It("stops unsent multipart messages when reports are disabled during delivery", func() {
		sent.onSend = func() { Expect(store.SetEnabled(false)).To(Succeed()) }
		Expect(svc.SendOnce(context.Background(), "key", strings.Repeat("🍰", 4000))).To(Succeed())
		Expect(sent.calls).To(Equal(1))
		Expect(store.Sent("key/done")).To(BeFalse())
	})
	It("labels and versions changed snapshots after a partial delivery", func() {
		sent.failCall = 2
		Expect(svc.SendOnce(context.Background(), "key", strings.Repeat("A", 7000))).To(HaveOccurred())
		Expect(store.Sent("key/done")).To(BeFalse())
		sent.failCall = 0
		Expect(svc.SendOnce(context.Background(), "key", strings.Repeat("B", 7000))).To(Succeed())
		Expect(sent.calls).To(Equal(5))
		Expect(sent.texts[0]).To(ContainSubstring("часть 1/3"))
		Expect(sent.texts[2]).To(ContainSubstring("часть 1/3"))
		Expect(strings.Split(sent.texts[0], "\n")[0]).NotTo(Equal(strings.Split(sent.texts[2], "\n")[0]))
		Expect(svc.SendOnce(context.Background(), "key", strings.Repeat("B", 7000))).To(Succeed())
		Expect(sent.calls).To(Equal(5))
	})
	It("uses a single complete civil date through a daylight saving transition", func() {
		loc, err := time.LoadLocation("America/New_York")
		Expect(err).NotTo(HaveOccurred())
		svc, err = notify.New(notify.Config{ChatID: 1, Location: loc, PollInterval: time.Minute, DailyEnabled: true, DailyHour: 1, RecoveryDays: 31, OrganizationID: "org"}, data, sent, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
		Expect(err).NotTo(HaveOccurred())
		Expect(svc.Tick(context.Background(), time.Date(2026, 11, 2, 3, 0, 0, 0, loc))).To(Succeed())
		Expect(data.periods).To(HaveLen(1))
		Expect(data.periods[0].From.Format("2006-01-02 15:04")).To(Equal("2026-11-01 00:00"))
		Expect(data.periods[0].To.Format("2006-01-02 15:04")).To(Equal("2026-11-02 00:00"))
		Expect(data.periods[0].To.Sub(data.periods[0].From)).To(Equal(25 * time.Hour))
	})
	It("selects only the most recent due date before the civil daily hour on a DST transition", func() {
		loc, err := time.LoadLocation("America/New_York")
		Expect(err).NotTo(HaveOccurred())
		svc, err = notify.New(notify.Config{ChatID: 1, Location: loc, PollInterval: time.Minute, DailyHour: 3, DailyEnabled: true, RecoveryDays: 31, OrganizationID: "org"}, data, sent, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
		Expect(err).NotTo(HaveOccurred())
		Expect(svc.Tick(context.Background(), time.Date(2026, 11, 1, 2, 30, 0, 0, loc))).To(Succeed())
		Expect(data.periods).To(HaveLen(1))
		Expect(data.periods[0].From.Format("2006-01-02")).To(Equal("2026-10-30"))
		Expect(svc.Tick(context.Background(), time.Date(2026, 11, 1, 3, 0, 0, 0, loc))).To(Succeed())
		Expect(data.periods).To(HaveLen(2))
		Expect(data.periods[1].From.Format("2006-01-02")).To(Equal("2026-10-31"))
		Expect(store.Sent("daily/2026-10-31/done")).To(BeTrue())
	})
	It("includes every sold position from the selected day without comparison or weekly queries", func() {
		rows := make([]analytics.Row, 0, 25)
		for i := 1; i <= 25; i++ {
			rows = append(rows, analytics.Row{Date: "2026-11-12", Name: fmt.Sprintf("Позиция %02d", i), Group: "Напитки", Quantity: analytics.DecimalFromInt(int64(i)), Revenue: analytics.DecimalFromInt(int64(i * 10))})
		}
		count := 25
		data.dataset = &analytics.Dataset{Rows: rows, ReceiptCount: &count}
		Expect(svc.Tick(context.Background(), time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC))).To(Succeed())
		Expect(data.periods).To(HaveLen(1))
		text := strings.Join(sent.texts, "\n")
		Expect(text).To(ContainSubstring("Автоматический отчёт за 12.11.2026"))
		Expect(text).To(ContainSubstring("Все позиции:"))
		for i := 1; i <= 25; i++ {
			Expect(text).To(ContainSubstring(fmt.Sprintf("Позиция %02d", i)))
		}
		Expect(text).NotTo(ContainSubstring("ТОП-"))
		Expect(text).NotTo(ContainSubstring("Сравнение"))
		Expect(text).NotTo(ContainSubstring("недели"))
		Expect(text).NotTo(ContainSubstring("Утренняя"))
		Expect(text).NotTo(ContainSubstring("Вечерняя"))
	})
	It("rejects an upstream dataset containing another day instead of mixing its sales into the report", func() {
		count := 2
		data.dataset = &analytics.Dataset{ReceiptCount: &count, Rows: []analytics.Row{{Date: "2026-11-12", Name: "Кофе", Quantity: analytics.DecimalFromInt(1), Revenue: analytics.DecimalFromInt(200)}, {Date: "2026-11-11", Name: "Чужой день", Quantity: analytics.DecimalFromInt(1), Revenue: analytics.DecimalFromInt(900)}}}
		Expect(svc.Tick(context.Background(), time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC))).To(MatchError(ContainSubstring("outside requested period")))
		Expect(sent.calls).To(BeZero())
		Expect(store.Sent("daily/2026-11-12/done")).To(BeFalse())
	})
	It("does not backfill a series of dates after a restart", func() {
		now := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
		Expect(svc.Tick(context.Background(), now)).To(Succeed())
		reopened, err := state.Open(statePath, true)
		Expect(err).NotTo(HaveOccurred())
		svc, err = notify.New(notify.Config{ChatID: 1, Location: time.UTC, PollInterval: time.Minute, DailyEnabled: true, DailyHour: 1, RecoveryDays: 31, OrganizationID: "org"}, data, sent, reopened, slog.New(slog.NewTextHandler(io.Discard, nil)))
		Expect(err).NotTo(HaveOccurred())
		Expect(svc.Tick(context.Background(), now)).To(Succeed())
		Expect(data.periods).To(HaveLen(1))
		Expect(sent.calls).To(Equal(1))
	})
	It("selects the latest due completed day at local midnight and switches only after hour plus delay", func() {
		loc := time.FixedZone("MSK", 3*60*60)
		var err error
		svc, err = notify.New(notify.Config{ChatID: 1, Location: loc, PollInterval: time.Minute, DailyEnabled: true, DailyHour: 1, Delay: 30 * time.Minute, RecoveryDays: 31, OrganizationID: "org"}, data, sent, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
		Expect(err).NotTo(HaveOccurred())
		Expect(svc.Tick(context.Background(), time.Date(2026, 11, 13, 0, 0, 0, 0, loc))).To(Succeed())
		Expect(data.periods).To(HaveLen(1))
		Expect(data.periods[0].From.Format("2006-01-02")).To(Equal("2026-11-11"))
		Expect(svc.Tick(context.Background(), time.Date(2026, 11, 13, 1, 29, 0, 0, loc))).To(Succeed())
		Expect(data.periods).To(HaveLen(1))
		Expect(svc.Tick(context.Background(), time.Date(2026, 11, 13, 1, 30, 0, 0, loc))).To(Succeed())
		Expect(data.periods).To(HaveLen(2))
		Expect(data.periods[1].From.Format("2006-01-02")).To(Equal("2026-11-12"))
		Expect(data.periods[1].To.Format("2006-01-02")).To(Equal("2026-11-13"))
	})
	It("performs no data reads when the daily schedule is disabled", func() {
		var err error
		svc, err = notify.New(notify.Config{ChatID: 1, Location: time.UTC, PollInterval: time.Minute, DailyEnabled: false, DailyHour: 1, RecoveryDays: 31, OrganizationID: "org"}, data, sent, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
		Expect(err).NotTo(HaveOccurred())
		Expect(svc.Tick(context.Background(), time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC))).To(Succeed())
		Expect(data.periods).To(BeEmpty())
		Expect(sent.calls).To(BeZero())
	})
	It("waits until a skipped spring hour has passed and preserves a 23-hour accounting date", func() {
		loc, err := time.LoadLocation("America/New_York")
		Expect(err).NotTo(HaveOccurred())
		svc, err = notify.New(notify.Config{ChatID: 1, Location: loc, PollInterval: time.Minute, DailyEnabled: true, DailyHour: 2, RecoveryDays: 31, OrganizationID: "org"}, data, sent, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
		Expect(err).NotTo(HaveOccurred())
		Expect(svc.Tick(context.Background(), time.Date(2026, 3, 8, 1, 30, 0, 0, loc))).To(Succeed())
		Expect(data.periods).To(HaveLen(1))
		Expect(data.periods[0].From.Format("2006-01-02")).To(Equal("2026-03-06"))
		Expect(svc.Tick(context.Background(), time.Date(2026, 3, 8, 3, 0, 0, 0, loc))).To(Succeed())
		Expect(data.periods).To(HaveLen(2))
		Expect(data.periods[1].From.Format("2006-01-02")).To(Equal("2026-03-07"))
		Expect(svc.Tick(context.Background(), time.Date(2026, 3, 9, 2, 0, 0, 0, loc))).To(Succeed())
		Expect(data.periods).To(HaveLen(3))
		Expect(data.periods[2].From.Format("2006-01-02")).To(Equal("2026-03-08"))
		Expect(data.periods[2].To.Sub(data.periods[2].From)).To(Equal(23 * time.Hour))
	})
	It("validates the complete webhook batch before sending any closure notice", func() {
		body := `[{"eventType":"PersonalShift","eventTime":"2026-11-13 01:00:00.000","organizationId":"org","eventInfo":{"id":"employee","opened":false,"terminalGroupId":"terminal"}},{"eventType":"PersonalShift","organizationId":"foreign","eventTime":"2026-11-13 01:00:00.000","eventInfo":{"id":"employee","opened":false,"terminalGroupId":"terminal"}}]`
		r := httptest.NewRequest("POST", "/webhooks/iiko", strings.NewReader(body))
		r.Header.Set("Authorization", strings.Repeat("s", 32))
		w := httptest.NewRecorder()
		svc.Webhook().ServeHTTP(w, r)
		Expect(w.Code).To(Equal(400))
		Expect(sent.calls).To(BeZero())
	})

})

var _ = Describe("Webhook wakeup of the daily scheduler", func() {
	It("acknowledges a burst and delivers only one current daily report from the existing worker", func() {
		store, err := state.Open(filepath.Join(GinkgoT().TempDir(), "state.json"), true)
		Expect(err).NotTo(HaveOccurred())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sent := &sender{onSend: cancel}
		source := &sales{}
		svc, err := notify.New(notify.Config{ChatID: 1, Location: time.UTC, PollInterval: time.Hour, DailyEnabled: true, DailyHour: 1, OrganizationID: "org", WebhookSecret: strings.Repeat("s", 32)}, source, sent, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
		Expect(err).NotTo(HaveOccurred())
		body := `[{"eventType":"PersonalShift","eventTime":"2026-11-19 01:00:00.000","organizationId":"org","eventInfo":{"id":"employee","opened":false,"terminalGroupId":"terminal"}}]`
		handler := svc.Webhook()
		for i := 0; i < 10; i++ {
			r := httptest.NewRequest("POST", "/webhooks/iiko", strings.NewReader(body))
			r.Header.Set("Authorization", strings.Repeat("s", 32))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			Expect(w.Code).To(Equal(200))
		}
		Expect(sent.calls).To(BeZero())
		Expect(source.periods).To(BeEmpty())
		Expect(svc.Run(ctx)).To(Succeed())
		Expect(sent.calls).To(Equal(1))
		Expect(source.periods).To(HaveLen(1))
		Expect(sent.texts[0]).To(ContainSubstring("Автоматический отчёт за"))
		Expect(sent.texts[0]).NotTo(ContainSubstring("PersonalShift"))
	})
})

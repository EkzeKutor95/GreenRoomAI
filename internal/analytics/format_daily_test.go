package analytics_test

import (
	"errors"
	"fmt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"greenroom-ai/internal/analytics"
	"testing"
	"time"
)

var _ = Describe("Single-date report presentation", func() {
	When("a complete day has more than twenty sold items", func() {
		It("shows that date and every item without a second date or daily trend", func() {
			period, err := analytics.NewPeriod("2026-11-18", "2026-11-18", time.FixedZone("MSK", 3*60*60))
			Expect(err).NotTo(HaveOccurred())
			rows := make([]analytics.Row, 0, 25)
			for i := 1; i <= 25; i++ {
				rows = append(rows, analytics.Row{Date: "2026-11-18", Name: fmt.Sprintf("Позиция %02d", i), Group: "Напитки", OrderID: fmt.Sprintf("order-%d", i), Quantity: analytics.DecimalFromInt(int64(i)), Revenue: analytics.DecimalFromInt(int64(i * 10))})
			}
			report, err := analytics.Aggregate(period, analytics.Dataset{Rows: rows, OrderIDsAvailable: true, Warnings: []string{"Проверить правила учёта возвратов."}}, analytics.DessertRules{})
			Expect(err).NotTo(HaveOccurred())
			text, err := analytics.FormatDaily(report)
			Expect(err).NotTo(HaveOccurred())
			Expect(text).To(HavePrefix("GREEN ROOM · Отчёт за 18.11.2026\n"))
			Expect(text).To(ContainSubstring("Все позиции:"))
			Expect(text).To(ContainSubstring("По группам:"))
			Expect(text).To(ContainSubstring("Чеки: 25"))
			Expect(text).To(ContainSubstring("Десерты:"))
			Expect(text).To(ContainSubstring("Проверить правила учёта возвратов."))
			for i := 1; i <= 25; i++ {
				Expect(text).To(ContainSubstring(fmt.Sprintf("Позиция %02d (Напитки): %d ед. · %d.00 ₽", i, i, i*10)))
			}
			for _, unwanted := range []string{"19.11.2026", "2026-11-18", "конец не включён", "Выручка по дням", "ТОП-"} {
				Expect(text).NotTo(ContainSubstring(unwanted))
			}
		})
	})
	When("the existing period formatter is used", func() {
		It("keeps its interval, top selection and daily subsection", func() {
			report, err := analytics.Aggregate(reportPeriod(), analytics.Dataset{Rows: []analytics.Row{row("a", "Кофе", "Напитки", "1", "100"), row("b", "Какао", "Напитки", "1", "200")}}, analytics.DessertRules{})
			Expect(err).NotTo(HaveOccurred())
			text := analytics.Format(report, 1)
			Expect(text).To(HavePrefix("GREEN ROOM · Продажи\n" + report.Period.String()))
			Expect(text).To(ContainSubstring("ТОП-1 позиций:"))
			Expect(text).To(ContainSubstring("Выручка по дням (OpenDate.Typed):"))
		})
	})
})

func TestDailyReportRequiresExactlyOneCivilDate(t *testing.T) {
	t.Parallel()
	location := time.FixedZone("MSK", 3*60*60)
	midnight := time.Date(2026, 11, 18, 0, 0, 0, 0, location)
	for _, scenario := range []struct {
		name   string
		period analytics.Period
	}{
		{"several dates", analytics.Period{From: midnight, To: midnight.AddDate(0, 0, 2)}},
		{"intraday", analytics.Period{From: midnight.Add(7 * time.Hour), To: midnight.Add(18 * time.Hour)}},
		{"unfinished date", analytics.Period{From: midnight, To: midnight.Add(12 * time.Hour)}},
		{"subsecond offset", analytics.Period{From: midnight.Add(time.Nanosecond), To: midnight.AddDate(0, 0, 1)}},
		{"empty", analytics.Period{From: midnight, To: midnight}},
		{"missing", analytics.Period{}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			text, err := analytics.FormatDaily(analytics.Report{Period: scenario.period})
			if !errors.Is(err, analytics.ErrInvalidPeriod) {
				t.Fatalf("want invalid period, got %v", err)
			}
			if text != "" {
				t.Fatal("invalid interval produced a report")
			}
		})
	}
}
func TestDailyReportAcceptsCivilDatesAcrossClockChanges(t *testing.T) {
	t.Parallel()
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name, date string
		hours      time.Duration
	}{{"spring", "2026-03-08", 23 * time.Hour}, {"autumn", "2026-11-01", 25 * time.Hour}} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			period, err := analytics.NewPeriod(scenario.date, scenario.date, location)
			if err != nil {
				t.Fatal(err)
			}
			if period.To.Sub(period.From) != scenario.hours {
				t.Fatal("fixture does not cross clock change")
			}
			if _, err = analytics.FormatDaily(analytics.Report{Period: period}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

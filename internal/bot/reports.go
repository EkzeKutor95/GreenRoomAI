package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"greenroom-ai/internal/analytics"
	"greenroom-ai/internal/telegram"
)

func (b *Bot) calendar(ctx context.Context, id int64, month time.Time, label string) error {
	first := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, b.cfg.Location)
	days := first.AddDate(0, 1, -1).Day()
	keyboard := telegram.Keyboard{{{Text: "Пн", Data: "noop"}, {Text: "Вт", Data: "noop"}, {Text: "Ср", Data: "noop"}, {Text: "Чт", Data: "noop"}, {Text: "Пт", Data: "noop"}, {Text: "Сб", Data: "noop"}, {Text: "Вс", Data: "noop"}}}
	weekday := (int(first.Weekday()) + 6) % 7
	row := []telegram.Button{}
	for i := 0; i < weekday; i++ {
		row = append(row, telegram.Button{Text: "·", Data: "noop"})
	}
	for day := 1; day <= days; day++ {
		date := time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, b.cfg.Location)
		row = append(row, telegram.Button{Text: strconv.Itoa(day), Data: "date:" + date.Format("2006-01-02")})
		if len(row) == 7 {
			keyboard = append(keyboard, row)
			row = nil
		}
	}
	if len(row) > 0 {
		keyboard = append(keyboard, row)
	}
	keyboard = append(keyboard, []telegram.Button{{Text: "← Месяц", Data: "cal:" + first.AddDate(0, -1, 0).Format("2006-01")}, {Text: "Месяц →", Data: "cal:" + first.AddDate(0, 1, 0).Format("2006-01")}})
	keyboard = append(keyboard, []telegram.Button{{Text: "← Главное меню", Data: "home"}})
	return b.messenger.Send(ctx, id, label+"\n"+first.Format("01.2006"), keyboard)
}

func (b *Bot) dateSelected(ctx context.Context, id int64, s *session, date string) error {
	parsed, err := time.ParseInLocation("2006-01-02", date, b.cfg.Location)
	if err != nil || parsed.Year() < 2000 || parsed.Year() > 2100 {
		return ErrSelection
	}
	if s.start == "" {
		s.start = date
		return b.calendar(ctx, id, parsed, "Начало: "+parsed.Format("02.01.2006")+". Выберите конец (включительно)")
	}
	period, err := analytics.NewPeriod(s.start, date, b.cfg.Location)
	if err != nil {
		return b.messenger.Send(ctx, id, "Конец периода должен быть не раньше начала, а диапазон — не длиннее 366 дней. Выберите конец ещё раз.", telegram.Keyboard{{{Text: "Выбрать конец", Data: "cal:" + parsed.Format("2006-01")}}, {{Text: "← Меню", Data: "home"}}})
	}
	s.start = ""
	if s.section == "compare_custom" && s.first == nil {
		s.first = &period
		return b.calendar(ctx, id, parsed, "Первый период выбран. Теперь выберите начало второго периода")
	}
	if s.section == "compare_custom" {
		first := *s.first
		s.first = nil
		return b.comparePeriods(ctx, id, first, period)
	}
	return b.selectedPeriod(ctx, id, s, period)
}

func validatePeriod(period analytics.Period) error {
	if !period.From.Before(period.To) || period.To.After(period.From.AddDate(0, 0, 366)) {
		return ErrPeriod
	}
	return nil
}
func (b *Bot) fetchReport(ctx context.Context, period analytics.Period) (analytics.Report, error) {
	if err := validatePeriod(period); err != nil {
		return analytics.Report{}, err
	}
	dataset, err := b.data.Sales(ctx, period)
	if err != nil {
		return analytics.Report{}, fmt.Errorf("obtain sales report: %w", err)
	}
	report, err := analytics.Aggregate(period, dataset, b.cfg.DessertRules)
	if err != nil {
		return analytics.Report{}, fmt.Errorf("aggregate sales report: %w", err)
	}
	return report, nil
}
func (b *Bot) selectedPeriod(ctx context.Context, id int64, s *session, period analytics.Period) error {
	s.report = nil
	s.start = ""
	if s.section == "employees" {
		return b.attendances(ctx, id, s, period)
	}
	if s.section == "sales_staff" || s.section == "cash_shifts" {
		return b.workforcePeriod(ctx, id, s, period)
	}
	report, err := b.fetchReport(ctx, period)
	if err != nil {
		return err
	}
	s.report = &report
	text := analytics.Format(report, 10)
	if s.section == "desserts" {
		text = dessertText(report)
	}
	return b.messenger.Send(ctx, id, text, reportKeyboard())
}
func dessertText(r analytics.Report) string {
	receipts := "недоступно"
	with := "недоступно"
	without := "недоступно"
	share := "недоступно"
	if r.Receipts != nil {
		receipts = strconv.Itoa(*r.Receipts)
	}
	if r.DessertReceipts != nil {
		with = strconv.Itoa(*r.DessertReceipts)
	}
	if r.WithoutDessertReceipts != nil {
		without = strconv.Itoa(*r.WithoutDessertReceipts)
	}
	if r.DessertShare != nil {
		share = r.DessertShare.Fixed(2) + "%"
	}
	return fmt.Sprintf("Десерты · %s — %s\nВсего чеков: %s\nЧеков с десертами: %s\nЧеков без десертов: %s\nДоля чеков с десертами: %s\nПродано десертов: %s\nВыручка от десертов: %s ₽\n%s", r.Period.From.Format("02.01.2006"), r.Period.To.AddDate(0, 0, -1).Format("02.01.2006"), receipts, with, without, share, r.DessertQuantity.String(), r.DessertRevenue.Fixed(2), strings.Join(r.Warnings, "\n"))
}
func (b *Bot) comparisonMenu(ctx context.Context, id int64) error {
	return b.messenger.Send(ctx, id, "Сравните периоды. Процент при нулевой базе показывается как недоступный.", telegram.Keyboard{
		{{Text: "7 дней / предыдущие 7", Data: "cmp:7d"}}, {{Text: "30 дней / предыдущие 30", Data: "cmp:30d"}},
		{{Text: "Текущий / предыдущий месяц", Data: "cmp:month"}}, {{Text: "Месяц: завершённые дни", Data: "cmp:month_to_date"}},
		{{Text: "О сравнении личных смен", Data: "cmp:shifts"}}, {{Text: "Два произвольных периода", Data: "cmp:custom"}}, {{Text: "← Главное меню", Data: "home"}},
	})
}
func (b *Bot) comparison(ctx context.Context, id int64, s *session, name string) error {
	s.report = nil
	if name == "custom" {
		s.section = "compare_custom"
		s.start = ""
		s.first = nil
		return b.calendar(ctx, id, time.Now().In(b.cfg.Location), "Первый период: выберите начало")
	}
	if name == "shifts" {
		return b.messenger.Send(ctx, id, "Для сравнения фактических личных смен нужны подтверждённые интервалы работы. Они не получены. Продажи по кассовым сменам доступны в разделе «Смены»; утро и вечер по расписанию не подменяют личные смены.", telegram.Keyboard{{{Text: "Кассовые смены по продажам", Data: "workforce:shifts"}}, {{Text: "← Сравнение периодов", Data: "compare"}}})
	}
	current, previous, err := analytics.ComparablePeriods(name, time.Now(), b.cfg.Location)
	if err != nil {
		return ErrSelection
	}
	return b.comparePeriods(ctx, id, current, previous)
}
func (b *Bot) comparePeriods(ctx context.Context, id int64, current, previous analytics.Period) error {
	first, err := b.fetchReport(ctx, current)
	if err != nil {
		return err
	}
	second, err := b.fetchReport(ctx, previous)
	if err != nil {
		return err
	}
	text := analytics.FormatComparison(analytics.Compare(first, second))
	return b.messenger.Send(ctx, id, text, telegram.Keyboard{{{Text: "Другие периоды", Data: "compare"}}, {{Text: "← Главное меню", Data: "home"}}})
}

package bot

import (
	"context"
	"fmt"
	"greenroom-ai/internal/analytics"
	"greenroom-ai/internal/iiko"
	"greenroom-ai/internal/telegram"
	"strings"
	"time"
)

const workforcePageSize = 10

func (b *Bot) employeesHome(ctx context.Context, id int64, s *session) error {
	s.section = "employees_home"
	s.start = ""
	s.first = nil
	s.report = nil
	s.workforce = nil
	s.employee = nil
	s.employees = nil
	s.directoryLoaded = false
	return b.messenger.Send(ctx, id, "Сотрудники\nПродажи кассиров доступны из OLAP. Кадровый справочник и явки запрашиваются отдельно через API сотрудников.", employeeHomeKeyboard())
}
func employeeHomeKeyboard() telegram.Keyboard {
	return telegram.Keyboard{{{Text: "Кассиры по продажам", Data: "workforce:employees"}}, {{Text: "Кадровый справочник", Data: "employees:directory"}}, {{Text: "← Главное меню", Data: "home"}}}
}
func (b *Bot) shiftsHome(ctx context.Context, id int64, s *session) error {
	s.clearDirectory()
	s.section = "shifts_home"
	s.start = ""
	s.first = nil
	s.report = nil
	s.workforce = nil
	return b.messenger.Send(ctx, id, "Смены\nВыберите кассовые смены по продажам или сведения о личных сменах и явках.", telegram.Keyboard{{{Text: "Кассовые смены по продажам", Data: "workforce:shifts"}}, {{Text: "Личные смены и явки", Data: "shifts:personal"}}, {{Text: "← Главное меню", Data: "home"}}})
}
func (b *Bot) personalShiftInfo(ctx context.Context, id int64) error {
	return b.messenger.Send(ctx, id, "Личные смены и явки\nCloud API показывает текущее состояние личной смены и передаёт события PersonalShift. История фактических интервалов личных смен через чтение API не подтверждена.\nЯвки — сведения табеля; их окончание не выдаётся за закрытие личной смены. Доступ к кадровому справочнику и явкам проверяется отдельно.", telegram.Keyboard{{{Text: "Кадровый справочник и явки", Data: "employees:directory"}}, {{Text: "Кассовые смены по продажам", Data: "workforce:shifts"}}, {{Text: "← Смены", Data: "shifts"}}})
}
func (b *Bot) workforcePeriods(ctx context.Context, id int64, s *session, section string) error {
	s.clearDirectory()
	s.section = section
	s.start = ""
	s.first = nil
	s.report = nil
	s.workforce = nil
	s.employee = nil
	return b.periodMenu(ctx, id)
}
func (b *Bot) workforcePeriod(ctx context.Context, id int64, s *session, p analytics.Period) error {
	s.workforce = nil
	if err := validatePeriod(p); err != nil {
		return err
	}
	if b.workforceSource == nil {
		return b.messenger.Send(ctx, id, "Аналитика кассиров и кассовых смен не подключена в этой конфигурации приложения.", homeKeyboard())
	}
	report, err := b.workforceSource.Workforce(ctx, p)
	if err != nil {
		return fmt.Errorf("load workforce sales: %w", err)
	}
	if !report.Period.From.Equal(p.From) || !report.Period.To.Equal(p.To) || report.Role != "Кассир" {
		return fmt.Errorf("workforce scope or role: %w", iiko.ErrInvalidResponse)
	}
	for _, employee := range report.Employees {
		if employee.Role != report.Role {
			return fmt.Errorf("mixed workforce roles: %w", iiko.ErrInvalidResponse)
		}
	}
	s.workforce = &report
	if s.section == "sales_staff" {
		return b.salesStaff(ctx, id, s, 0)
	}
	return b.cashShifts(ctx, id, s, 0)
}

// Validate before multiplication, including forged callbacks with huge page numbers.
func pageBounds(count, page int) (int, int, error) {
	if page < 0 || (count == 0 && page != 0) || (count > 0 && page > (count-1)/workforcePageSize) {
		return 0, 0, ErrSelection
	}
	start := page * workforcePageSize
	end := count
	if count-start > workforcePageSize {
		end = start + workforcePageSize
	}
	return start, end, nil
}
func pageNavigation(page, count int, prefix string) []telegram.Button {
	var row []telegram.Button
	if page > 0 {
		row = append(row, telegram.Button{Text: "← Назад", Data: fmt.Sprintf("%s%d", prefix, page-1)})
	}
	if count > 0 && page < (count-1)/workforcePageSize {
		row = append(row, telegram.Button{Text: "Далее →", Data: fmt.Sprintf("%s%d", prefix, page+1)})
	}
	return row
}
func periodDates(p analytics.Period) string {
	return p.From.Format("02.01.2006") + " — " + p.To.AddDate(0, 0, -1).Format("02.01.2006")
}
func workforceWarnings(text string, warnings []string) string {
	if len(warnings) > 0 {
		return text + "\n\nПримечания iiko:\n" + strings.Join(warnings, "\n")
	}
	return text
}
func (b *Bot) salesStaff(ctx context.Context, id int64, s *session, page int) error {
	if s.section != "sales_staff" || s.workforce == nil {
		return ErrSelection
	}
	snapshot := s.workforce
	start, end, err := pageBounds(len(snapshot.Employees), page)
	if err != nil {
		return err
	}
	text := "Кассиры по продажам\nПериод: " + periodDates(snapshot.Period) + "\nПродажи, зарегистрированные кассиром в iiko. Это не выручка за время его личной смены.\n"
	keyboard := telegram.Keyboard{}
	if len(snapshot.Employees) == 0 {
		text += "\nПродаж по кассирам за период не найдено."
	}
	for index := start; index < end; index++ {
		employee := snapshot.Employees[index]
		label := employee.Name
		if employee.ID == "" {
			label = "Кассир не указан"
		}
		text += fmt.Sprintf("\n%d. %s — %s ₽; %s ед.; %d чеков", index+1, label, employee.Revenue.Fixed(2), employee.Quantity.String(), employee.Receipts)
		if employee.ID != "" {
			keyboard = append(keyboard, []telegram.Button{{Text: employee.Name, Data: "staff:employee:" + employee.ID}})
		}
	}
	if row := pageNavigation(page, len(snapshot.Employees), "staff:page:"); len(row) > 0 {
		keyboard = append(keyboard, row)
	}
	keyboard = append(keyboard, []telegram.Button{{Text: "Другой период", Data: "periods"}}, []telegram.Button{{Text: "← Сотрудники", Data: "employees"}, {Text: "Меню", Data: "home"}})
	return b.messenger.Send(ctx, id, workforceWarnings(text, snapshot.Warnings), keyboard)
}
func averageReceipt(revenue analytics.Decimal, receipts int) (string, error) {
	if receipts < 0 {
		return "", iiko.ErrInvalidResponse
	}
	if receipts == 0 {
		return "недоступен: чеков нет", nil
	}
	average, err := revenue.Div(analytics.DecimalFromInt(int64(receipts)))
	if err != nil {
		return "", err
	}
	return average.Fixed(2) + " ₽", nil
}
func (b *Bot) salesEmployee(ctx context.Context, id int64, s *session, employeeID string) error {
	if s.section != "sales_staff" || s.workforce == nil || employeeID == "" {
		return ErrSelection
	}
	for _, employee := range s.workforce.Employees {
		if employee.ID != employeeID {
			continue
		}
		average, err := averageReceipt(employee.Revenue, employee.Receipts)
		if err != nil {
			return err
		}
		text := fmt.Sprintf("%s\nРоль в отчёте: %s\nПериод: %s\nПродажи, зарегистрированные кассиром: %s ₽\nПродано единиц: %s\nЧеков: %d\nСредний чек: %s\n\nЭто продажи по роли кассира в iiko, а не выручка за время личной смены и не доказательство того, кто приготовил заказ.", employee.Name, employee.Role, periodDates(s.workforce.Period), employee.Revenue.Fixed(2), employee.Quantity.String(), employee.Receipts, average)
		return b.messenger.Send(ctx, id, workforceWarnings(text, s.workforce.Warnings), telegram.Keyboard{{{Text: "← Все кассиры", Data: "staff:page:0"}}, {{Text: "Другой период", Data: "periods"}}, {{Text: "← Сотрудники", Data: "employees"}}})
	}
	return ErrSelection
}
func (b *Bot) cashShifts(ctx context.Context, id int64, s *session, page int) error {
	if s.section != "cash_shifts" || s.workforce == nil {
		return ErrSelection
	}
	snapshot := s.workforce
	start, end, err := pageBounds(len(snapshot.CashShifts), page)
	if err != nil {
		return err
	}
	text := "Кассовые смены по продажам\nПериод: " + periodDates(snapshot.Period) + "\nПоказатели учитывают только выбранный период. Кассовая смена может включать продажи других дат.\n"
	keyboard := telegram.Keyboard{}
	if len(snapshot.CashShifts) == 0 {
		text += "\nПродаж по кассовым сменам за период не найдено."
	}
	for index := start; index < end; index++ {
		shift := snapshot.CashShifts[index]
		label := cashShiftLabel(shift)
		text += fmt.Sprintf("\n%d. %s — %s ₽; %s ед.; %d чеков", index+1, label, shift.Revenue.Fixed(2), shift.Quantity.String(), shift.Receipts)
		if shift.ID != "" {
			keyboard = append(keyboard, []telegram.Button{{Text: label, Data: "cash:shift:" + shift.ID}})
		}
	}
	if row := pageNavigation(page, len(snapshot.CashShifts), "cash:page:"); len(row) > 0 {
		keyboard = append(keyboard, row)
	}
	keyboard = append(keyboard, []telegram.Button{{Text: "Другой период", Data: "periods"}}, []telegram.Button{{Text: "← Смены", Data: "shifts"}, {Text: "Меню", Data: "home"}})
	return b.messenger.Send(ctx, id, workforceWarnings(text, snapshot.Warnings), keyboard)
}
func cashShiftLabel(shift iiko.CashShift) string {
	if shift.Number != "" {
		return "Кассовая смена №" + shift.Number
	}
	if shift.ID == "" {
		return "Кассовая смена не указана"
	}
	return "Кассовая смена " + shift.ID
}
func (b *Bot) cashShift(ctx context.Context, id int64, s *session, shiftID string) error {
	if s.section != "cash_shifts" || s.workforce == nil || shiftID == "" {
		return ErrSelection
	}
	for _, shift := range s.workforce.CashShifts {
		if shift.ID != shiftID {
			continue
		}
		average, err := averageReceipt(shift.Revenue, shift.Receipts)
		if err != nil {
			return err
		}
		text := fmt.Sprintf("%s\nПериод: %s\nПродажи в выбранном периоде: %s ₽\nПродано единиц: %s\nЧеков: %d\nСредний чек: %s\n", cashShiftLabel(shift), periodDates(s.workforce.Period), shift.Revenue.Fixed(2), shift.Quantity.String(), shift.Receipts, average)
		text += "\nПервое открытие заказа в выборке: " + orderTime(shift.FirstOrderOpened, b.cfg.Location) + "\nПоследнее закрытие заказа в выборке: " + orderTime(shift.LastOrderClosed, b.cfg.Location)
		text += "\n\nЭто время заказов, а не открытие или закрытие кассовой либо личной смены. Показатели ограничены выбранным периодом; полный итог смены может отличаться."
		return b.messenger.Send(ctx, id, workforceWarnings(text, s.workforce.Warnings), telegram.Keyboard{{{Text: "← Все кассовые смены", Data: "cash:page:0"}}, {{Text: "Другой период", Data: "periods"}}, {{Text: "← Смены", Data: "shifts"}}})
	}
	return ErrSelection
}
func orderTime(value *time.Time, loc *time.Location) string {
	if value == nil {
		return "не указано"
	}
	return value.In(loc).Format("02.01.2006 15:04")
}
func (b *Bot) errorKeyboard(s *session) telegram.Keyboard {
	switch s.section {
	case "employees":
		return employeeHomeKeyboard()
	case "sales_staff":
		return telegram.Keyboard{{{Text: "Повторить выбор периода", Data: "workforce:employees"}}, {{Text: "← Сотрудники", Data: "employees"}, {Text: "Меню", Data: "home"}}}
	case "cash_shifts":
		return telegram.Keyboard{{{Text: "Повторить выбор периода", Data: "workforce:shifts"}}, {{Text: "← Смены", Data: "shifts"}, {Text: "Меню", Data: "home"}}}
	default:
		return homeKeyboard()
	}
}

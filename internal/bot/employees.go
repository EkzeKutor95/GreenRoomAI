package bot

import (
	"context"
	"fmt"
	"strings"

	"greenroom-ai/internal/analytics"
	"greenroom-ai/internal/iiko"
	"greenroom-ai/internal/telegram"
)

func (b *Bot) employees(ctx context.Context, id int64, s *session, page int, refresh bool) error {
	if refresh {
		s.employees = nil
		s.directoryLoaded = false
		s.employee = nil
		employees, err := b.data.Employees(ctx)
		if err != nil {
			return fmt.Errorf("list employees: %w", err)
		}
		s.employees = employees
		s.directoryLoaded = true
	}
	if s.section != "employees" || !s.directoryLoaded {
		return ErrSelection
	}
	start, end, err := pageBounds(len(s.employees), page)
	if err != nil {
		return err
	}
	if len(s.employees) == 0 {
		return b.messenger.Send(ctx, id, "iiko вернул пустой список сотрудников для текущего API-ключа.", homeKeyboard())
	}
	keyboard := telegram.Keyboard{}
	for i := start; i < end; i++ {
		employee := s.employees[i]
		keyboard = append(keyboard, []telegram.Button{{Text: employee.Name, Data: "emp:" + employee.ID}})
	}
	nav := []telegram.Button{}
	if page > 0 {
		nav = append(nav, telegram.Button{Text: "← Назад", Data: fmt.Sprintf("employees:%d", page-1)})
	}
	if end < len(s.employees) {
		nav = append(nav, telegram.Button{Text: "Далее →", Data: fmt.Sprintf("employees:%d", page+1)})
	}
	if len(nav) > 0 {
		keyboard = append(keyboard, nav)
	}
	keyboard = append(keyboard, []telegram.Button{{Text: "← Главное меню", Data: "home"}})
	return b.messenger.Send(ctx, id, "Выберите сотрудника. Данные явок не являются историей ввода PIN-кода.", keyboard)
}
func (b *Bot) employee(ctx context.Context, id int64, s *session, employeeID string) error {
	if s.section != "employees" || !s.directoryLoaded || employeeID == "" {
		return ErrSelection
	}
	var selected *iiko.Employee
	for i := range s.employees {
		if s.employees[i].ID == employeeID {
			employee := s.employees[i]
			selected = &employee
			break
		}
	}
	if selected == nil {
		return ErrSelection
	}
	s.employee = selected
	s.section = "employees"
	s.report = nil
	if err := b.messenger.Send(ctx, id, "Сотрудник: "+selected.Name+"\nВыберите период явок. Личные продажи и выручка за время работы — разные метрики; неподтверждённые показатели будут обозначены как недоступные.", nil); err != nil {
		return err
	}
	return b.periodMenu(ctx, id)
}
func (b *Bot) attendances(ctx context.Context, id int64, s *session, period analytics.Period) error {
	if s.section != "employees" || !s.directoryLoaded || s.employee == nil {
		return ErrSelection
	}
	if err := validatePeriod(period); err != nil {
		return err
	}
	records, err := b.data.Attendances(ctx, s.employee.ID, period)
	if err != nil {
		return fmt.Errorf("get employee attendances: %w", err)
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Сотрудник: %s\nЯвки за %s — %s\n", s.employee.Name, period.From.Format("02.01.2006"), period.To.AddDate(0, 0, -1).Format("02.01.2006"))
	if len(records) == 0 {
		text.WriteString("Явок за период не найдено.\n")
	}
	for _, record := range records {
		fmt.Fprintf(&text, "\nЯвка: %s — ", record.Start.In(b.cfg.Location).Format("02.01.2006 15:04"))
		if record.End == nil {
			text.WriteString("окончание не указано\n")
		} else {
			fmt.Fprintf(&text, "%s\n", record.End.In(b.cfg.Location).Format("02.01.2006 15:04"))
		}
		if record.PersonalStart != nil {
			fmt.Fprintf(&text, "Отметка начала на POS: %s\n", record.PersonalStart.In(b.cfg.Location).Format("02.01.2006 15:04"))
		}
		if record.PersonalEnd != nil {
			fmt.Fprintf(&text, "Отметка окончания на POS: %s\n", record.PersonalEnd.In(b.cfg.Location).Format("02.01.2006 15:04"))
		}
	}
	text.WriteString("\nЭто сведения табеля. Они не подтверждают закрытие личной смены на кассе.\nТочные личные интервалы работы не получены, поэтому показатели за личную смену не вычисляются. Продажи, зарегистрированные кассиром, доступны в разделе «Сотрудники → Кассиры по продажам». Отчёты приложения строятся по учётным датам iiko.")
	return b.messenger.Send(ctx, id, text.String(), telegram.Keyboard{{{Text: "Другой период", Data: "periods"}}, {{Text: "Другой сотрудник", Data: "employees"}}, {{Text: "← Главное меню", Data: "home"}}})
}

func (s *session) clearDirectory() { s.employees = nil; s.directoryLoaded = false; s.employee = nil }

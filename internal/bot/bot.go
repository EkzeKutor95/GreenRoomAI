package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"greenroom-ai/internal/analytics"
	"greenroom-ai/internal/iiko"
	"greenroom-ai/internal/telegram"
)

type Messenger interface {
	Updates(context.Context, int64) ([]telegram.Update, error)
	Send(context.Context, int64, string, telegram.Keyboard) error
	Document(context.Context, int64, string, []byte) error
	Answer(context.Context, string, string) error
	Check(context.Context) error
}
type Data interface {
	Sales(context.Context, analytics.Period) (analytics.Dataset, error)
	Employees(context.Context) ([]iiko.Employee, error)
	Attendances(context.Context, string, analytics.Period) ([]iiko.Attendance, error)
	Check(context.Context) error
}
type SalesWorkforce interface {
	Workforce(context.Context, analytics.Period) (iiko.WorkforceReport, error)
}

func WithSalesWorkforce(source SalesWorkforce) Option {
	return func(b *Bot) error {
		if source == nil {
			return ErrConfig
		}
		b.workforceSource = source
		return nil
	}
}

type Settings interface {
	Enabled() bool
	SetEnabled(bool) error
}
type Assistant interface {
	Ask(context.Context, string) (string, error)
}
type Cursor interface {
	Offset() int64
	SetOffset(int64) error
}
type Config struct {
	AllowedUserIDs []int64
	Location       *time.Location
	DessertRules   analytics.DessertRules
}
type Option func(*Bot) error

func WithAssistant(a Assistant) Option {
	return func(b *Bot) error {
		if a == nil {
			return ErrConfig
		}
		b.assistant = a
		return nil
	}
}
func WithDiagnostics(f func() string) Option {
	return func(b *Bot) error {
		if f == nil {
			return ErrConfig
		}
		b.diagnostics = f
		return nil
	}
}
func WithCursor(c Cursor) Option {
	return func(b *Bot) error {
		if c == nil {
			return ErrConfig
		}
		b.cursor = c
		return nil
	}
}

type session struct {
	section         string
	start           string
	first           *analytics.Period
	report          *analytics.Report
	employees       []iiko.Employee
	directoryLoaded bool
	workforce       *iiko.WorkforceReport
	employee        *iiko.Employee
	updated         time.Time
}
type Bot struct {
	cfg             Config
	messenger       Messenger
	data            Data
	workforceSource SalesWorkforce
	settings        Settings
	logger          *slog.Logger
	allowed         map[int64]struct{}
	sessions        map[int64]*session
	assistant       Assistant
	photo           *photoRecognition
	diagnostics     func() string
	cursor          Cursor
	mu              sync.Mutex
}

func New(cfg Config, messenger Messenger, data Data, settings Settings, logger *slog.Logger, options ...Option) (*Bot, error) {
	if cfg.Location == nil || len(cfg.AllowedUserIDs) == 0 || messenger == nil || data == nil || settings == nil || logger == nil {
		return nil, ErrConfig
	}
	b := &Bot{cfg: cfg, messenger: messenger, data: data, settings: settings, logger: logger, allowed: make(map[int64]struct{}), sessions: make(map[int64]*session)}
	for _, id := range cfg.AllowedUserIDs {
		if id <= 0 {
			return nil, ErrConfig
		}
		b.allowed[id] = struct{}{}
	}
	for _, option := range options {
		if option == nil {
			return nil, ErrConfig
		}
		if err := option(b); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func (b *Bot) Run(ctx context.Context) error {
	var offset int64
	if b.cursor != nil {
		offset = b.cursor.Offset()
	}
	backoff := time.Second
	for ctx.Err() == nil {
		updates, err := b.messenger.Updates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var apiError *telegram.APIError
			if errors.As(err, &apiError) && apiError.Code == 409 {
				return fmt.Errorf("another Telegram poller or webhook is active: %w", err)
			}
			delay := backoff
			if errors.As(err, &apiError) && apiError.RetryAfter > delay {
				delay = apiError.RetryAfter
			}
			b.logger.Warn("Telegram polling failed", "error", err)
			if !pause(ctx, delay) {
				return nil
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for _, update := range updates {
			if update.ID < offset {
				continue
			}
			if err := b.Handle(ctx, update); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				b.logger.Error("Telegram update handled with delivery failure", "error", err)
			}
			// Processing is complete even when delivery is uncertain; never blindly resend a message.
			offset = update.ID + 1
			if b.cursor != nil {
				if err := b.cursor.SetOffset(offset); err != nil {
					return fmt.Errorf("persist Telegram cursor: %w", err)
				}
			}
		}
	}
	return nil
}
func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Handle serializes navigation and its in-memory session. Run is a single consumer.
func (b *Bot) Handle(ctx context.Context, u telegram.Update) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	var userID, chatID int64
	var action, text, callbackID string
	if u.CallbackQuery != nil {
		q := u.CallbackQuery
		userID = q.From.ID
		callbackID = q.ID
		action = q.Data
		if q.Message == nil {
			return b.messenger.Answer(ctx, callbackID, "Откройте личный чат с ботом.")
		}
		chatID = q.Message.Chat.ID
		if q.Message.Chat.Type != "private" {
			return b.messenger.Answer(ctx, callbackID, "Доступно только в личном чате.")
		}
	} else if u.Message != nil {
		if u.Message.From == nil || u.Message.Chat.Type != "private" {
			return nil
		}
		userID = u.Message.From.ID
		chatID = u.Message.Chat.ID
		text = strings.TrimSpace(u.Message.Text)
	} else {
		return nil
	}
	if _, ok := b.allowed[userID]; !ok || userID != chatID {
		if callbackID != "" {
			return b.messenger.Answer(ctx, callbackID, "Доступ запрещён.")
		}
		return b.messenger.Send(ctx, chatID, "Доступ запрещён. Попросите владельца добавить ваш Telegram user ID в разрешённый список.", nil)
	}
	if callbackID != "" {
		if err := b.messenger.Answer(ctx, callbackID, ""); err != nil {
			return fmt.Errorf("answer callback: %w", err)
		}
	}
	if u.Message != nil && (len(u.Message.Photo) > 0 || u.Message.Document != nil) {
		return b.photoMessage(ctx, chatID, u.Message)
	}
	now := time.Now()
	s := b.sessions[userID]
	if s == nil || now.Sub(s.updated) > 30*time.Minute {
		s = &session{section: "reports"}
		b.sessions[userID] = s
	}
	s.updated = now
	var err error
	if action != "" {
		err = b.action(ctx, chatID, s, action)
	} else {
		err = b.text(ctx, chatID, s, text)
	}
	if err == nil {
		return nil
	}
	b.logger.Warn("business request failed", "error", err)
	var telegramError *telegram.APIError
	if errors.As(err, &telegramError) || errors.Is(err, telegram.ErrTransport) || errors.Is(err, telegram.ErrResponse) {
		return err
	}
	if text := iikoFailureMessage(err); text != "" {
		return b.messenger.Send(ctx, chatID, text, b.errorKeyboard(s))
	}
	if errors.Is(err, iiko.ErrUnsupportedInterval) {
		return b.messenger.Send(ctx, chatID, "Отчёты приложения строятся по учётным датам iiko. Точные личные интервалы работы не получены. Кассовые смены по продажам доступны в разделе «Смены».", b.errorKeyboard(s))
	}
	if errors.Is(err, iiko.ErrPermissionDenied) {
		return b.messenger.Send(ctx, chatID, "iiko отказала в доступе к этому разделу. Проверьте права API-ключа на сотрудников, явки или отчётность. Частичный отчёт не сформирован.", b.errorKeyboard(s))
	}
	if errors.Is(err, iiko.ErrOrganizationScope) {
		return b.messenger.Send(ctx, chatID, "Для отчётов необходим API-ключ, ограниченный одной настроенной организацией. Проверьте область доступа ключа в iikoWeb.", b.errorKeyboard(s))
	}
	if errors.Is(err, iiko.ErrResultLimit) {
		return b.messenger.Send(ctx, chatID, "Отчёт превысил безопасный объём данных. Выберите более короткий период.", b.errorKeyboard(s))
	}
	if errors.Is(err, ErrSelection) {
		return b.messenger.Send(ctx, chatID, "Выбор устарел. Вернитесь в меню и выберите период заново.", b.errorKeyboard(s))
	}
	if errors.Is(err, ErrPeriod) {
		return b.messenger.Send(ctx, chatID, "Выберите период не длиннее 366 дней.", b.errorKeyboard(s))
	}
	// External error bodies and arbitrary inputs never appear in messages.
	return b.messenger.Send(ctx, chatID, "Не удалось выполнить запрос. Проверьте права iiko и соединение в разделе «Состояние системы». Данные не подменялись нулями. Подробности — в локальном журнале.", b.errorKeyboard(s))
}

func (b *Bot) text(ctx context.Context, chatID int64, s *session, text string) error {
	if text == "/photo" {
		return b.photoHelp(ctx, chatID)
	}
	if text == "/start" || text == "/menu" || text == "" {
		return b.home(ctx, chatID, s)
	}
	if text == "/status" {
		return b.status(ctx, chatID)
	}
	if b.assistant == nil {
		return b.messenger.Send(ctx, chatID, "Выберите раздел кнопками. Для вопросов на русском языке настройте необязательный OPENAI_API_KEY.", homeKeyboard())
	}
	if len([]rune(text)) > 2000 {
		return b.messenger.Send(ctx, chatID, "Сократите вопрос до 2000 символов.", homeKeyboard())
	}
	answer, err := b.assistant.Ask(ctx, text)
	if err != nil {
		return fmt.Errorf("AI question: %w", err)
	}
	return b.messenger.Send(ctx, chatID, answer, homeKeyboard())
}

func (b *Bot) action(ctx context.Context, id int64, s *session, action string) error {
	switch {
	case action == "noop":
		return nil
	case action == "home":
		return b.home(ctx, id, s)
	case action == "photo":
		return b.photoHelp(ctx, id)
	case action == "status":
		return b.status(ctx, id)
	case action == "settings":
		return b.settingsMenu(ctx, id)
	case action == "settings:on" || action == "settings:off":
		if err := b.settings.SetEnabled(action == "settings:on"); err != nil {
			return fmt.Errorf("save automatic report setting: %w", err)
		}
		return b.settingsMenu(ctx, id)
	case action == "employees":
		return b.employeesHome(ctx, id, s)
	case action == "employees:directory":
		s.section = "employees"
		s.report = nil
		s.employee = nil
		s.start = ""
		s.workforce = nil
		return b.employees(ctx, id, s, 0, true)
	case action == "workforce:employees":
		return b.workforcePeriods(ctx, id, s, "sales_staff")
	case action == "workforce:shifts":
		return b.workforcePeriods(ctx, id, s, "cash_shifts")
	case strings.HasPrefix(action, "staff:page:"):
		page, err := strconv.Atoi(strings.TrimPrefix(action, "staff:page:"))
		if err != nil {
			return ErrSelection
		}
		return b.salesStaff(ctx, id, s, page)
	case strings.HasPrefix(action, "staff:employee:"):
		return b.salesEmployee(ctx, id, s, strings.TrimPrefix(action, "staff:employee:"))
	case strings.HasPrefix(action, "cash:page:"):
		page, err := strconv.Atoi(strings.TrimPrefix(action, "cash:page:"))
		if err != nil {
			return ErrSelection
		}
		return b.cashShifts(ctx, id, s, page)
	case strings.HasPrefix(action, "cash:shift:"):
		return b.cashShift(ctx, id, s, strings.TrimPrefix(action, "cash:shift:"))
	case action == "shifts:personal":
		s.section = "personal_info"
		s.clearDirectory()
		s.workforce = nil
		s.report = nil
		s.start = ""
		s.first = nil
		return b.personalShiftInfo(ctx, id)
	case strings.HasPrefix(action, "employees:"):
		page, err := strconv.Atoi(strings.TrimPrefix(action, "employees:"))
		if err != nil {
			return ErrSelection
		}
		return b.employees(ctx, id, s, page, false)
	case strings.HasPrefix(action, "emp:"):
		return b.employee(ctx, id, s, strings.TrimPrefix(action, "emp:"))
	case action == "shifts":
		return b.shiftsHome(ctx, id, s)
	case action == "compare":
		s.clearDirectory()
		s.workforce = nil
		s.report = nil
		s.section = "compare"
		s.first = nil
		return b.comparisonMenu(ctx, id)
	case strings.HasPrefix(action, "cmp:"):
		return b.comparison(ctx, id, s, strings.TrimPrefix(action, "cmp:"))
	case strings.HasPrefix(action, "section:"):
		section := strings.TrimPrefix(action, "section:")
		if section != "reports" && section != "items" && section != "desserts" {
			return ErrSelection
		}
		s.section = section
		s.clearDirectory()
		s.workforce = nil
		s.first = nil
		s.report = nil
		return b.periodMenu(ctx, id)
	case action == "periods":
		return b.periodMenu(ctx, id)
	case strings.HasPrefix(action, "p:"):
		p, err := analytics.Preset(strings.TrimPrefix(action, "p:"), time.Now(), b.cfg.Location)
		if err != nil {
			return ErrSelection
		}
		return b.selectedPeriod(ctx, id, s, p)
	case action == "custom":
		s.start = ""
		return b.calendar(ctx, id, time.Now().In(b.cfg.Location), "Выберите начало периода")
	case strings.HasPrefix(action, "cal:"):
		month, err := time.ParseInLocation("2006-01", strings.TrimPrefix(action, "cal:"), b.cfg.Location)
		if err != nil || month.Year() < 2000 || month.Year() > 2100 {
			return ErrSelection
		}
		label := "Выберите начало периода"
		if s.start != "" {
			label = "Выберите конец периода (включительно)"
		}
		return b.calendar(ctx, id, month, label)
	case strings.HasPrefix(action, "date:"):
		return b.dateSelected(ctx, id, s, strings.TrimPrefix(action, "date:"))
	case action == "csv":
		if s.report == nil {
			return ErrSelection
		}
		data, err := analytics.CSV(*s.report)
		if err != nil {
			return err
		}
		return b.messenger.Document(ctx, id, "greenroom-sales.csv", data)
	case action == "top:10" || action == "top:20":
		if s.report == nil {
			return ErrSelection
		}
		top := 10
		if action == "top:20" {
			top = 20
		}
		return b.messenger.Send(ctx, id, analytics.Format(*s.report, top), reportKeyboard())
	default:
		return ErrSelection
	}
}

func (b *Bot) home(ctx context.Context, id int64, s *session) error {
	s.section = "reports"
	s.report = nil
	s.workforce = nil
	s.clearDirectory()
	s.start = ""
	s.first = nil
	return b.messenger.Send(ctx, id, "GREEN ROOM AI\nВыберите раздел. Все отчёты получают актуальные данные из iiko. Время: "+b.cfg.Location.String()+".", mainKeyboard())
}
func homeKeyboard() telegram.Keyboard {
	return telegram.Keyboard{{{Text: "← Главное меню", Data: "home"}}}
}
func mainKeyboard() telegram.Keyboard {
	return telegram.Keyboard{
		{{Text: "Отчёты", Data: "section:reports"}, {Text: "Сотрудники", Data: "employees"}},
		{{Text: "Смены", Data: "shifts"}, {Text: "Проданные позиции", Data: "section:items"}},
		{{Text: "Десерты", Data: "section:desserts"}, {Text: "Сравнение периодов", Data: "compare"}},
		{{Text: "Настройки автоматических отчётов", Data: "settings"}},
		{{Text: "Распознать фото", Data: "photo"}},
		{{Text: "Состояние системы", Data: "status"}},
	}
}
func (b *Bot) periodMenu(ctx context.Context, id int64) error {
	return b.messenger.Send(ctx, id, "Выберите период отчёта:", telegram.Keyboard{
		{{Text: "Сегодня", Data: "p:today"}, {Text: "Вчера", Data: "p:yesterday"}},
		{{Text: "7 дней", Data: "p:7d"}, {Text: "10 дней", Data: "p:10d"}, {Text: "30 дней", Data: "p:30d"}},
		{{Text: "Текущий месяц", Data: "p:month"}, {Text: "Предыдущий месяц", Data: "p:previous_month"}},
		{{Text: "Произвольные даты", Data: "custom"}}, {{Text: "← Главное меню", Data: "home"}},
	})
}
func reportKeyboard() telegram.Keyboard {
	return telegram.Keyboard{{{Text: "ТОП-10", Data: "top:10"}, {Text: "ТОП-20", Data: "top:20"}, {Text: "CSV полностью", Data: "csv"}}, {{Text: "Другой период", Data: "periods"}, {Text: "← Меню", Data: "home"}}}
}
func (b *Bot) settingsMenu(ctx context.Context, id int64) error {
	status := "выключен"
	if b.settings.Enabled() {
		status = "включён"
	}
	text := "Ежедневный отчёт: " + status + ".\nОдин итог за завершённый учётный день iiko: выручка, чеки, средний чек и полный список проданных позиций.\nЧасовой пояс: " + b.cfg.Location.String() + "."
	if b.diagnostics != nil {
		text += "\n" + b.diagnostics()
	}
	return b.messenger.Send(ctx, id, text, telegram.Keyboard{{{Text: "Включить", Data: "settings:on"}, {Text: "Выключить", Data: "settings:off"}}, {{Text: "← Главное меню", Data: "home"}}})
}
func (b *Bot) status(ctx context.Context, id int64) error {
	checkCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	text := "GREEN ROOM AI\nTelegram: обработчик отвечает.\niiko: "
	if err := b.data.Check(checkCtx); err != nil {
		b.logger.Warn("iiko health check failed", "error", err)
		message := iikoFailureMessage(err)
		if message == "" {
			message = "Базовая проверка организации не пройдена. Подробности — в локальном журнале."
		}
		text += message
	} else {
		text += "получение токена и доступ к настроенной организации проверены. Доступ к OLAP и сотрудникам проверяется отдельно при запросе этих разделов."
	}
	text += "\nЧасовой пояс: " + b.cfg.Location.String() + "."
	if b.diagnostics != nil {
		text += "\n" + b.diagnostics()
	}
	return b.messenger.Send(ctx, id, text, homeKeyboard())
}

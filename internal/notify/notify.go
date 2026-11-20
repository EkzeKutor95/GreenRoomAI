// Package notify delivers one daily report without retaining business data.
package notify

import (
	"context"
	"crypto/sha256"
	"fmt"
	"greenroom-ai/internal/analytics"
	"greenroom-ai/internal/telegram"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type Reports interface {
	Sales(context.Context, analytics.Period) (analytics.Dataset, error)
}
type Sender interface {
	Send(context.Context, int64, string, telegram.Keyboard) error
}
type State interface {
	Enabled() bool
	SetEnabled(bool) error
	Sent(string) bool
	MarkSent(string) error
}
type Config struct {
	ChatID                        int64
	Location                      *time.Location
	PollInterval, Delay           time.Duration
	DailyEnabled                  bool
	DailyHour                     int
	RecoveryDays                  int // Deprecated: daily reports never replay a backlog.
	DessertRules                  analytics.DessertRules
	OrganizationID, WebhookSecret string
}
type Service struct {
	cfg                    Config
	reports                Reports
	sender                 Sender
	state                  State
	log                    *slog.Logger
	wake                   chan struct{}
	tickMu                 sync.Mutex
	sendMu                 sync.Mutex
	healthMu               sync.Mutex
	lastCheck, lastSuccess time.Time
	lastFailed             bool
}

func New(cfg Config, reports Reports, sender Sender, state State, logger *slog.Logger) (*Service, error) {
	if cfg.ChatID == 0 || cfg.Location == nil || cfg.PollInterval <= 0 || cfg.Delay < 0 || cfg.DailyHour < 1 || cfg.DailyHour > 23 || cfg.OrganizationID == "" || reports == nil || sender == nil || state == nil || logger == nil {
		return nil, ErrConfiguration
	}
	return &Service{cfg: cfg, reports: reports, sender: sender, state: state, log: logger, wake: make(chan struct{}, 1)}, nil
}
func (s *Service) Enabled() bool           { return s.state.Enabled() }
func (s *Service) SetEnabled(v bool) error { return s.state.SetEnabled(v) }
func (s *Service) Diagnostics() string {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	status := "ожидание первого цикла"
	if !s.lastCheck.IsZero() {
		status = "последний цикл " + s.lastCheck.In(s.cfg.Location).Format("02.01 15:04")
		if s.lastFailed {
			status += "; ошибка зависимости, будет повтор"
		} else {
			status += "; успешно"
		}
	}
	daily := "включена"
	if !s.cfg.DailyEnabled {
		daily = "отключена в конфигурации DAILY_REPORTS_ENABLED"
	}
	return "Планировщик: " + status + "\nЕжедневная рассылка: " + daily + ".\nОдин отчёт за завершённый учётный день iiko; все позиции и суммы только этой даты.\nВремя отправки: " + fmt.Sprintf("%02d:00", s.cfg.DailyHour) + " + задержка OLAP " + s.cfg.Delay.String() + " (" + s.cfg.Location.String() + "). Старые дни пачкой не отправляются."
}
func (s *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		if s.Enabled() {
			if err := s.Tick(ctx, time.Now()); err != nil && ctx.Err() == nil {
				s.log.Warn("automatic reporting cycle failed", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

// Tick sends only the latest due accounting date. Restarting or enabling
// reports never triggers a backlog of older dates. OpenDate.Typed identifies
// the accounting date; it does not prove an employee or operational shift.
func (s *Service) Tick(ctx context.Context, now time.Time) (err error) {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	s.healthMu.Lock()
	s.lastCheck = now
	s.healthMu.Unlock()
	defer func() {
		s.healthMu.Lock()
		s.lastFailed = err != nil
		if err == nil {
			s.lastSuccess = now
		}
		s.healthMu.Unlock()
	}()
	if !s.Enabled() || !s.cfg.DailyEnabled {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	period := s.latestDueDay(now)
	key := "daily/" + period.From.Format("2006-01-02")
	if s.state.Sent(key + "/done") {
		return nil
	}
	report, err := s.report(ctx, period)
	if err != nil {
		return fmt.Errorf("load daily report: %w", err)
	}
	formatted, err := analytics.FormatDaily(report)
	if err != nil {
		return fmt.Errorf("format daily report: %w", err)
	}
	text := "Автоматический отчёт за " + period.From.Format("02.01.2006") + "\nУчётный день iiko; часовой пояс " + s.cfg.Location.String() + ".\n\n" + formatted
	return s.SendOnce(ctx, key, text)
}

func (s *Service) latestDueDay(now time.Time) analytics.Period {
	// Subtract the ingestion delay before selecting a civil scheduled hour.
	// This keeps local calendar boundaries correct on 23- and 25-hour days.
	ready := now.Add(-s.cfg.Delay).In(s.cfg.Location)
	schedule := s.dailySchedule(ready)
	if schedule.After(ready) {
		schedule = s.dailySchedule(ready.AddDate(0, 0, -1))
	}
	end := time.Date(schedule.Year(), schedule.Month(), schedule.Day(), 0, 0, 0, 0, s.cfg.Location)
	return analytics.Period{From: end.AddDate(0, 0, -1), To: end}
}

func (s *Service) dailySchedule(date time.Time) time.Time {
	schedule := time.Date(date.Year(), date.Month(), date.Day(), s.cfg.DailyHour, 0, 0, 0, s.cfg.Location)
	// A spring clock change can normalize a nonexistent hour backwards.
	// Wait for the first valid hour after it instead of delivering early.
	if schedule.Hour() < s.cfg.DailyHour {
		schedule = schedule.Add(time.Duration(s.cfg.DailyHour-schedule.Hour()) * time.Hour)
	}
	return schedule
}

func (s *Service) report(ctx context.Context, p analytics.Period) (analytics.Report, error) {
	data, err := s.reports.Sales(ctx, p)
	if err != nil {
		return analytics.Report{}, err
	}
	return analytics.Aggregate(p, data, s.cfg.DessertRules)
}
func (s *Service) SendOnce(ctx context.Context, key, text string) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if !s.Enabled() || s.state.Sent(key+"/done") {
		return nil
	}
	// Hashes are technical delivery metadata, never a persisted report body.
	// A changed OLAP snapshot gets a new version so retries cannot silently skip
	// chunks by index from an older report. Every multipart message names its version.
	title := "Отчёт"
	if strings.HasPrefix(key, "daily/") {
		if day, err := time.Parse("2006-01-02", strings.TrimPrefix(key, "daily/")); err == nil {
			title = "Отчёт за " + day.Format("02.01.2006")
		}
	}
	version := eventKey(text)
	chunks := telegram.Chunks(text, 3300)
	for index, chunk := range chunks {
		if !s.Enabled() {
			return nil
		}
		part := fmt.Sprintf("%s/version/%s/part/%d", key, version, index)
		if s.state.Sent(part) {
			continue
		}
		if len(chunks) > 1 {
			chunk = fmt.Sprintf("%s • часть %d/%d • версия %s\n", title, index+1, len(chunks), version[:12]) + chunk
		}
		if err := s.sender.Send(ctx, s.cfg.ChatID, chunk, nil); err != nil {
			return fmt.Errorf("send notification: %w", err)
		}
		if err := s.state.MarkSent(part); err != nil {
			s.log.Error("notification delivered but deduplication state could not be persisted")
			return fmt.Errorf("persist delivered notification: %w", err)
		}
	}
	if err := s.state.MarkSent(key + "/done"); err != nil {
		return fmt.Errorf("persist completed notification: %w", err)
	}
	return nil
}
func eventKey(parts ...string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(parts, "/"))))
}

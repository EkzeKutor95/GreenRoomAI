package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"greenroom-ai/internal/analytics"
	"greenroom-ai/internal/assistant"
	"greenroom-ai/internal/bot"
	"greenroom-ai/internal/config"
	"greenroom-ai/internal/iiko"
	"greenroom-ai/internal/notify"
	"greenroom-ai/internal/state"
	"greenroom-ai/internal/telegram"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("application stopped", "error", err)
		os.Exit(1)
	}
}
func run(logger *slog.Logger) error {
	check := flag.Bool("check", false, "проверить конфигурацию и read-only подключение к iiko/Telegram")
	checkConfig := flag.Bool("check-config", false, "проверить конфигурацию без HTTP-запросов")
	checkWorkforce := flag.Bool("check-workforce", false, "проверить кассиров и кассовые смены OLAP без вывода имён и сумм")
	diagnose := flag.Bool("diagnose-iiko", false, "сравнить минимальные read-only OLAP-запросы без вывода продаж")
	diagnoseDate := flag.String("date", "", "завершённый день YYYY-MM-DD для диагностики; по умолчанию вчера")
	demo := flag.Bool("demo", false, "показать синтетический отчёт без ключей и сети")
	printVersion := flag.Bool("version", false, "версия приложения")
	envFile := flag.String("env", ".env", "локальный .env")
	flag.Parse()
	if *printVersion {
		fmt.Println("GREEN ROOM AI", version)
		return nil
	}
	if *demo {
		return demoReport()
	}
	if err := config.LoadEnv(*envFile); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if *checkConfig {
		fmt.Println("Конфигурация корректна. Значения секретов не выводятся.")
		if cfg.OpenAIKey == "" {
			fmt.Println("Распознавание фото выключено: OPENAI_API_KEY не задан.")
		} else {
			fmt.Println("Распознавание фото включено в конфигурации. Доступ OpenAI этой проверкой не проверяется.")
		}
		return nil
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	httpClient := &http.Client{Timeout: cfg.HTTPTimeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	api, err := iiko.New(iiko.Config{BaseURL: cfg.BaseURL, AppID: cfg.AppID, OrganizationID: cfg.OrganizationID, ClientSecret: cfg.ClientSecret, APIKey: cfg.APIKey, Location: cfg.Location, RequestInterval: cfg.RequestInterval}, httpClient, logger)
	if err != nil {
		return err
	}

	if *checkWorkforce {
		day := *diagnoseDate
		if day == "" {
			day = time.Now().In(cfg.Location).AddDate(0, 0, -1).Format("2006-01-02")
		}
		period, err := analytics.NewPeriod(day, day, cfg.Location)
		if err != nil {
			return err
		}
		checkCtx, done := context.WithTimeout(ctx, 3*time.Minute)
		defer done()
		report, err := api.Workforce(checkCtx, period)
		if err != nil {
			return fmt.Errorf("check sales workforce: %w", err)
		}
		result := struct {
			Date       string `json:"date"`
			Cashiers   int    `json:"cashiers"`
			CashShifts int    `json:"cash_shifts"`
			Role       string `json:"role"`
			Source     string `json:"source"`
		}{Date: day, Cashiers: len(report.Employees), CashShifts: len(report.CashShifts), Role: report.Role, Source: "OLAP Cashier / SessionID"}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return fmt.Errorf("write workforce check: %w", err)
		}
		return nil
	}

	if *diagnose {
		day := *diagnoseDate
		if day == "" {
			day = time.Now().In(cfg.Location).AddDate(0, 0, -1).Format("2006-01-02")
		}
		period, err := analytics.NewPeriod(day, day, cfg.Location)
		if err != nil {
			return err
		}
		diagnosticCtx, done := context.WithTimeout(ctx, 3*time.Minute)
		defer done()
		probes, err := api.DiagnoseSales(diagnosticCtx, period)
		if err != nil {
			return fmt.Errorf("diagnose iiko: %w", err)
		}
		encoder := json.NewEncoder(os.Stdout)
		for _, probe := range probes {
			if err := encoder.Encode(probe); err != nil {
				return fmt.Errorf("write diagnostic result: %w", err)
			}
		}
		return nil
	}

	tg, err := telegram.New(telegram.Config{Token: cfg.TelegramToken}, httpClient)
	if err != nil {
		return err
	}
	if *check {
		checkCtx, done := context.WithTimeout(ctx, 3*time.Minute)
		defer done()
		if err = api.Check(checkCtx); err != nil {
			return fmt.Errorf("iiko connectivity check: %w", err)
		}
		columns, err := api.Columns(checkCtx)
		if err != nil {
			return fmt.Errorf("OLAP capabilities check: %w", err)
		}
		if err = tg.Check(checkCtx); err != nil {
			return fmt.Errorf("Telegram connectivity check: %w", err)
		}
		fmt.Printf("Проверены авторизация iiko, настроенная организация, поля OLAP (%d) и Telegram getMe. Выполнение отчётов отдельно проверяется командой make diagnose-iiko.\n", len(columns))
		return nil
	}
	release, err := state.Acquire(cfg.StateFile + ".lock")
	if err != nil {
		return err
	}
	defer func() {
		if err := release(); err != nil {
			logger.Warn("release instance lock failed", "error", err)
		}
	}()
	store, err := state.Open(cfg.StateFile, cfg.AutoReports)
	if err != nil {
		return err
	}
	rules := analytics.DessertRules{Groups: cfg.DessertGroups, ProductIDs: cfg.DessertProductIDs}
	scheduler, err := notify.New(notify.Config{ChatID: cfg.ReportChatID, Location: cfg.Location, PollInterval: cfg.PollInterval, Delay: cfg.OLAPDelay, DailyEnabled: cfg.DailyReports, DailyHour: cfg.DailyHour, DessertRules: rules, OrganizationID: cfg.OrganizationID, WebhookSecret: cfg.WebhookSecret}, api, tg, store, logger)
	if err != nil {
		return err
	}
	options := []bot.Option{bot.WithCursor(store), bot.WithDiagnostics(scheduler.Diagnostics), bot.WithSalesWorkforce(api)}
	if cfg.OpenAIKey != "" {
		ai, err := assistant.New(assistant.Config{Key: cfg.OpenAIKey, Model: cfg.OpenAIModel, VisionModel: cfg.OpenAIVisionModel, Location: cfg.Location, DessertRules: rules}, httpClient, api)
		if err != nil {
			return err
		}
		options = append(options, bot.WithAssistant(ai), bot.WithPhotoRecognition(tg, ai))
	}
	app, err := bot.New(bot.Config{AllowedUserIDs: cfg.AllowedUserIDs, Location: cfg.Location, DessertRules: rules}, tg, api, scheduler, logger, options...)
	if err != nil {
		return err
	}
	workerCtx, stop := context.WithCancel(ctx)
	defer stop()
	results := make(chan error, 3)
	workers := 2
	go func() { results <- app.Run(workerCtx) }()
	go func() { results <- scheduler.Run(workerCtx) }()
	var server *http.Server
	if cfg.WebhookListen != "" {
		workers++
		server = &http.Server{Addr: cfg.WebhookListen, Handler: scheduler.Webhook(), BaseContext: func(net.Listener) context.Context { return workerCtx }, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 25 * time.Second, IdleTimeout: time.Minute}
		go func() {
			err := server.ListenAndServe()
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			results <- err
		}()
	}
	logger.Info("GREEN ROOM AI started", "version", version, "timezone", cfg.Location.String(), "automatic_reports", scheduler.Enabled(), "webhook_enabled", server != nil, "photo_recognition_enabled", cfg.OpenAIKey != "")
	var first error
	select {
	case <-ctx.Done():
	case first = <-results:
		workers--
	}
	stop()
	if server != nil {
		shutdownCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Warn("webhook shutdown failed", "error", err)
			if closeErr := server.Close(); closeErr != nil {
				logger.Warn("force-close webhook failed", "error", closeErr)
			}
		}
		done()
	}
	for i := 0; i < workers; i++ {
		if err := <-results; err != nil && first == nil {
			first = err
		}
	}
	logger.Info("GREEN ROOM AI stopped")
	return first
}
func demoReport() error {
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		return err
	}
	p, err := analytics.NewPeriod("2026-11-12", "2026-11-12", loc)
	if err != nil {
		return err
	}
	revenue, err := analytics.ParseDecimal("250.50")
	if err != nil {
		return err
	}
	qty := analytics.DecimalFromInt(1)
	dataset := analytics.Dataset{OrderIDsAvailable: true, Rows: []analytics.Row{{Date: "2026-11-12", Group: "Напитки", Name: "Капучино (пример)", OrderID: "demo-order", Revenue: revenue, Quantity: qty}, {Date: "2026-11-12", Group: "Десерты", Name: "Чизкейк (пример)", OrderID: "demo-order", Revenue: analytics.DecimalFromInt(300), Quantity: qty}}}
	report, err := analytics.Aggregate(p, dataset, analytics.DessertRules{Groups: []string{"Десерты"}})
	if err != nil {
		return err
	}
	fmt.Println("ДЕМОНСТРАЦИЯ: синтетические данные, не продажи GREEN ROOM.")
	fmt.Println(analytics.Format(report, 10))
	return nil
}

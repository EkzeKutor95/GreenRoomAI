package bot

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"greenroom-ai/internal/iiko"
)

// iikoFailureMessage translates only classified failures. Arbitrary upstream
// bodies and wrapped error text must never become a Telegram message.
func iikoFailureMessage(err error) string {
	var api *iiko.APIError
	errors.As(err, &api)
	if errors.Is(err, iiko.ErrUnauthorized) {
		if api != nil && api.Operation == "/api/v2/access_token" {
			return "iiko отклонила получение токена (HTTP 401). Проверьте актуальность clientSecret, apiKey и их привязку к приложению. Ключи в Telegram присылать не нужно."
		}
		return "iiko отклонила авторизацию в этом разделе (HTTP 401), в том числе после повторного получения токена. Проверьте доступ приложения и API-ключа к нужному API в iikoWeb. По одному коду 401 нельзя точно определить причину отказа."
	}
	if errors.Is(err, iiko.ErrUnavailable) {
		if api != nil && api.Status == http.StatusTooManyRequests {
			return "iiko ограничила частоту запросов (HTTP 429). Разрешённые повторные попытки не помогли; повторите запрос позже."
		}
		if api != nil && api.Status >= 500 && api.Status <= 599 {
			service := "Сервис iiko"
			if api.Operation == "/api/reporting/v1/olap/columns/get" || api.Operation == "/api/reporting/v1/olap/get" {
				service = "Сервис отчётности iiko"
			}
			return fmt.Sprintf("%s недоступен (HTTP %d). Повторные попытки не помогли. Повторите запрос позже; если ошибка сохраняется, обратитесь в поддержку iiko с журналом. Отчёт не сформирован.", service, api.Status)
		}
		return "Не удалось получить ответ от iiko. Проверьте соединение с интернетом и доступность сервиса; повторите запрос позже. Отчёт не сформирован."
	}
	if errors.Is(err, iiko.ErrMissingColumns) {
		return "iiko не предоставила обязательные поля OLAP для этого отчёта. Нужно сверить доступные поля и возможности API-ключа; подробности указаны в локальном журнале."
	}
	if errors.Is(err, iiko.ErrInvalidResponse) {
		return "Ответ iiko не соответствует ожидаемому контракту. Отчёт не сформирован; подробности проверки указаны в локальном журнале."
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "Внешний сервис не ответил за отведённое время. Повторите запрос позже. Отчёт не сформирован."
	}
	return ""
}

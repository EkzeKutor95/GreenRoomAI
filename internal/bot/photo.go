package bot

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"greenroom-ai/internal/assistant"
	"greenroom-ai/internal/telegram"
)

const maxPhotoPixels int64 = 24_000_000

type PhotoDownloader interface {
	DownloadFile(context.Context, string) ([]byte, error)
}
type PhotoRecognizer interface {
	RecognizePhoto(context.Context, []byte) (string, error)
}
type photoRecognition struct {
	downloader PhotoDownloader
	recognizer PhotoRecognizer
}

// WithPhotoRecognition enables the optional photo feature with both required
// dependencies validated before the bot can start processing messages.
func WithPhotoRecognition(downloader PhotoDownloader, recognizer PhotoRecognizer) Option {
	return func(b *Bot) error {
		if downloader == nil || recognizer == nil {
			return ErrConfig
		}
		b.photo = &photoRecognition{downloader: downloader, recognizer: recognizer}
		return nil
	}
}

func (b *Bot) photoHelp(ctx context.Context, chatID int64) error {
	if b.photo == nil {
		return b.messenger.Send(ctx, chatID, "Для распознавания фото укажите OPENAI_API_KEY в .env и перезапустите бота. Затем отправьте фотографию или файл JPEG/PNG в этот личный чат.", homeKeyboard())
	}
	return b.messenger.Send(ctx, chatID, "Пришлите фотографию чека или рукописной записи либо файл JPEG/PNG до 10 МиБ и 24 мегапикселей. Фото будет передано в OpenAI для распознавания. Неразборчивые места будут отмечены. Результат — черновик: проверьте имена и цифры по оригиналу; распознанный текст не изменяет данные iiko и отчёты.", homeKeyboard())
}

// photoMessage runs only after Handle verifies both sender and private chat.
// It deliberately never changes the report session or retains a photo/result.
func (b *Bot) photoMessage(ctx context.Context, chatID int64, message *telegram.Message) error {
	if b.photo == nil {
		return b.photoHelp(ctx, chatID)
	}
	fileID, err := photoFileID(message)
	if err != nil {
		return b.photoFailure(ctx, chatID, "validate", err)
	}
	processingCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := b.messenger.Send(processingCtx, chatID, "Передаю фото в OpenAI для распознавания. Это может занять некоторое время.", nil); err != nil {
		return err
	}
	photo, err := b.photo.downloader.DownloadFile(processingCtx, fileID)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return b.photoFailure(ctx, chatID, "download", err)
	}
	text, err := b.photo.recognizer.RecognizePhoto(processingCtx, photo)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return b.photoFailure(ctx, chatID, "recognize", err)
	}
	if strings.TrimSpace(text) == "" {
		return b.photoFailure(ctx, chatID, "recognize", ErrPhotoEmptyResult)
	}
	return b.messenger.Send(ctx, chatID, "Распознанный текст — черновик. Проверьте цифры и имена по фото.\n\n"+text, homeKeyboard())
}

func photoFileID(message *telegram.Message) (string, error) {
	if len(message.Photo) > 0 {
		largest := message.Photo[0]
		var largestArea int64
		for _, size := range message.Photo {
			if strings.TrimSpace(size.FileID) == "" || size.Width <= 0 || size.Height <= 0 || size.FileSize < 0 {
				return "", ErrPhotoMetadata
			}
			// Divide before multiplying so malformed dimensions cannot overflow int64.
			if int64(size.Width) > maxPhotoPixels/int64(size.Height) {
				return "", ErrPhotoDimensions
			}
			area := int64(size.Width) * int64(size.Height)
			if area > largestArea || (area == largestArea && size.FileSize > largest.FileSize) {
				largest, largestArea = size, area
			}
		}
		if largest.FileSize > telegram.MaxDownloadBytes {
			return "", telegram.ErrFileTooLarge
		}
		return largest.FileID, nil
	}
	if document := message.Document; document != nil {
		if strings.TrimSpace(document.FileID) == "" || document.FileSize < 0 {
			return "", ErrPhotoMetadata
		}
		if document.FileSize > telegram.MaxDownloadBytes {
			return "", telegram.ErrFileTooLarge
		}
		mimeType := strings.ToLower(strings.TrimSpace(strings.SplitN(document.MIMEType, ";", 2)[0]))
		switch mimeType {
		case "image/jpeg", "image/png":
		case "", "application/octet-stream":
			switch strings.ToLower(filepath.Ext(document.FileName)) {
			case ".jpg", ".jpeg", ".png":
			default:
				return "", ErrPhotoFileType
			}
		default:
			return "", ErrPhotoFileType
		}
		return document.FileID, nil
	}
	return "", ErrPhotoMetadata
}

func (b *Bot) photoFailure(ctx context.Context, chatID int64, stage string, err error) error {
	var apiError *assistant.APIError
	status := 0
	if errors.As(err, &apiError) {
		status = apiError.Status
	}
	reason := "recognition_failed"
	text := "Не удалось распознать фото. Проверьте доступ OpenAI API и повторите попытку. Попробуйте чёткий снимок без бликов."
	switch {
	case status == 401:
		reason, text = "provider_unauthorized", "OpenAI отклонила ключ API (HTTP 401). Проверьте OPENAI_API_KEY в .env и перезапустите бота. Ключ в Telegram присылать не нужно."
	case status == 429:
		reason, text = "provider_limited", "OpenAI ограничила запросы (HTTP 429). Подождите и повторите попытку. Если ошибка сохраняется, проверьте лимиты и баланс API-аккаунта."
	case status == 403:
		reason, text = "provider_forbidden", "OpenAI отказала в доступе (HTTP 403). Проверьте разрешения API-ключа и доступ к выбранной модели распознавания."
	case status == 400:
		reason, text = "provider_invalid_request", "OpenAI отклонила запрос распознавания (HTTP 400). Проверьте, поддерживает ли OPENAI_VISION_MODEL обработку изображений, и параметры запроса."
	case status >= 500 && status <= 599:
		reason, text = "provider_unavailable", fmt.Sprintf("Сервис OpenAI временно недоступен (HTTP %d). Повторите распознавание позже.", status)
	case errors.Is(err, telegram.ErrFileTooLarge):
		reason, text = "file_too_large", "Фото превышает лимит 10 МиБ. Уменьшите размер файла и отправьте его ещё раз."
	case errors.Is(err, assistant.ErrPhotoTooLarge):
		reason, text = "image_limit", "Фото превышает допустимый размер: не более 10 МиБ и 24 мегапикселей. Уменьшите файл или разрешение и отправьте снимок ещё раз."
	case errors.Is(err, ErrPhotoDimensions):
		reason, text = "dimensions_too_large", "Разрешение фото превышает 24 мегапикселя. Уменьшите разрешение и отправьте снимок ещё раз."
	case errors.Is(err, ErrPhotoFileType), errors.Is(err, assistant.ErrPhotoFormat):
		reason, text = "unsupported_file", "Для распознавания отправьте фотографию или файл JPEG/PNG. PDF, HEIC и другие форматы пока не поддерживаются."
	case errors.Is(err, ErrPhotoMetadata):
		reason, text = "invalid_metadata", "Не удалось прочитать сведения о фотографии. Отправьте её ещё раз как фото или файл JPEG/PNG."
	case errors.Is(err, assistant.ErrInvalidPhoto):
		reason, text = "invalid_image", "Файл не удалось прочитать как изображение. Отправьте исправный снимок в формате JPEG или PNG."
	case errors.Is(err, assistant.ErrPhotoResponse):
		reason, text = "incomplete_result", "Распознавание вернуло неполный или некорректный ответ. Повторите попытку или отправьте более чёткий снимок."
	case errors.Is(err, ErrPhotoEmptyResult), errors.Is(err, assistant.ErrPhotoUnreadable):
		reason, text = "empty_result", "На фото не удалось распознать текст. Попробуйте более чёткий снимок."
	case errors.Is(err, context.DeadlineExceeded):
		reason, text = "timeout", "Распознавание не завершилось за отведённое время. Повторите попытку позже или отправьте меньший снимок."
	case stage == "download":
		reason, text = "download_failed", "Не удалось скачать фото из Telegram. Отправьте его повторно; распознавание не выполнялось."
	}
	// Dependency errors may contain token-bearing URLs or recognized personal
	// text. Log only a fixed classification, never the raw error or file ID.
	b.logger.Warn("photo processing failed", "stage", stage, "reason", reason, "status", status)
	return b.messenger.Send(ctx, chatID, text, homeKeyboard())
}

package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

const maxResponseBytes = 16 << 20

type Config struct{ Token, BaseURL string }
type Client struct {
	baseURL     string
	fileBaseURL string
	http        *http.Client
}
type User struct {
	ID int64 `json:"id"`
}
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
type PhotoSize struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}
type FileDocument struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MIMEType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}
type Message struct {
	MessageID int64         `json:"message_id"`
	From      *User         `json:"from"`
	Chat      Chat          `json:"chat"`
	Text      string        `json:"text"`
	Caption   string        `json:"caption"`
	Photo     []PhotoSize   `json:"photo"`
	Document  *FileDocument `json:"document"`
}
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}
type Update struct {
	ID            int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}
type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}
type Keyboard [][]Button

type envelope struct {
	OK         bool            `json:"ok"`
	Result     json.RawMessage `json:"result"`
	Code       int             `json:"error_code"`
	Parameters struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func New(cfg Config, hc *http.Client) (*Client, error) {
	if cfg.Token == "" || strings.ContainsAny(cfg.Token, "/\\ ?#%\r\n\t") || hc == nil {
		return nil, ErrConfig
	}
	base := cfg.BaseURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, ErrConfig
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1")) {
		return nil, ErrConfig
	}
	client := *hc
	if client.Timeout == 0 {
		client.Timeout = 40 * time.Second
	}
	// Never forward a token-bearing request to another host after a redirect.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: strings.TrimRight(base, "/") + "/bot" + cfg.Token + "/", fileBaseURL: u.Scheme + "://" + u.Host + "/file/bot" + cfg.Token + "/", http: &client}, nil
}

func (c *Client) call(ctx context.Context, method string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+method, body)
	if err != nil {
		return fmt.Errorf("%s: %w", method, ErrConfig)
	}
	req.Header.Set("Content-Type", contentType)
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s: %w", method, ErrTransport)
	}
	defer response.Body.Close() // Read-only cleanup; no request body or secret is logged.
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || len(data) > maxResponseBytes {
		return fmt.Errorf("%s: %w", method, ErrResponse)
	}
	var result envelope
	if err := json.Unmarshal(data, &result); err != nil {
		return &APIError{Method: method, Code: response.StatusCode}
	}
	if response.StatusCode != http.StatusOK || !result.OK {
		code := result.Code
		if code == 0 {
			code = response.StatusCode
		}
		return &APIError{Method: method, Code: code, RetryAfter: time.Duration(result.Parameters.RetryAfter) * time.Second}
	}
	if out != nil {
		if err := json.Unmarshal(result.Result, out); err != nil {
			return fmt.Errorf("%s: %w", method, ErrResponse)
		}
	}
	return nil
}

func (c *Client) json(ctx context.Context, method string, payload any, out any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("%s: %w", method, ErrConfig)
	}
	return c.call(ctx, method, bytes.NewReader(data), "application/json", out)
}

func (c *Client) Check(ctx context.Context) error { return c.json(ctx, "getMe", struct{}{}, nil) }
func (c *Client) Updates(ctx context.Context, offset int64) ([]Update, error) {
	var updates []Update
	err := c.json(ctx, "getUpdates", struct {
		Offset  int64    `json:"offset"`
		Timeout int      `json:"timeout"`
		Limit   int      `json:"limit"`
		Allowed []string `json:"allowed_updates"`
	}{offset, 25, 100, []string{"message", "callback_query"}}, &updates)
	return updates, err
}
func (c *Client) Answer(ctx context.Context, id, text string) error {
	return c.json(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}

// Send does not retry: a lost response can mean Telegram already delivered the message.
func (c *Client) Send(ctx context.Context, chatID int64, text string, keyboard Keyboard) error {
	chunks := Chunks(text, 3500)
	for i, chunk := range chunks {
		payload := map[string]any{"chat_id": chatID, "text": chunk, "protect_content": true}
		if i == len(chunks)-1 && len(keyboard) > 0 {
			payload["reply_markup"] = map[string]any{"inline_keyboard": keyboard}
		}
		if err := c.json(ctx, "sendMessage", payload, nil); err != nil {
			return fmt.Errorf("send message part %d/%d: %w", i+1, len(chunks), err)
		}
	}
	return nil
}

func (c *Client) Document(ctx context.Context, chatID int64, name string, data []byte) error {
	if len(data) > 40<<20 || name == "" || strings.ContainsAny(name, "/\\\r\n") {
		return ErrConfig
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return fmt.Errorf("document chat: %w", err)
	}
	if err := writer.WriteField("protect_content", "true"); err != nil {
		return fmt.Errorf("document protection: %w", err)
	}
	part, err := writer.CreateFormFile("document", name)
	if err != nil {
		return fmt.Errorf("document part: %w", err)
	}
	if _, err = part.Write(data); err != nil {
		return fmt.Errorf("document write: %w", err)
	}
	if err = writer.Close(); err != nil {
		return fmt.Errorf("document finalize: %w", err)
	}
	return c.call(ctx, "sendDocument", &body, writer.FormDataContentType(), nil)
}

// Chunks preserves every rune and limits UTF-16 code units, Telegram's entity unit.
func Chunks(text string, limit int) []string {
	if limit < 2 {
		limit = 2
	}
	if text == "" {
		return []string{" "}
	}
	var chunks []string
	runes := []rune(text)
	for len(runes) > 0 {
		units, end := 0, 0
		for end < len(runes) {
			size := utf16.RuneLen(runes[end])
			if size < 1 {
				size = 1
			}
			if units+size > limit {
				break
			}
			units += size
			end++
		}
		chunks = append(chunks, string(runes[:end]))
		runes = runes[end:]
	}
	return chunks
}

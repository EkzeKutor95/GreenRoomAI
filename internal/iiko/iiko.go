// Package iiko implements only documented, read-only iikoCloud operations.
package iiko

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxResponseBytes = 32 << 20
const maxAttempts = 4

type Config struct {
	BaseURL, AppID, OrganizationID, ClientSecret, APIKey string
	Location                                             *time.Location
	RequestInterval                                      time.Duration
}

type Client struct {
	config         Config
	http           *http.Client
	logger         *slog.Logger
	tokenMu        sync.Mutex
	token          string
	expires        time.Time
	rateMu         sync.Mutex
	nextRequest    time.Time
	metadataMu     sync.Mutex
	columns        []Column
	columnsUntil   time.Time
	scopeMu        sync.Mutex
	scopeUntil     time.Time
	salesMu        sync.Mutex
	salesCache     map[string]cachedSales
	workforceMu    sync.Mutex
	workforceCache map[string]cachedWorkforce
}

func New(cfg Config, client *http.Client, logger *slog.Logger) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"))) || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return nil, fmt.Errorf("base URL: %w", ErrInvalidConfig)
	}
	if cfg.AppID == "" || cfg.OrganizationID == "" || cfg.ClientSecret == "" || cfg.APIKey == "" || cfg.Location == nil || cfg.RequestInterval < 0 || client == nil || logger == nil {
		return nil, ErrInvalidConfig
	}
	clone := *client
	if clone.Timeout <= 0 {
		clone.Timeout = 45 * time.Second
	}
	// Credentials must never follow a redirect to a different host.
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{config: cfg, http: &clone, logger: logger, salesCache: make(map[string]cachedSales), workforceCache: make(map[string]cachedWorkforce)}, nil
}

func (c *Client) waitRate(ctx context.Context) error {
	c.rateMu.Lock()
	now := time.Now()
	wait := c.nextRequest.Sub(now)
	if wait < 0 {
		wait = 0
	}
	c.nextRequest = now.Add(wait + c.config.RequestInterval)
	c.rateMu.Unlock()
	return waitContext(ctx, wait)
}
func waitContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token != "" && time.Now().Add(30*time.Second).Before(c.expires) {
		return c.token, nil
	}
	var response struct {
		Token string `json:"token"`
	}
	err := c.request(ctx, "/api/v2/access_token", map[string]string{"appId": c.config.AppID, "clientSecret": c.config.ClientSecret, "apiKey": c.config.APIKey}, "", &response)
	if err != nil {
		return "", fmt.Errorf("obtain iiko token: %w", err)
	}
	expiry, err := tokenExpiry(response.Token)
	if err != nil {
		return "", err
	}
	if !expiry.After(time.Now().Add(30 * time.Second)) {
		return "", fmt.Errorf("expired JWT: %w", ErrInvalidResponse)
	}
	c.token = response.Token
	c.expires = expiry
	return c.token, nil
}
func tokenExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("JWT structure: %w", ErrInvalidResponse)
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("JWT payload: %w", ErrInvalidResponse)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(data, &claims) != nil || claims.Exp == 0 {
		return time.Time{}, fmt.Errorf("JWT expiration: %w", ErrInvalidResponse)
	}
	// exp is used for proactive refresh only; the API validates token authenticity.
	return time.Unix(claims.Exp, 0), nil
}

type requestPolicy struct {
	attempts      int
	timeoutHeader bool
	stage         string
}
type responseMetadata struct {
	Status              int
	CorrelationID, Kind string
}

func normalRequestPolicy() requestPolicy {
	return requestPolicy{attempts: maxAttempts, timeoutHeader: true}
}
func (c *Client) post(ctx context.Context, path string, input, output any) error {
	_, err := c.postWithPolicy(ctx, path, input, output, normalRequestPolicy())
	return err
}
func (c *Client) postWithPolicy(ctx context.Context, path string, input, output any, policy requestPolicy) (responseMetadata, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return responseMetadata{}, err
	}
	meta, err := c.requestWithPolicy(ctx, path, input, token, output, policy)
	if api, ok := err.(*APIError); ok && api.Status == http.StatusUnauthorized {
		c.tokenMu.Lock()
		if c.token == token {
			c.token = ""
		}
		c.tokenMu.Unlock()
		token, err = c.accessToken(ctx)
		if err != nil {
			return meta, err
		}
		return c.requestWithPolicy(ctx, path, input, token, output, policy)
	}
	return meta, err
}
func (c *Client) request(ctx context.Context, path string, input any, token string, output any) error {
	_, err := c.requestWithPolicy(ctx, path, input, token, output, normalRequestPolicy())
	return err
}
func (c *Client) requestWithPolicy(ctx context.Context, path string, input any, token string, output any, policy requestPolicy) (responseMetadata, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return responseMetadata{}, fmt.Errorf("encode iiko request: %w", err)
	}
	var last error
	var meta responseMetadata
	for attempt := 0; attempt < policy.attempts; attempt++ {
		if err = c.waitRate(ctx); err != nil {
			return meta, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.config.BaseURL, "/")+path, bytes.NewReader(body))
		if err != nil {
			return meta, fmt.Errorf("create iiko request: %w", ErrInvalidConfig)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		if policy.timeoutHeader {
			req.Header.Set("Timeout", "40")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := c.http.Do(req)
		delay := time.Duration(1<<attempt) * 100 * time.Millisecond
		if err != nil {
			if ctx.Err() != nil {
				return meta, ctx.Err()
			}
			meta = responseMetadata{}
			// A transport error can contain URL/proxy credentials. Expose classification only.
			last = &APIError{Operation: path, Cause: ErrUnavailable}
		} else {
			payload, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
			closeErr := response.Body.Close()
			meta = responseMetadata{Status: response.StatusCode, CorrelationID: responseCorrelationID(response.Header, payload), Kind: responseKind(response.Header.Get("Content-Type"), payload)}
			switch {
			case readErr != nil || closeErr != nil:
				last = &APIError{Operation: path, Status: meta.Status, CorrelationID: meta.CorrelationID, ResponseKind: meta.Kind, Cause: ErrUnavailable}
			case len(payload) > maxResponseBytes:
				return meta, &APIError{Operation: path, Status: meta.Status, CorrelationID: meta.CorrelationID, ResponseKind: meta.Kind, Cause: ErrResultLimit}
			case response.StatusCode >= 200 && response.StatusCode < 300:
				if json.Unmarshal(payload, output) != nil {
					return meta, &APIError{Operation: path, Status: meta.Status, CorrelationID: meta.CorrelationID, ResponseKind: meta.Kind, Cause: ErrInvalidResponse}
				}
				return meta, nil
			default:
				api := classifyStatus(path, response.StatusCode)
				api.CorrelationID = meta.CorrelationID
				api.ResponseKind = meta.Kind
				last = api
				if !retryable(response.StatusCode) {
					return meta, last
				}
				if retryDelay, ok := retryAfter(response.Header.Get("Retry-After")); ok && retryDelay > delay {
					delay = retryDelay
				}
			}
		}
		if attempt+1 == policy.attempts {
			break
		}
		c.logger.Warn("iiko request will be retried", "operation", path, "stage", policy.stage, "attempt", attempt+1, "status", meta.Status, "backoff_ms", delay.Milliseconds(), "correlation_id", meta.CorrelationID, "response_kind", meta.Kind)
		if err = waitContext(ctx, delay); err != nil {
			return meta, err
		}
	}
	return meta, last
}

func retryable(status int) bool { return status == 408 || status == 429 || status >= 500 }
func retryAfter(value string) (time.Duration, bool) {
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	if until, err := http.ParseTime(value); err == nil {
		return time.Until(until), true
	}
	return 0, false
}
func classifyStatus(path string, status int) *APIError {
	cause := ErrValidation
	switch {
	case status == 401:
		cause = ErrUnauthorized
	case status == 403:
		cause = ErrPermissionDenied
	case retryable(status):
		cause = ErrUnavailable
	case status >= 300 && status < 400:
		cause = ErrInvalidResponse
	}
	return &APIError{Operation: path, Status: status, Cause: cause}
}

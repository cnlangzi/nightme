package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type apiClient interface {
	call(ctx context.Context, method string, params map[string]any, result any) error
	download(ctx context.Context, filePath string) ([]byte, error)
}

type httpClient struct {
	token        string
	baseURL      string
	fileBaseURL  string
	client       *http.Client
}

func newHTTPClient(token string) *httpClient {
	return &httpClient{
		token:       token,
		baseURL:     "https://api.telegram.org/bot" + token + "/",
		fileBaseURL: "https://api.telegram.org/file/bot" + token + "/",
		client:      &http.Client{Timeout: 45 * time.Second},
	}
}

type apiEnvelope struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	Parameters  *apiParameters  `json:"parameters"`
}

type apiParameters struct {
	RetryAfter int `json:"retry_after"`
}

type apiError struct {
	StatusCode int
	ErrorCode  int
	RetryAfter int
	Message    string
}

func (e *apiError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return "telegram API error"
}

func (c *httpClient) call(ctx context.Context, method string, params map[string]any, result any) error {
	if c == nil || c.client == nil {
		return errors.New("telegram: API client is nil")
	}
	form := url.Values{}
	for key, value := range params {
		switch typed := value.(type) {
		case nil:
			continue
		case string:
			form.Set(key, typed)
		case bool:
			form.Set(key, strconv.FormatBool(typed))
		case int:
			form.Set(key, strconv.Itoa(typed))
		case int64:
			form.Set(key, strconv.FormatInt(typed, 10))
		case float64:
			form.Set(key, strconv.FormatFloat(typed, 'f', -1, 64))
		case []string:
			for _, item := range typed {
				form.Add(key, item)
			}
		default:
			encoded, err := json.Marshal(typed)
			if err != nil {
				return fmt.Errorf("telegram: encode %s: %w", key, err)
			}
			form.Set(key, string(encoded))
		}
	}

	endpoint := c.baseURL + method
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("telegram: create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("telegram: request %s: %w", method, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("telegram: read response: %w", err)
	}
	var envelope apiEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("telegram: decode response: %w", err)
	}
	if !envelope.OK {
		retryAfter := 0
		if envelope.Parameters != nil {
			retryAfter = envelope.Parameters.RetryAfter
		}
		return &apiError{StatusCode: response.StatusCode, ErrorCode: envelope.ErrorCode, RetryAfter: retryAfter, Message: envelope.Description}
	}
	if result != nil && len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			return fmt.Errorf("telegram: decode result: %w", err)
		}
	}
	return nil
}

func (c *httpClient) download(ctx context.Context, filePath string) ([]byte, error) {
	if c == nil || c.client == nil {
		return nil, errors.New("telegram: API client is nil")
	}
	if filePath == "" || strings.Contains(filePath, "..") {
		return nil, errors.New("telegram: invalid file path")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.fileBaseURL+filePath, nil)
	if err != nil {
		return nil, fmt.Errorf("telegram: create file request: %w", err)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("telegram: download file: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("telegram: file download status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("telegram: read file: %w", err)
	}
	return data, nil
}

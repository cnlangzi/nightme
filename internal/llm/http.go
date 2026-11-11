package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Error is a classified failure of an LLM call.
//
// The classification is what a caller branches on: a retryable error
// is worth trying again, a non-retryable one is worth telling the
// user about.
type Error struct {
	// Status is the HTTP status of the failure. Zero when the
	// request never reached the server.
	Status int
	// Code is the provider's machine-readable error code, when it
	// sent one.
	Code string
	// Message is the human-readable reason.
	Message string
	// Retryable is true when the call may be tried again.
	Retryable bool
	// Err is the underlying error, when there is one.
	Err error
}

// Error implements error.
func (e *Error) Error() string {
	var b strings.Builder
	switch {
	case e.Status > 0 && e.Code != "":
		fmt.Fprintf(&b, "llm: %s (http %d, %s)", e.Message, e.Status, e.Code)
	case e.Status > 0:
		fmt.Fprintf(&b, "llm: %s (http %d)", e.Message, e.Status)
	default:
		fmt.Fprintf(&b, "llm: %s", e.Message)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether err is worth retrying.
func Retryable(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Retryable
	}
	return false
}

// APIKey reads the API key from the environment variable the
// configuration names.
//
// The key is never stored in the configuration file: the file names
// the variable, the environment holds the secret.
func APIKey(envName string) (string, error) {
	name := strings.TrimSpace(envName)
	if name == "" {
		return "", nil
	}
	key := strings.TrimSpace(os.Getenv(name))
	if key == "" {
		return "", fmt.Errorf("llm: the environment variable %s is empty or unset", name)
	}
	return key, nil
}

// retryPolicy is the shared transient-failure policy: 429, 5xx and
// network errors are retried with a growing backoff.
type retryPolicy struct {
	attempts int
	base     time.Duration
	max      time.Duration
}

// defaultRetryPolicy is the policy the providers use.
func defaultRetryPolicy() retryPolicy {
	return retryPolicy{attempts: 4, base: 500 * time.Millisecond, max: 8 * time.Second}
}

// PostJSON sends body as JSON to url and decodes the answer into out.
//
// A 429, a 5xx or a network failure is retried with a growing
// backoff; a Retry-After header is honoured when the server sends one.
// Everything else — a 401, a 400 — is reported as a non-retryable
// Error.
func PostJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, body any, out any) error {
	policy := defaultRetryPolicy()
	var lastErr error
	for attempt := 0; attempt < policy.attempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, policy.backoff(attempt)); err != nil {
				return err
			}
		}
		retryAfter, err := postOnce(ctx, client, url, headers, body, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !Retryable(err) {
			return err
		}
		if retryAfter > 0 {
			// The server told us when to come back; the next
			// iteration waits exactly that long instead of the
			// schedule.
			if err := sleepCtx(ctx, retryAfter); err != nil {
				return err
			}
		}
	}
	return lastErr
}

// postOnce performs one request. The returned duration is the
// Retry-After the server asked for, when it sent one.
func postOnce(ctx context.Context, client *http.Client, url string, headers map[string]string, body any, out any) (time.Duration, error) {
	if client == nil {
		client = http.DefaultClient
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, &Error{Message: "the request could not be encoded", Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, &Error{Message: "the request could not be built", Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, &Error{Message: "the request was cancelled", Err: ctx.Err()}
		}
		return 0, &Error{Message: "the request did not reach the server", Retryable: true, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, &Error{Message: "the answer could not be read", Retryable: true, Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return retryAfter(resp), statusError(resp.StatusCode, payload)
	}
	if out == nil {
		return 0, nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return 0, &Error{Status: resp.StatusCode, Message: "the answer is not valid JSON", Err: err}
	}
	return 0, nil
}

// statusError classifies an HTTP failure.
func statusError(status int, payload []byte) error {
	code, msg := parseErrorBody(payload)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &Error{Status: status, Code: code, Message: firstNonEmpty(msg, "the credentials were rejected")}
	case status == http.StatusTooManyRequests:
		return &Error{Status: status, Code: code, Message: firstNonEmpty(msg, "the server is rate limiting the requests"), Retryable: true}
	case status >= 500:
		return &Error{Status: status, Code: code, Message: firstNonEmpty(msg, "the server failed"), Retryable: true}
	default:
		return &Error{Status: status, Code: code, Message: firstNonEmpty(msg, "the request was rejected")}
	}
}

// errorBody is the shape the OpenAI-compatible protocols report an
// error in.
type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
	// The decision protocols report the error at the top level.
	Message string `json:"message"`
	Detail  string `json:"detail"`
}

// parseErrorBody extracts the error code and message from a failure
// payload. A payload that is not JSON yields empty strings.
func parseErrorBody(payload []byte) (code, msg string) {
	var body errorBody
	if err := json.Unmarshal(payload, &body); err != nil {
		return "", strings.TrimSpace(string(payload))
	}
	code = stringifyCode(body.Error.Code)
	if body.Error.Type != "" && code == "" {
		code = body.Error.Type
	}
	msg = firstNonEmpty(body.Error.Message, body.Message, body.Detail)
	return code, msg
}

// stringifyCode renders a code that may be a string or a number.
func stringifyCode(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case float64:
		return strconv.FormatFloat(c, 'f', -1, 64)
	default:
		return ""
	}
}

// retryAfter reads the Retry-After header of a response. Only the
// delay-seconds form is honoured; an HTTP date is ignored, because a
// wall-clock date is not worth the risk of computing the wrong
// delay.
func retryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// backoff returns how long to wait before the next attempt.
func (p retryPolicy) backoff(attempt int) time.Duration {
	d := p.base << (attempt - 1)
	if d > p.max || d <= 0 {
		d = p.max
	}
	return d
}

// sleepCtx waits for d, or until ctx ends. It reports whether the
// wait completed.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

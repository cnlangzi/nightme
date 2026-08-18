package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPClient_Call_OK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/getMe") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":{"id":42,"username":"bot","first_name":"B"}}`)
	}))
	defer server.Close()

	client := &httpClient{token: "x", baseURL: server.URL + "/", client: server.Client()}
	var result UserInfo
	if err := client.call(context.Background(), "getMe", nil, &result); err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.ID != 42 || result.Username != "bot" {
		t.Fatalf("result = %+v", result)
	}
}

func TestHTTPClient_Call_Params(t *testing.T) {
	var captured map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		captured = make(map[string]string)
		for key, values := range r.PostForm {
			if len(values) > 0 {
				captured[key] = values[0]
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	client := &httpClient{token: "x", baseURL: server.URL + "/", client: server.Client()}
	err := client.call(context.Background(), "sendMessage", map[string]any{
		"chat_id": "100",
		"text":    "hello",
		"bool":    true,
		"int":     42,
	}, nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if captured["chat_id"] != "100" || captured["text"] != "hello" {
		t.Fatalf("captured = %v", captured)
	}
}

func TestHTTPClient_Call_JSONParam(t *testing.T) {
	var captured string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		captured = r.PostForm.Get("reply_markup")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	client := &httpClient{token: "x", baseURL: server.URL + "/", client: server.Client()}
	err := client.call(context.Background(), "sendMessage", map[string]any{
		"reply_markup": map[string]any{"inline_keyboard": []any{}},
	}, nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(captured, "inline_keyboard") {
		t.Fatalf("reply_markup not JSON-encoded: %q", captured)
	}
}

func TestHTTPClient_Call_RetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":false,"description":"rate","error_code":429,"parameters":{"retry_after":5}}`)
	}))
	defer server.Close()

	client := &httpClient{token: "x", baseURL: server.URL + "/", client: server.Client()}
	err := client.call(context.Background(), "getMe", nil, nil)
	if err == nil {
		t.Fatal("call must fail on !ok")
	}
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err type = %T", err)
	}
	if apiErr.RetryAfter != 5 {
		t.Fatalf("retry_after = %d", apiErr.RetryAfter)
	}
}

func TestHTTPClient_Call_NilClient(t *testing.T) {
	var c *httpClient
	if err := c.call(context.Background(), "x", nil, nil); err == nil {
		t.Fatal("nil client must error")
	}
}

func TestHTTPClient_Download(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer server.Close()

	client := &httpClient{token: "x", fileBaseURL: server.URL + "/", client: server.Client()}
	data, err := client.download(context.Background(), "file")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if string(data) != "payload" {
		t.Fatalf("download = %q", string(data))
	}
}

func TestHTTPClient_Download_InvalidPath(t *testing.T) {
	client := &httpClient{token: "x", client: http.DefaultClient}
	if _, err := client.download(context.Background(), ""); err == nil {
		t.Fatal("empty path must error")
	}
	if _, err := client.download(context.Background(), "../etc/passwd"); err == nil {
		t.Fatal("path traversal must error")
	}
}

func TestHTTPClient_Download_NilClient(t *testing.T) {
	var c *httpClient
	if _, err := c.download(context.Background(), "x"); err == nil {
		t.Fatal("nil client must error")
	}
}

func TestHTTPClient_Download_BadStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := &httpClient{token: "x", fileBaseURL: server.URL + "/", client: server.Client()}
	_, err := client.download(context.Background(), "file")
	if err == nil {
		t.Fatal("500 must error")
	}
}

func TestAPIError_Message(t *testing.T) {
	if (&apiError{Message: "x"}).Error() != "x" {
		t.Fatal("message")
	}
	if (&apiError{}).Error() != "telegram API error" {
		t.Fatal("default")
	}
	if (&apiError{}).Error() == "" {
		t.Fatal("nil")
	}
}

func TestNewHTTPClient(t *testing.T) {
	c := newHTTPClient("token")
	if c == nil || c.token != "token" {
		t.Fatal("newHTTPClient")
	}
	if !strings.Contains(c.baseURL, "token") {
		t.Fatalf("baseURL = %q", c.baseURL)
	}
}

func TestAPIEnvelope_Decode(t *testing.T) {
	body := []byte(`{"ok":true,"result":{"file_id":"x","file_path":"documents/x"}}`)
	var envelope apiEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK {
		t.Fatal("ok flag")
	}
	var file File
	if err := json.Unmarshal(envelope.Result, &file); err != nil {
		t.Fatal(err)
	}
	if file.FileID != "x" || file.FilePath != "documents/x" {
		t.Fatal("file")
	}
}

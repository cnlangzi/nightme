package telegram

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// fakeAPI is a configurable apiClient that records every call so
// tests can assert on them. It's deliberately not goroutine-safe
// per-key; tests that interleave calls use the helper mutex.
type fakeAPI struct {
	mu sync.Mutex
	// Calls records every method call in order.
	Calls []fakeCall
	// Errors forces the next call to return err.
	Errors []error
	// GetMe returns the user record used by Adapter.Start.
	GetMeResult UserInfo
	// GetMeErr is returned from "getMe" if set.
	GetMeErr error
	// FileBytes is the bytes returned by download for any path
	// (download uses file_path, not file_id).
	FileBytes []byte
	// ForceReplyRequired asserts that the next sendMessage uses
	// force_reply in its reply_markup.
	ForceReplyRequired bool
	// LastForceReply flags the most recent sendMessage as having
	// force_reply, exposed for assertions across threads.
	LastForceReply bool
	// TransientOnce, if non-nil, returns this error on the next
	// call and clears itself. Used to exercise the retry path.
	TransientOnce error
	// CallCount is incremented on every call (test hook for
	// asserting retry counts).
	callCount int
	// callErr is a static error override (returns from every call).
	callErr error
}

type fakeCall struct {
	Method string
	Params map[string]any
}

func (f *fakeAPI) call(ctx context.Context, method string, params map[string]any, result any) error {
	// Mirror a real HTTP client's ctx-honouring behaviour so
	// that pollLoop, which checks err after every call, actually
	// observes a cancelled context. Without this, every test
	// case that calls Start() leaks a pollLoop goroutine —
	// fakeAPI returns nil indefinitely and the loop never
	// breaks. On Windows CI's 7GB hosted runner this OOMs the
	// test binary after ~150 test cases have piled up
	// goroutines (each one burns ~8KB stack + the per-call
	// allocations). See PR #224 windows-latest job 95925744882.
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount++
	f.Calls = append(f.Calls, fakeCall{Method: method, Params: copyParams(params)})
	if f.callErr != nil {
		return f.callErr
	}
	if f.TransientOnce != nil {
		err := f.TransientOnce
		f.TransientOnce = nil
		return err
	}
	if len(f.Errors) > 0 {
		err := f.Errors[0]
		f.Errors = f.Errors[1:]
		if err != nil {
			return err
		}
	}
	if method == "getMe" {
		if f.GetMeErr != nil {
			return f.GetMeErr
		}
		if result != nil {
			_ = json.Unmarshal(mustMarshal(f.GetMeResult), result)
		}
		return nil
	}
	if method == "sendMessage" {
		if result != nil {
			_ = json.Unmarshal([]byte(`{"message_id":`+strconv.Itoa(nextSendMessageID())+`,"chat":{"id":1,"type":"private"}}`), result)
		}
		if params != nil {
			if markup, ok := params["reply_markup"].(map[string]any); ok {
				if _, has := markup["force_reply"]; has {
					f.LastForceReply = true
				}
			}
		}
		return nil
	}
	if method == "editMessageText" || method == "editMessageReplyMarkup" {
		return nil
	}
	if method == "setMessageReaction" {
		return nil
	}
	if method == "answerCallbackQuery" {
		return nil
	}
	if method == "createForumTopic" {
		if result != nil {
			_ = json.Unmarshal([]byte(`{"message_thread_id":1,"name":"x"}`), result)
		}
		return nil
	}
	if method == "getFile" {
		if result != nil {
			_ = json.Unmarshal([]byte(`{"file_id":"x","file_path":"documents/x.bin"}`), result)
		}
		return nil
	}
	if method == "getUpdates" {
		// The pollLoop unmarshals into a struct with Updates []Update.
		// Returning an empty object keeps the loop polling harmlessly.
		if result != nil {
			_ = json.Unmarshal([]byte(`{}`), result)
		}
		return nil
	}
	return nil
}

func (f *fakeAPI) download(ctx context.Context, filePath string) ([]byte, error) {
	return f.FileBytes, nil
}

func copyParams(params map[string]any) map[string]any {
	out := make(map[string]any, len(params))
	for key, value := range params {
		out[key] = value
	}
	return out
}

func mustMarshal(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return data
}

var sendMessageCounter atomic.Int64

func nextSendMessageID() int {
	n := sendMessageCounter.Add(1)
	return 100 + int(n)
}

func resetSendMessageCounter() {
	sendMessageCounter.Store(0)
}

// findCalls returns the indices of f.Calls whose Method matches.
func (f *fakeAPI) snapshotCalls() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeCall, len(f.Calls))
	copy(out, f.Calls)
	return out
}

func findCalls(calls []fakeCall, method string) []int {
	out := make([]int, 0)
	for index, call := range calls {
		if call.Method == method {
			out = append(out, index)
		}
	}
	return out
}

// joinParam concatenates two strings (handy for assertions on
// callback_data).
func joinParam(value any) string {
	if value == nil {
		return ""
	}
	if typed, ok := value.(string); ok {
		return typed
	}
	return ""
}

// findCall returns the first call matching method, or nil.
func findCall(calls []fakeCall, method string) *fakeCall {
	for index := range calls {
		if calls[index].Method == method {
			return &calls[index]
		}
	}
	return nil
}

// paramsString concatenates all string params into a single
// debug string for assertion failure messages.
func paramsString(call *fakeCall) string {
	if call == nil {
		return ""
	}
	var b strings.Builder
	for key, value := range call.Params {
		b.WriteString(key)
		b.WriteString("=")
		switch typed := value.(type) {
		case string:
			b.WriteString(typed)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

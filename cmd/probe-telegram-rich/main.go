// Round 10: verify RichText inline entity shape (string | array | typed).
// Critical for L2 walker: can a paragraph's text be an array of mixed
// strings + bold/italic/code/url entities?
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type result struct {
	Name   string
	Status int
	OK     bool
	Desc   string
}

func main() {
	token := os.Getenv("NIGHTME_TELEGRAM_BOT_TOKEN")
	chatID := os.Getenv("NIGHTME_TELEGRAM_PROBE_CHAT")
	if token == "" || chatID == "" {
		die("set NIGHTME_TELEGRAM_BOT_TOKEN and NIGHTME_TELEGRAM_PROBE_CHAT")
	}
	fmt.Fprintf(os.Stderr, "→ chat_id=%s token_len=%d\n\n", chatID, len(token))

	type tc struct {
		name string
		body string
	}
	tests := []tc{
		// (1) Control: plain string text (works since round 4)
		{"plain string text", `{"blocks":[{"type":"paragraph","text":"plain"}]}`},
		// (2) Array form: single string in array
		{"array form, single string", `{"blocks":[{"type":"paragraph","text":["just a string"]}]}`},
		// (3) Bold inline
		{"bold inline", `{"blocks":[{"type":"paragraph","text":[{"type":"bold","text":"Hello"}," world"]}]}`},
		// (4) Italic inline
		{"italic inline", `{"blocks":[{"type":"paragraph","text":[{"type":"italic","text":"emphasis"}," here"]}]}`},
		// (5) Code inline
		{"code inline", `{"blocks":[{"type":"paragraph","text":["use ",{"type":"code","text":"fmt.Println"}," here"]}]}`},
		// (6) URL inline (the most common for LLM output)
		{"url inline", `{"blocks":[{"type":"paragraph","text":[{"type":"url","text":"Telegram docs","url":"https://core.telegram.org/bots/api"}]}]}`},
		// (7) Bold + italic + url in one paragraph (realistic LLM mix)
		{"mixed bold+italic+url", `{"blocks":[{"type":"paragraph","text":[{"type":"bold","text":"Important:"}," see ","{"type":"italic","text":"section 3.1"}]}],`},
		// ↑ intentional broken json to test error parsing; will fix below
		// Replace with correct:
		{"mixed bold+italic+url (fixed)", `{"blocks":[{"type":"paragraph","text":[{"type":"bold","text":"Important:"}," see the ",{"type":"italic","text":"docs"},{"type":"url","text":"here","url":"https://example.com"}]}]}`},
		// (8) Inline in heading
		{"bold in heading", `{"blocks":[{"type":"heading","text":[{"type":"bold","text":"Bold title"}],"size":1}]}`},
		// (9) Inline in list item
		{"code in list item", `{"blocks":[{"type":"list","items":[{"blocks":[{"type":"paragraph","text":["run ",{"type":"code","text":"go test"}," first"]}]}]}]}`},
		// (10) Nested RichText: bold contains italic
		{"nested bold>italic", `{"blocks":[{"type":"paragraph","text":[{"type":"bold","text":[{"type":"italic","text":"bold italic"}]}]}]}`},
	}

	var results []result
	for i, t := range tests {
		if i > 0 {
			time.Sleep(300 * time.Millisecond)
		}
		form := url.Values{
			"chat_id":      {chatID},
			"rich_message": {t.body},
		}
		status, ok, desc, _ := call(token, "sendRichMessage", form)
		results = append(results, result{Name: t.name, Status: status, OK: ok, Desc: desc})
		fmt.Fprintf(os.Stderr, "  %s %s — %s\n", mark(ok), t.name, trunc(desc, 70))
	}

	fmt.Println()
	fmt.Println(strings.Repeat("─", 130))
	fmt.Printf("%-40s | %4s | %4s | %s\n", "Test", "HTTP", "OK", "Description")
	fmt.Println(strings.Repeat("─", 130))
	for _, r := range results {
		fmt.Printf("%-40s | %4d | %4s | %s\n",
			trunc(r.Name, 40), r.Status, mark(r.OK), trunc(r.Desc, 70))
	}

	passed, failed := 0, 0
	for _, r := range results {
		if r.OK {
			passed++
		} else {
			failed++
		}
	}
	fmt.Println()
	fmt.Printf("─── Round 10 summary: %d passed, %d failed ───\n", passed, failed)
	if failed > 0 {
		fmt.Println("\nFailed (need attention before L2 walker design):")
		for _, r := range results {
			if !r.OK {
				fmt.Printf("  ✗ %s: %s\n", r.Name, r.Desc)
			}
		}
	}
}

func call(token, method string, form url.Values) (int, bool, string, int64) {
	endpoint := "https://api.telegram.org/bot" + token + "/" + method
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, false, "request build: " + err.Error(), 0
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false, "http: " + err.Error(), 0
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var env struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		ErrorCode   int    `json:"error_code"`
		Result      struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return resp.StatusCode, false, "decode: " + err.Error() + " body=" + trunc(string(body), 200), 0
	}
	return resp.StatusCode, env.OK, env.Description, env.Result.MessageID
}

func mark(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

func die(msg string) {
	fmt.Fprintln(os.Stderr, "fatal: "+msg)
	os.Exit(2)
}

// Command send_one sends ONE real message to Feishu. Used by the
// F-37 thread-routing real-server probe to verify rendering
// effects on the actual production bot.
//
// Usage:
//
//	go run ./cmd/_probe/send_one \
//	    -chat-id oc_xxx \
//	    -user-msg-id om_xxx \
//	    -prefix "[probe-D]" \
//	    -mode reply-true \
//	    -content "thread-only"
//
// Modes:
//
//	create         POST /im/v1/messages                (top-level, no parent)
//	create-root    POST /im/v1/messages  body.root_id=om_xxx   (raw, bypasses SDK field omission)
//	reply-default  POST /messages/{om_xxx}/reply  no reply_in_thread
//	reply-false    POST /messages/{om_xxx}/reply  reply_in_thread:false (explicit)
//	reply-true     POST /messages/{om_xxx}/reply  reply_in_thread:true  (F-37 path)
//	chain-reply    POST /messages/{om_xxx}/reply  chain mode (same as reply-true)
//
// Credentials are read from ~/.config/nightme/config.yaml
// (app_id + app_secret under `feishu:`).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"gopkg.in/yaml.v3"
)

type configFile struct {
	Feishu struct {
		AppID     string `yaml:"app_id"`
		AppSecret string `yaml:"app_secret"`
	} `yaml:"feishu"`
}

func main() {
	var (
		chatID      = flag.String("chat-id", "", "Feishu chat_id (oc_xxx) — or owner open_id (ou_xxx) when -receive-type=open_id")
		userMsgID   = flag.String("user-msg-id", "", "Feishu message_id (om_xxx) to reply to — required for reply-* modes")
		prefix      = flag.String("prefix", "[probe]", "prefix to prepend to body so you can identify this in Feishu UI")
		content     = flag.String("content", "thread test", "body text after the prefix")
		mode        = flag.String("mode", "reply-true", "create | reply-default | reply-false | reply-true | chain-reply")
		receiveType = flag.String("receive-type", "chat_id", "chat_id | open_id | email — only used in create mode (for DM send)")
		confPath    = flag.String("config", defaultConfigPath(), "path to nightme config.yaml")
	)
	flag.Parse()

	if *chatID == "" {
		fail("chat-id is required")
	}
	if !(*mode == "create" || *mode == "create-root") && *userMsgID == "" {
		fail("user-msg-id is required for reply-* modes")
	}

	appID, appSecret := loadCreds(*confPath)
	if appID == "" || appSecret == "" {
		fail("could not read feishu.app_id / feishu.app_secret from %s", *confPath)
	}

	cli := lark.NewClient(appID, appSecret)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	body := *prefix + " " + *content
	switch *mode {
	case "create":
		sendCreate(ctx, cli, *chatID, body, *receiveType)
	case "reply-default":
		sendReply(ctx, cli, *userMsgID, body, nil)
	case "reply-false":
		sendReply(ctx, cli, *userMsgID, body, ptrBool(false))
	case "reply-true":
		sendReply(ctx, cli, *userMsgID, body, ptrBool(true))
	case "chain-reply":
		sendReply(ctx, cli, *userMsgID, body, ptrBool(true))
	default:
		fail("unknown mode %q (valid: create, reply-default, reply-false, reply-true, chain-reply)", *mode)
	}
}

func sendCreate(ctx context.Context, cli *lark.Client, receiveID, body, receiveType string) {
	var ridType string
	switch receiveType {
	case "chat_id", "":
		ridType = larkim.CreateMessageV1ReceiveIDTypeChatId
	case "open_id":
		ridType = larkim.CreateMessageV1ReceiveIDTypeOpenId
	case "email":
		ridType = larkim.CreateMessageV1ReceiveIDTypeEmail
	case "user_id":
		ridType = larkim.CreateMessageV1ReceiveIDTypeUserId
	case "union_id":
		ridType = larkim.CreateMessageV1ReceiveIDTypeUnionId
	default:
		fail("unknown receive-type %q (valid: chat_id | open_id | email | user_id | union_id)", receiveType)
	}
	resp, err := cli.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(ridType).
		Body(&larkim.CreateMessageReqBody{
			ReceiveId: strPtr(receiveID),
			MsgType:   strPtr(larkim.MsgTypeText),
			Content:   strPtr(fmt.Sprintf(`{"text":%q}`, body)),
		}).Build())
	if err != nil {
		fail("create: %v", err)
	}
	printReplyResp("create", resp.Code, resp.Msg,
		deref(resp.Data.MessageId), deref(resp.Data.ParentId),
		deref(resp.Data.RootId), deref(resp.Data.ThreadId))
}

func sendReply(ctx context.Context, cli *lark.Client, userMsgID, body string, replyInThread *bool) {
	builder := larkim.NewReplyMessageReqBodyBuilder().
		MsgType(larkim.MsgTypeText).
		Content(fmt.Sprintf(`{"text":%q}`, body))
	if replyInThread != nil {
		// NOTE: per F-37 §3.4 the production code only calls
		// .ReplyInThread(true); this CLI exposes all three
		// explicit states (nil/false/true) for the probe.
		builder = builder.ReplyInThread(*replyInThread)
	}
	resp, err := cli.Im.V1.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(userMsgID).
		Body(builder.Build()).
		Build())
	if err != nil {
		fail("reply: %v", err)
	}
	printReplyResp("reply", resp.Code, resp.Msg,
		deref(resp.Data.MessageId), deref(resp.Data.ParentId),
		deref(resp.Data.RootId), deref(resp.Data.ThreadId))
}

func printReplyResp(op string, code int, msg, messageID, parentID, rootID, threadID string) {
	fmt.Printf("  → %s code=%d msg=%q\n", op, code, msg)
	fmt.Printf("  → message_id=%s\n", messageID)
	fmt.Printf("  → parent_id =%s\n", parentID)
	fmt.Printf("  → root_id   =%s\n", rootID)
	fmt.Printf("  → thread_id =%s\n", threadID)
}

func ptrBool(b bool) *bool   { return &b }
func strPtr(s string) *string { return &s }

func deref(s *string) string {
	if s == nil {
		return "—"
	}
	return *s
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
	os.Exit(1)
}

func defaultConfigPath() string {
	if env := os.Getenv("NIGHTME_CONFIG"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, c := range []string{".config/nightme/config.yaml", ".nightme/config.yaml"} {
		p := home + "/" + c
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return home + "/.config/nightme/config.yaml"
}

func loadCreds(path string) (string, string) {
	if path == "" {
		return "", ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	var c configFile
	if err := yaml.Unmarshal(data, &c); err != nil {
		log.Printf("yaml parse %s: %v", path, err)
		return "", ""
	}
	return c.Feishu.AppID, c.Feishu.AppSecret
}

package telegram

type Update struct {
	UpdateID         int64              `json:"update_id"`
	Message          *Message           `json:"message"`
	EditedMessage    *Message           `json:"edited_message"`
	CallbackQuery    *CallbackQuery     `json:"callback_query"`
	MessageReaction  *MessageReactionUpdate `json:"message_reaction"`
	MyChatMember     *ChatMemberUpdate  `json:"my_chat_member"`
	ChatMember       *ChatMemberUpdate  `json:"chat_member"`
}

// MessageReactionUpdate is emitted when a user changes their
// reaction on a message. Telegram surfaces both additions
// (NewReaction non-empty, OldReaction empty) and removals
// (NewReaction empty, OldReaction non-empty) on the same shape;
// the handler routes both into the runtime as InboundMessage.Reaction.
type MessageReactionUpdate struct {
	Chat        Chat             `json:"chat"`
	MessageID   int              `json:"message_id"`
	User        User             `json:"user"`
	Date        int64            `json:"date"`
	OldReaction []ReactionType   `json:"old_reaction"`
	NewReaction []ReactionType   `json:"new_reaction"`
}

type ReactionType struct {
	Type  string `json:"type"`
	Emoji string `json:"emoji"`
}

type Message struct {
	MessageID       int             `json:"message_id"`
	MessageThreadID int             `json:"message_thread_id"`
	IsTopicMessage  bool            `json:"is_topic_message"`
	Date            int64           `json:"date"`
	Chat            Chat            `json:"chat"`
	From            *User           `json:"from"`
	Text            string          `json:"text"`
	Caption         string          `json:"caption"`
	ReplyToMessage  *Message        `json:"reply_to_message"`
	Entities        []MessageEntity `json:"entities"`
	Photo           []PhotoSize     `json:"photo"`
	Document        *Document       `json:"document"`
	Audio           *Document       `json:"audio"`
	Voice           *Document       `json:"voice"`
	Video           *Document       `json:"video"`
	Sticker         *Sticker        `json:"sticker"`
}

type Chat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	Username string `json:"username"`
}

type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

type MessageEntity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

type PhotoSize struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type Sticker struct {
	FileID string `json:"file_id"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

type ChatMemberUpdate struct {
	Chat          Chat        `json:"chat"`
	From          User        `json:"from"`
	OldChatMember *ChatMember `json:"old_chat_member"`
	NewChatMember *ChatMember `json:"new_chat_member"`
}

type ChatMember struct {
	Status string `json:"status"`
	User   User   `json:"user"`
}

type UserInfo struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

type File struct {
	FileID   string `json:"file_id"`
	FilePath string `json:"file_path"`
}

type SendMessageResult struct {
	MessageID int  `json:"message_id"`
	Chat      Chat `json:"chat"`
}

type ForumTopic struct {
	MessageThreadID int    `json:"message_thread_id"`
	Name            string `json:"name"`
}

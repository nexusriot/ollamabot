package bot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/nexusriot/ollamabot/pkg/auth"
)

type OllamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []OllamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

type OllamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type OllamaChatResponse struct {
	Model   string        `json:"model"`
	Message OllamaMessage `json:"message"`
	Done    bool          `json:"done"`
	Error   string        `json:"error,omitempty"`
}

type Bot struct {
	api           *tgbotapi.BotAPI
	userStore     *auth.UserStore
	ollamaBaseURL string
	model         string
}

func NewBot(api *tgbotapi.BotAPI, userStore *auth.UserStore, ollamaBaseURL, model string) *Bot {
	return &Bot{
		api:           api,
		userStore:     userStore,
		ollamaBaseURL: strings.TrimRight(ollamaBaseURL, "/"),
		model:         model,
	}
}

// sendTypingUntilDone periodically sends "typing" action until done is closed.
func (b *Bot) sendTypingUntilDone(chatID int64, done <-chan struct{}) {
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			// best-effort, ignore errors
			_, _ = b.api.Send(tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping))
		}
	}
}

func (b *Bot) Run() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.api.GetUpdatesChan(u)

	for update := range updates {
		if update.Message == nil {
			continue
		}
		if update.Message.From == nil {
			continue
		}

		chatID := update.Message.Chat.ID
		userID := update.Message.From.ID
		text := strings.TrimSpace(update.Message.Text)

		if text == "" {
			continue
		}

		if b.userStore.IsEnabled() && b.userStore.IsAdmin(userID) {
			if strings.HasPrefix(text, "/adduser") {
				b.handleAddUser(chatID, text)
				continue
			}

			if strings.HasPrefix(text, "/listusers") {
				b.handleListUsers(chatID)
				continue
			}
		}

		if strings.HasPrefix(text, "/whoami") {
			b.handleWhoAmI(chatID, userID)
			continue
		}

		if b.userStore.IsEnabled() {
			allowed, err := b.userStore.IsAuthorized(userID)
			if err != nil {
				log.Printf("auth error for user %d: %v", userID, err)
				msg := tgbotapi.NewMessage(chatID,
					"⚠️ Internal auth error, please try again later.")
				_, _ = b.api.Send(msg)
				continue
			}

			if !allowed {
				msg := tgbotapi.NewMessage(chatID,
					"🚫 You are not allowed to use this bot.\n"+
						"Ask the admin to add your Telegram ID.")
				_, _ = b.api.Send(msg)
				continue
			}

			// user is allowed; update last_activity (best effort)
			if err := b.userStore.Touch(userID); err != nil {
				log.Printf("failed to update last_activity for %d: %v", userID, err)
			}
		}

		if strings.HasPrefix(text, "/start") {
			msg := tgbotapi.NewMessage(chatID,
				"Hi! Send me any message and I'll forward it to Ollama ("+b.model+").\n\n"+
					"Code blocks with ``` will be rendered as code in Telegram.")
			msg.ParseMode = "Markdown"
			_, _ = b.api.Send(msg)
			continue
		}

		if strings.HasPrefix(text, "/model") {
			b.handleModelCommand(chatID, text)
			continue
		}

		// Initial typing indicator
		_, _ = b.api.Send(tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping))

		// capture current model so changing /model later
		// doesn't affect this request mid-flight
		currentModel := b.model
		prompt := text

		// Handle Ollama call in background with progress "typing..."
		go func(chatID int64, prompt, modelForCall string) {
			done := make(chan struct{})

			// progress goroutine
			go b.sendTypingUntilDone(chatID, done)

			reply, err := b.callOllama(modelForCall, prompt)

			// stop typing loop
			close(done)

			if err != nil {
				log.Printf("ollama error: %v", err)
				msg := tgbotapi.NewMessage(chatID, "⚠️ Error from backend: "+err.Error())
				_, _ = b.api.Send(msg)
				return
			}

			msg := tgbotapi.NewMessage(chatID, reply)
			msg.ParseMode = "Markdown"
			msg.DisableWebPagePreview = true

			for _, chunk := range splitTelegramMessage(reply, 4000) {
				msg.Text = chunk
				_, _ = b.api.Send(msg)
				time.Sleep(300 * time.Millisecond)
			}
		}(chatID, prompt, currentModel)
	}
}

func (b *Bot) handleAddUser(chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		msg := tgbotapi.NewMessage(chatID, "Usage: /adduser <telegram_id>")
		_, _ = b.api.Send(msg)
		return
	}

	toAdd, err := parseInt64(parts[1])
	if err != nil {
		msg := tgbotapi.NewMessage(chatID, "Invalid user id (must be integer).")
		_, _ = b.api.Send(msg)
		return
	}

	if err := b.userStore.AddUser(toAdd); err != nil {
		log.Printf("adduser error: %v", err)
		msg := tgbotapi.NewMessage(chatID, "⚠️ Failed to add user: "+err.Error())
		_, _ = b.api.Send(msg)
		return
	}

	msg := tgbotapi.NewMessage(chatID,
		fmt.Sprintf("✅ User %d has been added/updated.", toAdd))
	_, _ = b.api.Send(msg)
}

func (b *Bot) handleListUsers(chatID int64) {
	users, err := b.userStore.ListUsers(200)
	if err != nil {
		log.Printf("/listusers error: %v", err)
		msg := tgbotapi.NewMessage(chatID, "⚠️ Failed to list users: "+err.Error())
		_, _ = b.api.Send(msg)
		return
	}

	if len(users) == 0 {
		msg := tgbotapi.NewMessage(chatID, "No users in DB yet.")
		_, _ = b.api.Send(msg)
		return
	}

	var sb strings.Builder
	sb.WriteString("Registered users:\n")
	for _, urow := range users {
		fmt.Fprintf(&sb,
			"- ID: %d\n  created_at: %s\n  last_activity: %s\n",
			urow.TelegramID, urow.CreatedAt, urow.LastActivity)
	}

	msg := tgbotapi.NewMessage(chatID, sb.String())
	_, _ = b.api.Send(msg)
}

func (b *Bot) handleWhoAmI(chatID, userID int64) {
	var sb strings.Builder
	sb.WriteString("Your info:\n")
	fmt.Fprintf(&sb, "- Telegram ID: %d\n", userID)

	if b.userStore.IsEnabled() {
		sb.WriteString("- Auth: ENABLED\n")
		if b.userStore.IsAdmin(userID) {
			sb.WriteString("- Role: admin\n")
		} else {
			sb.WriteString("- Role: user\n")
		}
		allowed, err := b.userStore.IsAuthorized(userID)
		if err != nil {
			fmt.Fprintf(&sb, "- Allowed: ERROR (%v)\n", err)
		} else if allowed {
			sb.WriteString("- Allowed: YES\n")
		} else {
			sb.WriteString("- Allowed: NO\n")
		}
	} else {
		sb.WriteString("- Auth: DISABLED (bot is open for everyone)\n")
	}

	msg := tgbotapi.NewMessage(chatID, sb.String())
	_, _ = b.api.Send(msg)
}

func (b *Bot) handleModelCommand(chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) >= 2 {
		b.model = parts[1]
		reply := fmt.Sprintf("✅ Model changed to `%s`", b.model)
		msg := tgbotapi.NewMessage(chatID, reply)
		msg.ParseMode = "Markdown"
		_, _ = b.api.Send(msg)
	} else {
		msg := tgbotapi.NewMessage(chatID,
			fmt.Sprintf("Current model: `%s`\nUsage: `/model llama3.1`", b.model))
		msg.ParseMode = "Markdown"
		_, _ = b.api.Send(msg)
	}
}

func (b *Bot) callOllama(model, prompt string) (string, error) {
	reqBody := OllamaChatRequest{
		Model: model,
		Messages: []OllamaMessage{
			{
				Role: "user",
				Content: prompt + "\n\n" +
					"Please answer in Markdown. Use fenced code blocks (```lang ... ```).",
			},
		},
		Stream: false,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	url := b.ollamaBaseURL + "/api/chat"
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return "", fmt.Errorf("ollama HTTP %d: %s", resp.StatusCode, buf.String())
	}

	var oresp OllamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&oresp); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if oresp.Error != "" {
		return "", fmt.Errorf("ollama error: %s", oresp.Error)
	}

	return strings.TrimSpace(oresp.Message.Content), nil
}

func splitTelegramMessage(s string, max int) []string {
	if len(s) <= max {
		return []string{s}
	}
	var res []string
	runes := []rune(s)
	for len(runes) > max {
		res = append(res, string(runes[:max]))
		runes = runes[max:]
	}
	if len(runes) > 0 {
		res = append(res, string(runes))
	}
	return res
}

func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

package bot

import (
	"bufio"
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
	httpClient    *http.Client
	stream        bool
}

func NewBot(api *tgbotapi.BotAPI, userStore *auth.UserStore, ollamaBaseURL, model string, timeout time.Duration, stream bool) *Bot {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	return &Bot{
		api:           api,
		userStore:     userStore,
		ollamaBaseURL: strings.TrimRight(ollamaBaseURL, "/"),
		model:         model,
		httpClient: &http.Client{
			Timeout: timeout,
		},
		stream: stream,
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

func (b *Bot) streamOllamaToTelegramHybrid(chatID int64, model, prompt string) error {
	reqBody := OllamaChatRequest{
		Model: model,
		Messages: []OllamaMessage{
			{
				Role:    "user",
				Content: prompt,
			},
		},
		Stream: true,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	url := b.ollamaBaseURL + "/api/chat"

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("ollama HTTP %d: %s", resp.StatusCode, buf.String())
	}

	// placeholder message - will be edited
	placeholder := tgbotapi.NewMessage(chatID, "…")
	sentMsg, err := b.api.Send(placeholder)
	if err != nil {
		return fmt.Errorf("send placeholder: %w", err)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var full strings.Builder
	lastEdit := time.Now()
	lastSent := ""

	flush := func(force bool) error {
		text := strings.TrimSpace(full.String())
		if text == "" {
			text = "…"
		}

		// Telegram limit
		runes := []rune(text)
		if len(runes) > 4096 {
			text = string(runes[:4096])
		}

		if !force {
			if time.Since(lastEdit) < 700*time.Millisecond {
				return nil
			}
			if text == lastSent {
				return nil
			}
		}

		edit := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, text)
		edit.DisableWebPagePreview = true

		if _, err := b.api.Send(edit); err != nil {
			return err
		}

		lastEdit = time.Now()
		lastSent = text
		return nil
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var chunk OllamaChatResponse
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}

		if chunk.Error != "" {
			return fmt.Errorf("ollama error: %s", chunk.Error)
		}

		if chunk.Message.Content != "" {
			full.WriteString(chunk.Message.Content)
			_ = flush(false)
		}

		if chunk.Done {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	// final flush
	_ = flush(true)

	fullText := strings.TrimSpace(full.String())

	//  formatted message
	b.sendFinalMarkdown(chatID, fullText)

	del := tgbotapi.NewDeleteMessage(chatID, sentMsg.MessageID)
	_, _ = b.api.Request(del)

	return nil
}

func escapeTelegramMarkdownV2Smart(s string) string {
	var result strings.Builder

	inCodeBlock := false
	lines := strings.Split(s, "\n")

	for i, line := range lines {
		trim := strings.TrimSpace(line)

		// toggle code block
		if strings.HasPrefix(trim, "```") {
			inCodeBlock = !inCodeBlock
			result.WriteString(line)
		} else if inCodeBlock {
			// inside code block → DO NOT escape
			result.WriteString(line)
		} else {
			// normal text → escape
			result.WriteString(escapeTelegramMarkdownV2(line))
		}

		if i < len(lines)-1 {
			result.WriteString("\n")
		}
	}

	return result.String()
}

func (b *Bot) sendFinalMarkdown(chatID int64, text string) {
	escaped := escapeTelegramMarkdownV2Smart(text)

	parts := splitTelegramMessage(escaped, 4000)

	for i, part := range parts {
		msg := tgbotapi.NewMessage(chatID, part)
		msg.ParseMode = "MarkdownV2"
		msg.DisableWebPagePreview = true

		_, err := b.api.Send(msg)
		if err != nil {
			log.Printf("final markdown send error: %v", err)
		}

		if i < len(parts)-1 {
			time.Sleep(200 * time.Millisecond)
		}
	}
}

func escapeTelegramMarkdownV2(s string) string {
	replacer := strings.NewReplacer(
		"_", "\\_",
		"*", "\\*",
		"[", "\\[",
		"]", "\\]",
		"(", "\\(",
		")", "\\)",
		"~", "\\~",
		"`", "\\`",
		">", "\\>",
		"#", "\\#",
		"+", "\\+",
		"-", "\\-",
		"=", "\\=",
		"|", "\\|",
		"{", "\\{",
		"}", "\\}",
		".", "\\.",
		"!", "\\!",
	)
	return replacer.Replace(s)
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

		// Handle Ollama call in background (with typing progress)
		go func(chatID int64, prompt, modelForCall string) {
			done := make(chan struct{})
			go b.sendTypingUntilDone(chatID, done)

			defer close(done)

			if b.stream {
				err := b.streamOllamaToTelegramHybrid(chatID, modelForCall, prompt)
				if err != nil {
					log.Printf("stream error: %v", err)
					msg := tgbotapi.NewMessage(chatID, "⚠️ Error: "+err.Error())
					_, _ = b.api.Send(msg)
				}
				return
			}

			reply, err := b.callOllama(modelForCall, prompt)
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

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	log.Printf("Ollama (non-stream) call to %s took %s, status=%d",
		url, time.Since(start), resp.StatusCode)

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

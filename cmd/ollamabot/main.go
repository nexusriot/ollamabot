package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	_ "modernc.org/sqlite"
)

// -------- Ollama structures --------

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

// -------- Auth / user store --------

type UserStore struct {
	enabled bool
	adminID int64
	db      *sql.DB
}

type DBUser struct {
	TelegramID   int64
	CreatedAt    string
	LastActivity string
}

// NewUserStoreFromEnv initializes auth depending on env vars.
// If BOT_AUTH_ENABLED is not true/1/yes, auth is disabled and no DB is used.
func NewUserStoreFromEnv() (*UserStore, error) {
	enabledStr := os.Getenv("BOT_AUTH_ENABLED")
	enabled := strings.EqualFold(enabledStr, "1") ||
		strings.EqualFold(enabledStr, "true") ||
		strings.EqualFold(enabledStr, "yes")

	if !enabled {
		log.Println("Auth: disabled (BOT_AUTH_ENABLED is not true/1/yes). Bot is open for everyone.")
		return &UserStore{enabled: false}, nil
	}

	adminStr := os.Getenv("BOT_ADMIN_ID")
	if adminStr == "" {
		return nil, fmt.Errorf("auth enabled but BOT_ADMIN_ID is not set")
	}

	adminID, err := strconv.ParseInt(adminStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid BOT_ADMIN_ID %q: %w", adminStr, err)
	}

	dbPath := os.Getenv("BOT_AUTH_DB_PATH")
	if dbPath == "" {
		dbPath = "/var/lib/ollamabot/bot_users.db"
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	// init schema
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			telegram_id   INTEGER PRIMARY KEY,
			created_at    TEXT NOT NULL,
			last_activity TEXT NOT NULL
		);
	`)
	if err != nil {
		return nil, fmt.Errorf("create table users: %w", err)
	}

	log.Printf("Auth: ENABLED. Admin ID=%d, DB=%s", adminID, dbPath)
	return &UserStore{
		enabled: true,
		adminID: adminID,
		db:      db,
	}, nil
}

func (s *UserStore) IsEnabled() bool {
	return s != nil && s.enabled
}

func (s *UserStore) IsAdmin(id int64) bool {
	return s != nil && s.enabled && id == s.adminID
}

// IsAuthorized returns true if user is allowed to use the bot.
func (s *UserStore) IsAuthorized(id int64) (bool, error) {
	if s == nil || !s.enabled {
		return true, nil
	}
	if id == s.adminID {
		return true, nil
	}

	var exists int
	err := s.db.QueryRow(`SELECT 1 FROM users WHERE telegram_id = ?`, id).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// AddUser inserts or updates a user (admin command).
func (s *UserStore) AddUser(id int64) error {
	if s == nil || !s.enabled {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`
		INSERT INTO users (telegram_id, created_at, last_activity)
		VALUES (?, ?, ?)
		ON CONFLICT(telegram_id) DO UPDATE SET last_activity = excluded.last_activity
	`, id, now, now)
	return err
}

// Touch updates last_activity for an existing user.
// Does nothing if user is not in table (we don't auto-create users here).
func (s *UserStore) Touch(id int64) error {
	if s == nil || !s.enabled {
		return nil
	}
	if id == s.adminID {
		// we don't track admin activity in the users table
		return nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`
		UPDATE users
		SET last_activity = ?
		WHERE telegram_id = ?
	`, now, id)
	return err
}

// ListUsers returns up to limit users ordered by created_at.
func (s *UserStore) ListUsers(limit int) ([]DBUser, error) {
	if s == nil || !s.enabled {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}

	rows, err := s.db.Query(`
		SELECT telegram_id, created_at, last_activity
		FROM users
		ORDER BY created_at ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var res []DBUser
	for rows.Next() {
		var u DBUser
		if err := rows.Scan(&u.TelegramID, &u.CreatedAt, &u.LastActivity); err != nil {
			return nil, err
		}
		res = append(res, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// -------- Main --------

func main() {
	telegramToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if telegramToken == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN is not set")
	}

	ollamaBaseURL := os.Getenv("OLLAMA_BASE_URL")
	if ollamaBaseURL == "" {
		ollamaBaseURL = "http://ollama:11434"
	}

	model := os.Getenv("OLLAMA_MODEL")

	// init auth (optional)
	userStore, err := NewUserStoreFromEnv()
	if err != nil {
		log.Fatalf("failed to init auth: %v", err)
	}

	log.Printf("Starting bot. Model=%s, OllamaBaseURL=%s", model, ollamaBaseURL)

	bot, err := tgbotapi.NewBotAPI(telegramToken)
	if err != nil {
		log.Fatalf("failed to create bot: %v", err)
	}

	bot.Debug = false
	log.Printf("Authorized on account %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

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

		// ---------- Admin-only commands: /adduser, /listusers ----------
		if userStore.IsEnabled() && userStore.IsAdmin(userID) {
			if strings.HasPrefix(text, "/adduser") {
				parts := strings.Fields(text)
				if len(parts) < 2 {
					msg := tgbotapi.NewMessage(chatID, "Usage: /adduser <telegram_id>")
					_, _ = bot.Send(msg)
					continue
				}

				toAdd, err := strconv.ParseInt(parts[1], 10, 64)
				if err != nil {
					msg := tgbotapi.NewMessage(chatID, "Invalid user id (must be integer).")
					_, _ = bot.Send(msg)
					continue
				}

				if err := userStore.AddUser(toAdd); err != nil {
					log.Printf("adduser error: %v", err)
					msg := tgbotapi.NewMessage(chatID, "⚠️ Failed to add user: "+err.Error())
					_, _ = bot.Send(msg)
					continue
				}

				msg := tgbotapi.NewMessage(chatID,
					fmt.Sprintf("✅ User %d has been added/updated.", toAdd))
				_, _ = bot.Send(msg)
				continue
			}

			if strings.HasPrefix(text, "/listusers") {
				users, err := userStore.ListUsers(200)
				if err != nil {
					log.Printf("/listusers error: %v", err)
					msg := tgbotapi.NewMessage(chatID, "⚠️ Failed to list users: "+err.Error())
					_, _ = bot.Send(msg)
					continue
				}

				if len(users) == 0 {
					msg := tgbotapi.NewMessage(chatID, "No users in DB yet.")
					_, _ = bot.Send(msg)
					continue
				}

				var b strings.Builder
				b.WriteString("Registered users:\n")
				for _, urow := range users {
					fmt.Fprintf(&b,
						"- ID: %d\n  created_at: %s\n  last_activity: %s\n",
						urow.TelegramID, urow.CreatedAt, urow.LastActivity)
				}

				msg := tgbotapi.NewMessage(chatID, b.String())
				_, _ = bot.Send(msg)
				continue
			}
		}

		// ---------- /whoami (always allowed) ----------
		if strings.HasPrefix(text, "/whoami") {
			var b strings.Builder
			b.WriteString("Your info:\n")
			fmt.Fprintf(&b, "- Telegram ID: %d\n", userID)

			if userStore.IsEnabled() {
				b.WriteString("- Auth: ENABLED\n")
				if userStore.IsAdmin(userID) {
					b.WriteString("- Role: admin\n")
				} else {
					b.WriteString("- Role: user\n")
				}
				allowed, err := userStore.IsAuthorized(userID)
				if err != nil {
					fmt.Fprintf(&b, "- Allowed: ERROR (%v)\n", err)
				} else if allowed {
					b.WriteString("- Allowed: YES\n")
				} else {
					b.WriteString("- Allowed: NO\n")
				}
			} else {
				b.WriteString("- Auth: DISABLED (bot is open for everyone)\n")
			}

			msg := tgbotapi.NewMessage(chatID, b.String())
			_, _ = bot.Send(msg)
			continue
		}

		// ---------- Auth check (if enabled) ----------
		if userStore.IsEnabled() {
			allowed, err := userStore.IsAuthorized(userID)
			if err != nil {
				log.Printf("auth error for user %d: %v", userID, err)
				msg := tgbotapi.NewMessage(chatID,
					"⚠️ Internal auth error, please try again later.")
				_, _ = bot.Send(msg)
				continue
			}

			if !allowed {
				msg := tgbotapi.NewMessage(chatID,
					"🚫 You are not allowed to use this bot.\n"+
						"Ask the admin to add your Telegram ID.")
				_, _ = bot.Send(msg)
				continue
			}

			// user is allowed; update last_activity (best effort)
			if err := userStore.Touch(userID); err != nil {
				log.Printf("failed to update last_activity for %d: %v", userID, err)
			}
		}

		// ---------- Existing commands ----------

		if strings.HasPrefix(text, "/start") {
			msg := tgbotapi.NewMessage(chatID,
				"Hi! Send me any message and I'll forward it to Ollama ("+model+").\n\n"+
					"Code blocks with ``` will be rendered as code in Telegram.")
			msg.ParseMode = "Markdown"
			_, _ = bot.Send(msg)
			continue
		}

		if strings.HasPrefix(text, "/model") {
			parts := strings.Fields(text)
			if len(parts) >= 2 {
				model = parts[1]
				reply := fmt.Sprintf("✅ Model changed to `%s`", model)
				msg := tgbotapi.NewMessage(chatID, reply)
				msg.ParseMode = "Markdown"
				_, _ = bot.Send(msg)
			} else {
				msg := tgbotapi.NewMessage(chatID,
					fmt.Sprintf("Current model: `%s`\nUsage: `/model llama3.1`", model))
				msg.ParseMode = "Markdown"
				_, _ = bot.Send(msg)
			}
			continue
		}

		// Acknowledge user
		typing := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
		_, _ = bot.Send(typing)

		go func(chatID int64, text string) {
			reply, err := callOllama(ollamaBaseURL, model, text)
			if err != nil {
				log.Printf("ollama error: %v", err)
				msg := tgbotapi.NewMessage(chatID, "⚠️ Error from backend: "+err.Error())
				_, _ = bot.Send(msg)
				return
			}

			msg := tgbotapi.NewMessage(chatID, reply)
			msg.ParseMode = "Markdown"
			msg.DisableWebPagePreview = true

			for _, chunk := range splitTelegramMessage(reply, 4000) {
				msg.Text = chunk
				_, _ = bot.Send(msg)
				time.Sleep(300 * time.Millisecond)
			}
		}(chatID, text)
	}
}

// -------- Ollama helper --------

func callOllama(baseURL, model, prompt string) (string, error) {
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

	url := strings.TrimRight(baseURL, "/") + "/api/chat"
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

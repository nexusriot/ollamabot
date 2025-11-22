package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
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
		if update.Message == nil { // ignore non-message updates
			continue
		}

		chatID := update.Message.Chat.ID
		text := strings.TrimSpace(update.Message.Text)

		if text == "" {
			continue
		}

		// Handle simple commands
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

			// Telegram Markdown supports ``` blocks well. Most Ollama models
			// reply in Markdown already, so we just pass it through.
			msg := tgbotapi.NewMessage(chatID, reply)
			msg.ParseMode = "Markdown" // not MarkdownV2, less escaping hassle
			msg.DisableWebPagePreview = true

			// Split if message > 4096 chars
			for _, chunk := range splitTelegramMessage(reply, 4000) {
				msg.Text = chunk
				_, _ = bot.Send(msg)
				// small delay to avoid flood limits
				time.Sleep(300 * time.Millisecond)
			}
		}(chatID, text)
	}
}

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

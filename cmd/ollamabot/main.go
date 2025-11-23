package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/nexusriot/ollamabot/pkg/auth"
	"github.com/nexusriot/ollamabot/pkg/bot"
)

func readAuthConfigFromEnv() (auth.AuthConfig, error) {
	var cfg auth.AuthConfig

	enabledStr := os.Getenv("BOT_AUTH_ENABLED")
	cfg.Enabled = strings.EqualFold(enabledStr, "1") ||
		strings.EqualFold(enabledStr, "true") ||
		strings.EqualFold(enabledStr, "yes")

	if !cfg.Enabled {
		// No further auth config needed
		return cfg, nil
	}

	adminStr := os.Getenv("BOT_ADMIN_ID")
	if adminStr == "" {
		return cfg, fmt.Errorf("auth enabled but BOT_ADMIN_ID is not set")
	}

	adminID, err := strconv.ParseInt(adminStr, 10, 64)
	if err != nil {
		return cfg, fmt.Errorf("invalid BOT_ADMIN_ID %q: %w", adminStr, err)
	}
	cfg.AdminID = adminID

	dbPath := os.Getenv("BOT_AUTH_DB_PATH")
	if dbPath == "" {
		dbPath = "/var/lib/ollamabot/bot_users.db"
	}
	cfg.DBPath = dbPath

	return cfg, nil
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

	// Read auth config from env
	authCfg, err := readAuthConfigFromEnv()
	if err != nil {
		log.Fatalf("failed to read auth config: %v", err)
	}

	// Initialize user store (may be disabled)
	userStore, err := auth.NewUserStore(authCfg)
	if err != nil {
		log.Fatalf("failed to init auth/user store: %v", err)
	}

	// Telegram bot API
	botAPI, err := tgbotapi.NewBotAPI(telegramToken)
	if err != nil {
		log.Fatalf("failed to create bot: %v", err)
	}

	botAPI.Debug = false
	log.Printf("Authorized on account %s", botAPI.Self.UserName)

	// Create and run bot
	bot := bot.NewBot(botAPI, userStore, ollamaBaseURL, model)
	bot.Run()
}

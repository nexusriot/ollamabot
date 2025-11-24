# ollamabot-telegram

A Telegram bot that forwards user messages to an [Ollama](https://ollama.com/) model and returns Markdown-formatted answers.  
Built in Go, deployable with Docker, and optionally locked down with SQLite-based user authentication.

---
## Features

- **Telegram ↔ Ollama bridge**  
  Sends each Telegram message to Ollama `/api/chat` and returns a Markdown response.

- **Typing indicator while waiting**  
  Shows “typing…” every few seconds until the Ollama reply is ready.

- **Markdown output with code blocks**  
  Perfect for code answers and formatted text.

- **Optional authentication (whitelist)**
    - One admin user (by Telegram ID).
    - Admin can add/list users.
    - Users stored in SQLite at `/var/lib/ollamabot/bot_users.db`.

- **Runtime model switching**  
  `/model <name>` changes the active model on the fly.

- **Auto-splitting long answers**  
  Telegram message limit-safe (chunks of ≤ 4000 chars).

---

## Environment Variables

### Required
| Variable | Description |
|---------|-------------|
| `TELEGRAM_BOT_TOKEN` | Bot token from BotFather |

### Optional (Ollama)
| Variable | Default               | Description             |
|----------|-----------------------|-------------------------|
| `OLLAMA_BASE_URL` | `http://ollama:11434` | Ollama API base URL     |
| `OLLAMA_MODEL` | *(empty)*             | Default model for chats |
| `OLLAMA_TIMEOUT_SECONDS` | 180                   | Ollama request timeout  |
| `OLLAMA_STREAM` | false                 | Use Ollama stream       |

### Optional (Authentication)
Authentication is **disabled by default**.

Enable it with:

| Variable | Description |
|----------|-------------|
| `BOT_AUTH_ENABLED` | Set to `true` / `1` / `yes` to enable whitelist |
| `BOT_ADMIN_ID` | Telegram user ID of the admin |
| `BOT_AUTH_DB_PATH` | SQLite DB path inside container (default `/var/lib/ollamabot/bot_users.db`) |


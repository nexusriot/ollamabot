package auth

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "modernc.org/sqlite"
)

type AuthConfig struct {
	Enabled bool
	AdminID int64
	DBPath  string
}

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

// NewUserStore creates a user store based on the given config.
// If cfg.Enabled is false, returns a disabled store with no DB.
func NewUserStore(cfg AuthConfig) (*UserStore, error) {
	if !cfg.Enabled {
		log.Println("Auth: disabled (BOT_AUTH_ENABLED is not true/1/yes). Bot is open for everyone.")
		return &UserStore{enabled: false}, nil
	}

	if cfg.AdminID == 0 {
		return nil, fmt.Errorf("auth enabled but AdminID is 0")
	}
	if cfg.DBPath == "" {
		return nil, fmt.Errorf("auth enabled but DBPath is empty")
	}

	db, err := sql.Open("sqlite", cfg.DBPath)
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

	log.Printf("Auth: ENABLED. Admin ID=%d, DB=%s", cfg.AdminID, cfg.DBPath)
	return &UserStore{
		enabled: true,
		adminID: cfg.AdminID,
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

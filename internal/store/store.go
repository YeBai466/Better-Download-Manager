// Package store persists tasks and settings in a local SQLite database
// (pure-Go modernc driver, no CGO).
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "modernc.org/sqlite"

	"github.com/yebai/b-download-manager/internal/config"
	"github.com/yebai/b-download-manager/internal/downloader"
)

// Store is the persistence layer. It is safe for concurrent use (database/sql
// manages its own pool; SQLite is opened in WAL mode).
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies the schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: serialize writes to avoid "database is locked"
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS tasks (
    id         TEXT PRIMARY KEY,
    data       TEXT NOT NULL,
    status     TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);`)
	return err
}

// SaveTask inserts or updates a task record.
func (s *Store) SaveTask(rec downloader.Record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO tasks (id, data, status, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET data=excluded.data, status=excluded.status`,
		rec.ID, string(data), string(rec.Status), rec.CreatedAt.UnixMilli(),
	)
	return err
}

// DeleteTask removes a task record.
func (s *Store) DeleteTask(id string) error {
	_, err := s.db.Exec(`DELETE FROM tasks WHERE id = ?`, id)
	return err
}

// LoadTasks returns all persisted task records ordered by creation time.
func (s *Store) LoadTasks() ([]downloader.Record, error) {
	rows, err := s.db.Query(`SELECT data FROM tasks ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []downloader.Record
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var rec downloader.Record
		if err := json.Unmarshal([]byte(data), &rec); err != nil {
			return nil, fmt.Errorf("decode task: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

const settingsKey = "app"

// LoadSettings returns the saved settings, or defaults if none are stored yet.
func (s *Store) LoadSettings() (config.Settings, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, settingsKey).Scan(&value)
	if err == sql.ErrNoRows {
		return config.Default(), nil
	}
	if err != nil {
		return config.Settings{}, err
	}
	var cfg config.Settings
	if err := json.Unmarshal([]byte(value), &cfg); err != nil {
		return config.Default(), nil // tolerate corrupt settings
	}
	cfg.Normalize()
	return cfg, nil
}

// SaveSettings persists the application settings.
func (s *Store) SaveSettings(cfg config.Settings) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		settingsKey, string(data),
	)
	return err
}

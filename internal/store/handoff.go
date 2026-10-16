package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type Handoff struct {
	ID      string          `json:"id"`
	Request json.RawMessage `json:"request"`
	State   string          `json:"state"`
	Error   string          `json:"error,omitempty"`
}

func (s *Store) SaveHandoff(h Handoff) error {
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO handoffs(id,data,created_at) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, h.ID, string(data), time.Now().UnixMilli())
	return err
}
func (s *Store) Handoff(id string) (Handoff, bool, error) {
	var data string
	err := s.db.QueryRow(`SELECT data FROM handoffs WHERE id=?`, id).Scan(&data)
	if err == sql.ErrNoRows {
		return Handoff{}, false, nil
	}
	if err != nil {
		return Handoff{}, false, err
	}
	var h Handoff
	err = json.Unmarshal([]byte(data), &h)
	return h, err == nil, err
}
func (s *Store) PendingHandoffs() ([]Handoff, error) {
	rows, err := s.db.Query(`SELECT data FROM handoffs ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Handoff
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var h Handoff
		if err := json.Unmarshal([]byte(data), &h); err != nil {
			return nil, fmt.Errorf("handoff: %w", err)
		}
		if h.State == "dialog" || h.State == "processing" {
			result = append(result, h)
		}
	}
	return result, rows.Err()
}
func (s *Store) DeleteHandoff(id string) error {
	_, err := s.db.Exec(`DELETE FROM handoffs WHERE id=?`, id)
	return err
}

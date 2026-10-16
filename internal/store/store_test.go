package store

import (
	"path/filepath"
	"testing"
)

func TestOldSettingsGainNewDefaultsAndKeepFalseValues(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.db.Exec(`INSERT INTO settings(key,value) VALUES('app','{"categorize":false,"takeoverEnabled":false,"maxConcurrent":5,"connections":8}')`)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := s.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AdaptiveConnections || cfg.Categorize || cfg.TakeoverEnabled {
		t.Fatalf("incorrect defaults: %+v", cfg)
	}
}

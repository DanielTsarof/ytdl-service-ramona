package config

import (
	"strings"
	"testing"
)

func TestDatabaseLabelHidesPassword(t *testing.T) {
	for dsn, want := range map[string]string{
		"postgres://app:s3cret@db.internal:5433/ytdl?sslmode=disable":     "app@db.internal:5433/ytdl",
		"host=db.internal port=5433 user=app password=s3cret dbname=ytdl": "app@db.internal:5433/ytdl",
		"": "missing",
	} {
		got := databaseLabel(dsn)
		if got != want {
			t.Errorf("databaseLabel(%q) = %q, want %q", dsn, got, want)
		}
		if strings.Contains(got, "s3cret") {
			t.Errorf("password leaked: %q", got)
		}
	}
}

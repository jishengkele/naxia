package db

import (
	"strings"
	"testing"
)

func TestProfileConcurrencyRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	p := &LLMProfile{Name: "concurrency-roundtrip", Format: "openai", Model: "fixture", APIKey: "original-key", MaxConcurrency: 3, Streaming: true}
	id, err := d.SaveProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	for _, next := range []struct {
		max int
		key string
	}{{3, ""}, {1, ""}, {5, "replacement-key"}, {0, ""}} {
		p.ID, p.MaxConcurrency, p.APIKey = id, next.max, next.key
		if _, err := d.SaveProfile(p); err != nil {
			t.Fatal(err)
		}
		loaded, err := d.ProfileByID(id)
		if err != nil || loaded == nil || loaded.MaxConcurrency != next.max {
			t.Fatalf("keyed load=%+v err=%v", loaded, err)
		}
		profiles, err := d.ListProfiles()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, profile := range profiles {
			if profile.ID == id {
				found = profile.MaxConcurrency == next.max
			}
		}
		if !found {
			t.Fatal("list projection lost concurrency limit")
		}
		pool, err := d.PoolProfiles()
		if err != nil {
			t.Fatal(err)
		}
		found = false
		for _, profile := range pool {
			if profile.ID == id {
				found = profile.MaxConcurrency == next.max
			}
		}
		if !found {
			t.Fatal("pool projection lost concurrency limit")
		}
		if next.key == "replacement-key" && loaded.APIKey != next.key {
			t.Fatal("keyed update changed field positions")
		}
	}
	for _, limit := range []int{-1, 2147483648} {
		p.MaxConcurrency = limit
		if _, err := d.SaveProfile(p); err == nil {
			t.Fatalf("accepted limit %d", limit)
		}
	}
}

func TestProfileConcurrencyOldSchemaDefault(t *testing.T) {
	// Apply the actual startup migration to an old schema without touching public.
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE SCHEMA concurrency_migration_test; SET LOCAL search_path TO concurrency_migration_test, public`); err != nil {
		t.Fatal(err)
	}
	oldLines := []string{}
	for _, line := range strings.Split(schemaSQL, "\n") {
		if !strings.Contains(line, "max_concurrency") {
			oldLines = append(oldLines, line)
		}
	}
	if _, err := tx.Exec(strings.Join(oldLines, "\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO llm_profiles(name,format,model) VALUES ('legacy','openai','fixture')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(schemaSQL); err != nil {
		t.Fatal(err)
	}
	var limit int
	if err := tx.QueryRow(`SELECT max_concurrency FROM llm_profiles WHERE name='legacy'`).Scan(&limit); err != nil || limit != 0 {
		t.Fatalf("legacy limit=%d err=%v", limit, err)
	}
	if _, err := tx.Exec(`UPDATE llm_profiles SET max_concurrency=-1 WHERE name='legacy'`); err == nil {
		t.Fatal("database accepted a negative limit")
	}
}

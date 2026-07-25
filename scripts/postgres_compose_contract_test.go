package main

import (
	"os"
	"strings"
	"testing"
)

func TestPostgresTestComposeUsesNamedDataVolume(t *testing.T) {
	data, err := os.ReadFile("../test/postgres/docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for required, want := range map[string]int{
		"      - postgres_data:/var/lib/postgresql/data": 1,
		"\nvolumes:\n  postgres_data:\n":                 1,
	} {
		if got := strings.Count(source, required); got != want {
			t.Fatalf("Postgres Compose source count for %q = %d, want %d", required, got, want)
		}
	}
	if strings.Contains(source, "      - /var/lib/postgresql/data") {
		t.Fatal("Postgres Compose restores an anonymous data volume")
	}
}

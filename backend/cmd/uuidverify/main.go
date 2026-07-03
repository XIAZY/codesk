package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"notty/backend/internal/notty"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "snapshot":
		runSnapshot(os.Args[2:])
	case "verify":
		runVerify(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func runSnapshot(args []string) {
	flags := flag.NewFlagSet("snapshot", flag.ExitOnError)
	databaseURL := flags.String("database-url", os.Getenv("NOTTY_DATABASE_URL"), "Postgres database URL")
	out := flags.String("out", "", "snapshot JSON output path")
	_ = flags.Parse(args)
	if *databaseURL == "" || *out == "" {
		flags.Usage()
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := openDB(ctx, *databaseURL)
	defer db.Close()
	snapshot, err := notty.CaptureUUIDMigrationSnapshot(ctx, db)
	if err != nil {
		log.Fatalf("capture snapshot: %v", err)
	}
	if err := writeJSONFile(*out, snapshot); err != nil {
		log.Fatalf("write snapshot: %v", err)
	}
	fmt.Printf("snapshot written to %s\n", *out)
}

func runVerify(args []string) {
	flags := flag.NewFlagSet("verify", flag.ExitOnError)
	databaseURL := flags.String("database-url", os.Getenv("NOTTY_DATABASE_URL"), "Postgres database URL")
	beforePath := flags.String("before", "", "pre-migration snapshot JSON path")
	mappingPath := flags.String("mapping", "", "old-to-new mapping JSON path")
	mappingTable := flags.String("mapping-table", "", "old-to-new mapping table with entity_type, old_id, new_id")
	checkOpaquePayloads := flags.Bool("check-opaque-payloads", false, "fail if opaque JSON/log payloads still contain old prefixed IDs")
	_ = flags.Parse(args)
	if *databaseURL == "" || *beforePath == "" || (*mappingPath == "" && *mappingTable == "") {
		flags.Usage()
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := openDB(ctx, *databaseURL)
	defer db.Close()
	before := readSnapshot(*beforePath)
	after, err := notty.CaptureUUIDMigrationSnapshot(ctx, db)
	if err != nil {
		log.Fatalf("capture post-migration snapshot: %v", err)
	}
	mappings := []notty.UUIDMigrationMapping{}
	if *mappingPath != "" {
		mappings = append(mappings, readMappingFile(*mappingPath)...)
	}
	if *mappingTable != "" {
		tableMappings, err := notty.LoadUUIDMigrationMappingsFromTable(ctx, db, *mappingTable)
		if err != nil {
			log.Fatalf("load mapping table: %v", err)
		}
		mappings = append(mappings, tableMappings...)
	}
	issues := notty.VerifyUUIDMigrationSnapshots(before, after, mappings)
	databaseIssues, err := notty.VerifyUUIDMigrationDatabase(ctx, db)
	if err != nil {
		log.Fatalf("verify database: %v", err)
	}
	issues = append(issues, databaseIssues...)
	if *checkOpaquePayloads {
		opaqueIssues, err := notty.VerifyUUIDMigrationJSONPayloads(ctx, db)
		if err != nil {
			log.Fatalf("verify opaque payloads: %v", err)
		}
		issues = append(issues, opaqueIssues...)
	}
	if len(issues) == 0 {
		fmt.Println("uuid migration verification passed")
		return
	}
	encoded, err := json.MarshalIndent(issues, "", "  ")
	if err != nil {
		log.Fatalf("encode issues: %v", err)
	}
	fmt.Println(string(encoded))
	os.Exit(1)
}

func openDB(ctx context.Context, databaseURL string) *sql.DB {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		log.Fatalf("ping database: %v", err)
	}
	return db
}

func readSnapshot(path string) *notty.UUIDMigrationSnapshot {
	var snapshot notty.UUIDMigrationSnapshot
	readJSONFile(path, &snapshot)
	return &snapshot
}

func readMappingFile(path string) []notty.UUIDMigrationMapping {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read %s: %v", path, err)
	}
	var file notty.UUIDMigrationMappingFile
	if err := json.Unmarshal(data, &file); err == nil && len(file.Mappings) > 0 {
		return file.Mappings
	}
	var mappings []notty.UUIDMigrationMapping
	if err := json.Unmarshal(data, &mappings); err != nil {
		log.Fatalf("decode %s: %v", path, err)
	}
	return mappings
}

func readJSONFile(path string, dest any) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, dest); err != nil {
		log.Fatalf("decode %s: %v", path, err)
	}
}

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage:
  uuidverify snapshot --database-url "$NOTTY_DATABASE_URL" --out before.json
  uuidverify verify --database-url "$NOTTY_DATABASE_URL" --before before.json --mapping mapping.json
  uuidverify verify --database-url "$NOTTY_DATABASE_URL" --before before.json --mapping-table uuid_migration_map

The mapping table must expose entity_type, old_id, and new_id columns.
Opaque JSON/log payloads are not hard-gated by default; pass --check-opaque-payloads
when deliberately scrubbing them in the migration window.
`)
}

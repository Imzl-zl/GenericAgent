package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("Unable to connect to database: %v", err)
	}
	defer pool.Close()

	// Check if config column exists
	var exists bool
	err = pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM information_schema.columns
			WHERE table_name = 'llm_providers'
			AND column_name = 'config'
		)
	`).Scan(&exists)

	if err != nil {
		log.Fatalf("Query failed: %v", err)
	}

	if exists {
		fmt.Println("✅ config column EXISTS in llm_providers table")
	} else {
		fmt.Println("❌ config column DOES NOT EXIST in llm_providers table")
		fmt.Println("Running ALTER TABLE to add it...")

		_, err = pool.Exec(ctx, `
			ALTER TABLE llm_providers
			ADD COLUMN IF NOT EXISTS config JSONB NOT NULL DEFAULT '{}'::jsonb
		`)

		if err != nil {
			log.Fatalf("Failed to add config column: %v", err)
		}

		fmt.Println("✅ config column added successfully")
	}

	// Show current table structure
	fmt.Println("\nCurrent llm_providers columns:")
	rows, err := pool.Query(ctx, `
		SELECT column_name, data_type, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_name = 'llm_providers'
		ORDER BY ordinal_position
	`)
	if err != nil {
		log.Fatalf("Failed to query columns: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name, dataType, nullable string
		var def *string
		if err := rows.Scan(&name, &dataType, &nullable, &def); err != nil {
			log.Fatalf("Scan failed: %v", err)
		}
		defStr := "NULL"
		if def != nil {
			defStr = *def
		}
		fmt.Printf("  - %s (%s) nullable=%s default=%s\n", name, dataType, nullable, defStr)
	}
}

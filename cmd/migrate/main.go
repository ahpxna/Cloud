package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"family-photo-cloud/internal/database"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	migrations, err := database.LoadMigrations(env("MIGRATIONS_DIR", "/app/migrations"))
	if err != nil {
		log.Fatal(err)
	}
	baseline, err := optionalPositiveInt("PHOTO_MIGRATION_BASELINE_VERSION")
	if err != nil {
		log.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("connect PostgreSQL: %v", err)
	}
	defer conn.Close(context.Background())
	if err := database.Apply(ctx, conn, migrations, baseline); err != nil {
		log.Fatal(err)
	}
	log.Printf("schema is current through migration %04d", migrations[len(migrations)-1].Version)
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func optionalPositiveInt(name string) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive migration version", name)
	}
	return value, nil
}

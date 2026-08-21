package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"family-photo-cloud/internal/account"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/term"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "admin:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 || os.Args[1] != "create-user" {
		return errors.New("usage: admin create-user -email user@example.com [-role member|admin]")
	}
	flags := flag.NewFlagSet("create-user", flag.ContinueOnError)
	email := flags.String("email", "", "family member email")
	role := flags.String("role", "member", "member or admin")
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	*email = strings.ToLower(strings.TrimSpace(*email))
	if *email == "" || (*role != "member" && *role != "admin") {
		return errors.New("valid -email and -role member|admin are required")
	}

	fmt.Fprint(os.Stderr, "Password (minimum 12 characters): ")
	var password []byte
	var err error
	stdinFD := int(os.Stdin.Fd())
	if term.IsTerminal(stdinFD) {
		password, err = term.ReadPassword(stdinFD)
		fmt.Fprintln(os.Stderr)
	} else {
		password, err = bufio.NewReader(os.Stdin).ReadBytes('\n')
		password = []byte(strings.TrimRight(string(password), "\r\n"))
	}
	if err != nil {
		return err
	}
	passwordHash, err := account.HashPassword(string(password))
	for index := range password {
		password[index] = 0
	}
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	var id string
	err = pool.QueryRow(ctx, `
        INSERT INTO users (email, password_hash, role, state)
        VALUES ($1, $2, $3, 'active')
        RETURNING id::text`, *email, passwordHash, *role).Scan(&id)
	if err != nil {
		return err
	}
	fmt.Printf("created %s user %s (%s)\n", *role, *email, id)
	return nil
}

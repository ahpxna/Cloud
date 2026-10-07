package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"family-photo-cloud/internal/account"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/term"
)

const usage = `usage: admin <command> [flags]

commands:
  create-user     -email EMAIL [-role member|admin]   create an invite-only account (prompts for password)
  list-users                                          show accounts, state, MFA and active devices
  disable-user    -email EMAIL                        block sign-in and revoke every device
  enable-user     -email EMAIL                        allow sign-in again (devices stay revoked)
  reset-password  -email EMAIL                        set a new password (prompts) and revoke every device
  revoke-sessions -email EMAIL                        sign every device out, e.g. after a lost phone
  reset-mfa       -email EMAIL                        remove a lost authenticator and revoke every device
  delete-user     -email EMAIL -confirm EMAIL         start deletion: block sign-in, revoke devices, destroy MFA`

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "admin:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin *os.File, stdout, stderr io.Writer) error {
	if len(args) < 1 {
		return errors.New(usage)
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	email := flags.String("email", "", "family member email")
	role := flags.String("role", "member", "member or admin (create-user only)")
	confirm := flags.String("confirm", "", "repeat the email to confirm delete-user")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	*email = strings.ToLower(strings.TrimSpace(*email))
	needsEmail := command != "list-users"
	if needsEmail && *email == "" {
		return fmt.Errorf("-email is required\n%s", usage)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	connect := func() (*account.AdminRepository, func(), error) {
		pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
		if err != nil {
			return nil, nil, err
		}
		return account.NewAdminRepository(pool), pool.Close, nil
	}

	switch command {
	case "create-user":
		if *role != "member" && *role != "admin" {
			return errors.New("-role must be member or admin")
		}
		passwordHash, err := promptPasswordHash(stdin, stderr)
		if err != nil {
			return err
		}
		repository, closePool, err := connect()
		if err != nil {
			return err
		}
		defer closePool()
		id, err := repository.CreateUser(ctx, *email, passwordHash, *role)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "created %s user %s (%s)\n", *role, *email, id)
	case "list-users":
		repository, closePool, err := connect()
		if err != nil {
			return err
		}
		defer closePool()
		accounts, err := repository.ListUsers(ctx)
		if err != nil {
			return err
		}
		table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "EMAIL\tROLE\tSTATE\tMFA\tDEVICES\tCREATED\tID")
		for _, summary := range accounts {
			mfa := "off"
			if summary.MFAEnabled {
				mfa = "on"
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", summary.Email, summary.Role, summary.State, mfa,
				summary.ActiveDevices, summary.CreatedAt.UTC().Format("2006-01-02"), summary.ID)
		}
		return table.Flush()
	case "disable-user", "revoke-sessions", "reset-mfa":
		repository, closePool, err := connect()
		if err != nil {
			return err
		}
		defer closePool()
		action := map[string]func(context.Context, string) (int64, error){
			"disable-user":    repository.Disable,
			"revoke-sessions": repository.RevokeSessions,
			"reset-mfa":       repository.ResetMFA,
		}[command]
		revoked, err := action(ctx, *email)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s: %s done; revoked %d device session(s)\n", command, *email, revoked)
	case "enable-user":
		repository, closePool, err := connect()
		if err != nil {
			return err
		}
		defer closePool()
		if err := repository.Enable(ctx, *email); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "enabled %s; the person must sign in again\n", *email)
	case "reset-password":
		passwordHash, err := promptPasswordHash(stdin, stderr)
		if err != nil {
			return err
		}
		repository, closePool, err := connect()
		if err != nil {
			return err
		}
		defer closePool()
		revoked, err := repository.ResetPassword(ctx, *email, passwordHash)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "password reset for %s; revoked %d device session(s)\n", *email, revoked)
	case "delete-user":
		if strings.ToLower(strings.TrimSpace(*confirm)) != *email {
			return errors.New("delete-user requires -confirm with the same email")
		}
		repository, closePool, err := connect()
		if err != nil {
			return err
		}
		defer closePool()
		revoked, err := repository.MarkDeleted(ctx, *email)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "marked %s for deletion; revoked %d device session(s). "+
			"Finish the purge with docs/runbooks/account-lifecycle.md\n", *email, revoked)
	default:
		return fmt.Errorf("unknown command %q\n%s", command, usage)
	}
	return nil
}

func promptPasswordHash(stdin *os.File, stderr io.Writer) (string, error) {
	fmt.Fprint(stderr, "Password (minimum 12 characters): ")
	var password []byte
	var err error
	stdinFD := int(stdin.Fd())
	if term.IsTerminal(stdinFD) {
		password, err = term.ReadPassword(stdinFD)
		fmt.Fprintln(stderr)
	} else {
		password, err = bufio.NewReader(stdin).ReadBytes('\n')
		if errors.Is(err, io.EOF) && len(password) > 0 {
			err = nil
		}
		password = []byte(strings.TrimRight(string(password), "\r\n"))
	}
	if err != nil {
		return "", err
	}
	passwordHash, err := account.HashPassword(string(password))
	for index := range password {
		password[index] = 0
	}
	return passwordHash, err
}

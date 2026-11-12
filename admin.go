package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"github.com/outhaul-dev/outhaul/internal/config"
	"github.com/outhaul-dev/outhaul/internal/server"
	"github.com/outhaul-dev/outhaul/internal/store"
	"golang.org/x/term"
)

// runAdmin implements `outhaul admin create NAME`. A usage error is 2, every
// other failure 1.
func runAdmin(args []string, getenv config.Getenv, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 2 || args[0] != "create" {
		fmt.Fprintln(stderr, "usage: outhaul admin create NAME")
		return 2
	}
	cfg := config.Load(getenv)
	if err := adminCreate(context.Background(), cfg, args[1], stdin, stdout); err != nil {
		fmt.Fprintf(stderr, "outhaul: %v\n", err)
		return 1
	}
	return 0
}

// adminCreate creates a password system admin in the database the server uses.
// It never creates or migrates the database, and it writes nothing to
// Outhaul's log.
func adminCreate(ctx context.Context, cfg config.Config, name string, stdin io.Reader, out io.Writer) error {
	if err := server.ValidUsername(name); err != nil {
		return err
	}
	path := cfg.DBPath()
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no Outhaul database at %s; install Outhaul first", path)
		}
		return err
	}
	if err := checkDatabaseOwner(path, name, os.Geteuid()); err != nil {
		return err
	}
	st, err := store.OpenExisting(path)
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := st.GetUserByName(ctx, name); err == nil {
		return fmt.Errorf("There is already an account called %s.", name)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	password, err := readAdminPassword(stdin, out)
	if err != nil {
		return err
	}
	if err := server.ValidateCredentials(name, password); err != nil {
		return err
	}
	hash, err := server.HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := st.CreateUser(ctx, name, hash); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("There is already an account called %s.", name)
		}
		return err
	}
	fmt.Fprintf(out, "Created system admin %q.\n", name)
	return nil
}

// readAdminPassword asks twice without echo at a terminal. When standard input
// isn't a terminal, the first line of it is the password.
func readAdminPassword(in io.Reader, out io.Writer) (string, error) {
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(out, "Password: ")
		first, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return "", err
		}
		fmt.Fprint(out, "Password again: ")
		second, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return "", err
		}
		if string(first) != string(second) {
			return "", errors.New("the two passwords don't match")
		}
		return string(first), nil
	}

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if line == "" {
		return "", errors.New("no password on standard input")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// checkDatabaseOwner refuses a user other than the database file's owner, so
// root can't leave files in the data directory the service can't write. The
// refusal names the sudo -u line that works.
func checkDatabaseOwner(path, name string, euid int) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	uid := int(st.Uid)
	if uid == euid {
		return nil
	}
	owner := strconv.Itoa(uid)
	if u, err := user.LookupId(owner); err == nil {
		owner = u.Username
	}
	return fmt.Errorf("run it as %s, the account that owns the database: sudo -u %s outhaul admin create %s", owner, owner, name)
}

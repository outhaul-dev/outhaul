package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/outhaul-dev/outhaul/internal/backup"
	"github.com/outhaul-dev/outhaul/internal/config"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/secret"
	"github.com/outhaul-dev/outhaul/internal/store"
)

// keyHelp is the sentence every key refusal carries. Leaving out --key-file
// on a fresh host is the likeliest mistake: the install's own new key can
// never open the copy.
const keyHelp = "pass the original with --key-file. The key the fresh install made for itself can't work."

type restoreOptions struct {
	From    string // a path (Task 11); later "latest" or a stamp (Task 12)
	KeyFile string
}

func parseRestoreOptions(args []string) (restoreOptions, error) {
	fs := flag.NewFlagSet("restore-state", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "", "the copy to restore: a file path")
	keyFile := fs.String("key-file", "", "the 32 raw bytes of secret.key")
	if err := fs.Parse(args); err != nil {
		return restoreOptions{}, err
	}
	if fs.NArg() != 0 {
		return restoreOptions{}, errors.New("restore-state takes no positional arguments")
	}
	if *from == "" {
		return restoreOptions{}, errors.New("--from is required")
	}
	return restoreOptions{From: *from, KeyFile: *keyFile}, nil
}

// runRestoreState is the command entry point. A usage error is 2, every other
// failure 1.
func runRestoreState(args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	opts, err := parseRestoreOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "outhaul: %v\n", err)
		return 2
	}
	cfg := config.Load(getenv)
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "outhaul: %v\n", err)
		return 1
	}
	if err := restoreState(context.Background(), cfg, opts, stdout); err != nil {
		fmt.Fprintf(stderr, "outhaul: %v\n", err)
		return 1
	}
	return 0
}

// restoreState puts a copy back on an empty install. Every refusal leaves the
// host's database and key as they were.
func restoreState(ctx context.Context, cfg config.Config, opts restoreOptions, out io.Writer) error {
	info, err := os.Stat(cfg.DataDir)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("install Outhaul first: %s does not exist", cfg.DataDir)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("install Outhaul first: %s is not a directory", cfg.DataDir)
	}

	dbPath := cfg.DBPath()
	if _, err := os.Stat(dbPath); err == nil {
		if err := store.CheckDatabaseIdle(dbPath); err != nil {
			if errors.Is(err, store.ErrDatabaseInUse) {
				return errors.New("stop Outhaul first: the database is open by another process")
			}
			return err
		}
		has, err := store.HasUserInFile(dbPath)
		if err != nil {
			return err
		}
		if has {
			return errors.New("this fills an empty install only; to replace this one, stop Outhaul and move outhaul.db, its -wal and -shm, and secret.key away yourself")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	staged := filepath.Join(cfg.DataDir, "outhaul.db.restoring")
	cleanup := func() {
		for _, p := range []string{staged, staged + "-wal", staged + "-shm"} {
			os.Remove(p)
		}
	}
	// A killed earlier attempt can leave the staged file in WAL mode. Removing
	// the sidecars too is what keeps SQLite from replaying them into this copy.
	cleanup()
	done := false
	defer func() {
		if !done {
			cleanup()
		}
	}()

	if err := stageCopy(opts.From, staged); err != nil {
		return err
	}
	before, err := store.CheckStateCopy(staged)
	if err != nil {
		return err
	}
	keyRaw, box, err := restoreKey(cfg, opts)
	if err != nil {
		return err
	}

	st, err := store.Open(staged, box)
	if err != nil {
		return fmt.Errorf("open the staged copy: %w", err)
	}
	sealed, err := st.CheckSealed(ctx)
	if err != nil {
		st.Close()
		return fmt.Errorf("%v; %s", err, keyHelp)
	}
	accounts, apps, databases, err := st.StateCounts(ctx)
	if err != nil {
		st.Close()
		return err
	}
	if err := st.StripSessions(ctx); err != nil {
		st.Close()
		return err
	}
	stamp := restoreStamp(opts.From, time.Now())
	if err := st.SetRestoreMarker(ctx, core.RestoreMarker{
		RestoredAt: time.Now().UTC(), From: stamp, Pending: true,
	}); err != nil {
		st.Close()
		return err
	}
	after, err := st.AppliedMigrations(ctx)
	if err != nil {
		st.Close()
		return err
	}
	if err := closeStaged(st, staged); err != nil {
		return err
	}

	if err := store.CheckDatabaseIdle(dbPath); err != nil {
		if errors.Is(err, store.ErrDatabaseInUse) {
			return errors.New("stop Outhaul first: the database is open by another process")
		}
		return err
	}
	moved, err := swapIn(cfg, staged, keyRaw, stamp)
	if err != nil {
		return err
	}
	done = true

	applied := diffMigrations(before, after)
	fmt.Fprintf(out, "restored %s from %s\n", stamp, opts.From)
	fmt.Fprintf(out, "accounts: %d, apps: %d, databases: %d\n", accounts, apps, databases)
	fmt.Fprintf(out, "sealed values opened: %d\n", sealed)
	fmt.Fprintf(out, "migrations applied: %s\n", strings.Join(applied, ", "))
	if len(moved) > 0 {
		fmt.Fprintf(out, "moved aside: %s\n", strings.Join(moved, ", "))
	}
	fmt.Fprintln(out, "The first start will re-create managed databases and apps, then list what is still left to do.")
	return nil
}

// stageCopy opens src as gzip or a plain SQLite file, by its first bytes, and
// writes the staged database mode 0600.
func stageCopy(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer f.Close()
	br := bufio.NewReader(f)
	head, _ := br.Peek(16)
	var r io.Reader = br
	if bytes.HasPrefix(head, []byte{0x1f, 0x8b}) {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("%w: %s is not a gzip file: %v", store.ErrNotStateCopy, src, err)
		}
		defer gz.Close()
		r = gz
	} else if !bytes.HasPrefix(head, []byte("SQLite format 3\x00")) {
		return fmt.Errorf("%w: %s is not gzip or a SQLite database", store.ErrNotStateCopy, src)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return fmt.Errorf("write the staged copy: %w", err)
	}
	return out.Close()
}

// restoreKey reads the key from --key-file, or the install's own key. A
// missing key is nil, and CheckSealed refuses a copy with sealed values.
func restoreKey(cfg config.Config, opts restoreOptions) ([]byte, *secret.Box, error) {
	if opts.KeyFile != "" {
		raw, err := os.ReadFile(opts.KeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("%v; %s", err, keyHelp)
		}
		box, err := secret.NewBox(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("%v; %s", err, keyHelp)
		}
		return raw, box, nil
	}
	raw, err := os.ReadFile(cfg.SecretKeyPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	box, err := secret.NewBox(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("%v; %s", err, keyHelp)
	}
	return raw, box, nil
}

// closeStaged closes the staged database. A -wal that survives the close holds
// the marker or migrations that never reached the main file, so it is a
// refusal; a -shm is only an index and is removed.
func closeStaged(st *store.Store, path string) error {
	if err := st.Close(); err != nil {
		return err
	}
	if _, err := os.Stat(path + "-wal"); err == nil {
		return fmt.Errorf("the staged database still has a -wal beside it (%s-wal); refusing to replace the host with it", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(path + "-shm"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// swapIn writes the supplied key first, moves the old key and database aside,
// then puts the staged database in place. Nothing is deleted.
func swapIn(cfg config.Config, staged string, keyRaw []byte, stamp string) ([]string, error) {
	var moved []string
	keyPath := cfg.SecretKeyPath()
	if keyRaw != nil {
		old, err := os.ReadFile(keyPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return moved, err
		}
		if !bytes.Equal(old, keyRaw) {
			if err == nil {
				dst, err := moveAside(keyPath, stamp)
				if err != nil {
					return moved, err
				}
				moved = append(moved, dst)
			}
			if err := writeKey(keyPath, keyRaw); err != nil {
				return moved, err
			}
		}
	}

	dbPath := cfg.DBPath()
	suffix := stamp
	for i := 2; ; i++ {
		if _, err := os.Stat(dbPath + ".before-restore-" + suffix); errors.Is(err, os.ErrNotExist) {
			break
		}
		suffix = fmt.Sprintf("%s-%d", stamp, i)
	}
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(p); err == nil {
			dst := p + ".before-restore-" + suffix
			if err := os.Rename(p, dst); err != nil {
				return moved, err
			}
			moved = append(moved, dst)
		} else if !errors.Is(err, os.ErrNotExist) {
			return moved, err
		}
	}
	if err := chownLike(staged, cfg.DataDir); err != nil {
		return moved, err
	}
	if err := os.Rename(staged, dbPath); err != nil {
		return moved, err
	}
	return moved, nil
}

func moveAside(path, stamp string) (string, error) {
	dst := path + ".before-restore-" + stamp
	for i := 2; ; i++ {
		if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
			break
		}
		dst = fmt.Sprintf("%s.before-restore-%s-%d", path, stamp, i)
	}
	if err := os.Rename(path, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// writeKey writes raw through a temp file, mode 0600, and renames it into
// place, so a reader never sees a half-written key.
func writeKey(path string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".secret.key-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := chownLike(tmp, filepath.Dir(path)); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// chownLike gives a file the data directory's owner and group when the
// command runs as root, so the service can open it.
func chownLike(path, dir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return os.Chown(path, int(st.Uid), int(st.Gid))
}

// restoreStamp is the copy's stamp: its file name when that is a stamp,
// otherwise the time of the restore.
func restoreStamp(from string, now time.Time) string {
	base := filepath.Base(from)
	base = strings.TrimSuffix(base, ".gz")
	base = strings.TrimSuffix(base, ".db")
	if _, err := time.Parse("20060102T150405Z", base); err == nil {
		return base
	}
	return backup.Stamp(now)
}

func diffMigrations(before, after []string) []string {
	seen := make(map[string]bool, len(before))
	for _, n := range before {
		seen[n] = true
	}
	var out []string
	for _, n := range after {
		if !seen[n] {
			out = append(out, n)
		}
	}
	return out
}

package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/secret"
)

// HasSealedValues guards secret.key regeneration: a new key would make every
// value sealed with the old one unreadable, so each kind of sealed value must
// count.
func TestHasSealedValuesIgnoresAnUnsealedDestination(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`INSERT INTO destinations
		(name, endpoint, region, bucket, access_key, secret_key, created_at, kind)
		VALUES ('Azure Blob', 'https://examplebackups.blob.core.windows.net', '', 'outhaul', '', '', '2026-01-01T00:00:00Z', 'azure')`); err != nil {
		t.Fatal(err)
	}
	has, err := s.HasSealedValues(context.Background())
	if err != nil || has {
		t.Fatalf("HasSealedValues = %v, %v; want false, nil", has, err)
	}
}

func TestCheckSealedWithTheRightKey(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	if _, err := s.CreateApp(ctx, core.App{Name: "web", RepoURL: "git@example.com:o/r.git", Domain: "web.test", SSHPrivateKey: "PRIVATE"}); err != nil {
		t.Fatal(err)
	}
	app, err := s.CreateApp(ctx, core.App{Name: "envapp", RepoURL: "https://example.com/o/r.git", Domain: "env.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnv(ctx, app.ID, "K", "v", false); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProject(ctx, core.Project{Name: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectEnv(ctx, p.ID, "K", "v", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDatabase(ctx, core.Database{Name: "db", Engine: core.EnginePostgres, Image: "img", Username: "u", Password: "pw", DBName: "db"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDestination(ctx, core.Destination{Name: "s3", Endpoint: "http://m:9000", Bucket: "b", AccessKey: "AK", SecretKey: "SK"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateGithubAppSource(ctx, core.GithubAppCreds{AppID: 1, Slug: "o", PrivateKey: "PEM", WebhookSecret: "w", ClientID: "c", ClientSecret: "cs"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSSOSettings(ctx, SSOSettings{Issuer: "https://idp.example.com", ClientID: "c", ClientSecret: "s"}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCloudflareToken(ctx, "tok"); err != nil {
		t.Fatal(err)
	}

	n, err := s.CheckSealed(ctx)
	if err != nil || n != 10 {
		t.Fatalf("CheckSealed = %d, %v; want 10, nil", n, err)
	}
}

func TestCheckSealedWithTheWrongKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	box, err := secret.Load(filepath.Join(t.TempDir(), "key"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, box)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "git@example.com:o/r.git", Domain: "web.test", SSHPrivateKey: "PRIVATE"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	other, err := secret.Load(filepath.Join(t.TempDir(), "key2"))
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, other)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.CheckSealed(context.Background()); err == nil || !strings.Contains(err.Error(), "decryption failed") {
		t.Fatalf("CheckSealed with another key = %v, want decryption failed", err)
	}
}

func TestCheckSealedWithNothingSealed(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	if n, err := s.CheckSealed(ctx); err != nil || n != 0 {
		t.Fatalf("empty store: CheckSealed = %d, %v", n, err)
	}
	if _, err := s.db.Exec(`INSERT INTO destinations
		(name, endpoint, region, bucket, access_key, secret_key, created_at, kind)
		VALUES ('Azure Blob', 'https://examplebackups.blob.core.windows.net', '', 'outhaul', '', '', '2026-01-01T00:00:00Z', 'azure')`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CheckSealed(ctx); err != nil || n != 0 {
		t.Fatalf("with only an unsealed destination: CheckSealed = %d, %v", n, err)
	}
}

func TestCheckSealedWithoutABox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	box, err := secret.Load(filepath.Join(t.TempDir(), "key"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, box)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "git@example.com:o/r.git", Domain: "web.test", SSHPrivateKey: "PRIVATE"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.CheckSealed(context.Background()); err == nil || !strings.Contains(err.Error(), "--key-file") {
		t.Fatalf("CheckSealed without a box = %v, want --key-file", err)
	}
}

func TestHasSealedValues(t *testing.T) {
	ctx := context.Background()
	seeds := map[string]func(t *testing.T, s *Store){
		"app deploy key": func(t *testing.T, s *Store) {
			if _, err := s.CreateApp(ctx, core.App{Name: "web", RepoURL: "git@x:y.git", Domain: "web.test", SSHPrivateKey: "PRIVATE"}); err != nil {
				t.Fatal(err)
			}
		},
		"app env": func(t *testing.T, s *Store) {
			app, err := s.CreateApp(ctx, core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SetEnv(ctx, app.ID, "K", "v", false); err != nil {
				t.Fatal(err)
			}
		},
		"project env": func(t *testing.T, s *Store) {
			p, err := s.CreateProject(ctx, core.Project{Name: "shop"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SetProjectEnv(ctx, p.ID, "K", "v", false); err != nil {
				t.Fatal(err)
			}
		},
		"database password": func(t *testing.T, s *Store) {
			if _, err := s.CreateDatabase(ctx, core.Database{Name: "db", Engine: core.EnginePostgres, Image: "img", Username: "u", Password: "pw", DBName: "db"}); err != nil {
				t.Fatal(err)
			}
		},
		"backup destination": func(t *testing.T, s *Store) {
			if _, err := s.CreateDestination(ctx, core.Destination{Name: "s3", Endpoint: "http://m:9000", Bucket: "b", AccessKey: "AK", SecretKey: "SK"}); err != nil {
				t.Fatal(err)
			}
		},
		"github app": func(t *testing.T, s *Store) {
			if _, err := s.CreateGithubAppSource(ctx, core.GithubAppCreds{AppID: 1, Slug: "o", PrivateKey: "PEM", WebhookSecret: "w", ClientID: "c", ClientSecret: "cs"}); err != nil {
				t.Fatal(err)
			}
		},
		"sso client secret": func(t *testing.T, s *Store) {
			if err := s.SetSSOSettings(ctx, SSOSettings{Issuer: "https://idp", ClientID: "c", ClientSecret: "s"}, false); err != nil {
				t.Fatal(err)
			}
		},
		"cloudflare token": func(t *testing.T, s *Store) {
			if err := s.SetCloudflareToken(ctx, "tok"); err != nil {
				t.Fatal(err)
			}
		},
	}

	if has, err := openWithBox(t).HasSealedValues(ctx); err != nil || has {
		t.Fatalf("empty store: HasSealedValues = %v, %v; want false", has, err)
	}
	plain := openWithBox(t)
	if _, err := plain.CreateApp(ctx, core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"}); err != nil {
		t.Fatal(err)
	}
	if has, err := plain.HasSealedValues(ctx); err != nil || has {
		t.Errorf("app without secrets: HasSealedValues = %v, %v; want false", has, err)
	}
	for name, seed := range seeds {
		t.Run(name, func(t *testing.T) {
			s := openWithBox(t)
			seed(t, s)
			if has, err := s.HasSealedValues(ctx); err != nil || !has {
				t.Errorf("HasSealedValues = %v, %v; want true", has, err)
			}
		})
	}
}

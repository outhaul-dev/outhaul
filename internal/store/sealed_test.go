package store

import (
	"context"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// HasSealedValues guards secret.key regeneration: a new key would make every
// value sealed with the old one unreadable, so each kind of sealed value must
// count.
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

package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// seedSecrets puts a sentinel in every place a page could leak a secret from.
func seedSecrets(t *testing.T, e *testEnv) seededProject {
	t.Helper()
	ctx := context.Background()
	sp := seedProject(t, e, "acme")
	for _, err := range []error{
		e.store.SetEnv(ctx, sp.App.ID, "PLAIN", "plain-sentinel", false),
		e.store.SetEnv(ctx, sp.App.ID, "HIDDEN", "secret-sentinel", true),
		e.store.SetProjectEnv(ctx, sp.Project.ID, "PLAIN", "plain-sentinel", false),
		e.store.SetProjectEnv(ctx, sp.Project.ID, "HIDDEN", "secret-sentinel", true),
		e.store.SetWebhookCredentials(ctx, sp.App.ID, "whtok-sentinel", "whsec-sentinel"),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.store.CreateDatabase(ctx, core.Database{
		Name: "secretdb", ProjectID: sp.Project.ID, Engine: core.EnginePostgres, Image: "postgres:17",
		Username: "u", DBName: "d", Password: "dbpw-sentinel",
	}); err != nil {
		t.Fatal(err)
	}
	return sp
}

func secretDatabase(t *testing.T, e *testEnv, projectID int64) core.Database {
	t.Helper()
	dbs, err := e.store.ListDatabasesByProject(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dbs {
		if d.Name == "secretdb" {
			return d
		}
	}
	t.Fatal("secretdb not found")
	return core.Database{}
}

func TestViewerSeesNoSecrets(t *testing.T) {
	e := newTestEnv(t)
	sp := seedSecrets(t, e)
	db := secretDatabase(t, e, sp.Project.ID)
	viewer := e.signInAs(t, "vera", core.RoleProjectViewer, sp.Project.ID)
	member := e.signInAs(t, "max", core.RoleProjectMember, sp.Project.ID)

	paths := []string{
		fmt.Sprintf("/apps/%d", sp.App.ID),
		fmt.Sprintf("/projects/%d", sp.Project.ID),
		fmt.Sprintf("/databases/%d", db.ID),
		fmt.Sprintf("/databases/%d", sp.Database.ID),
	}
	sentinels := []string{"plain-sentinel", "secret-sentinel", "whsec-sentinel", "whtok-sentinel", "dbpw-sentinel", "pw123"}
	for _, p := range paths {
		resp := e.getAs(t, viewer, p)
		if resp.StatusCode != 200 {
			t.Fatalf("viewer GET %s = %d", p, resp.StatusCode)
		}
		html := body(t, resp)
		for _, s := range sentinels {
			if strings.Contains(html, s) {
				t.Errorf("viewer GET %s leaks %q", p, s)
			}
		}
	}

	// The keys and status are still shown.
	html := body(t, e.getAs(t, viewer, paths[0]))
	for _, want := range []string{"PLAIN", "HIDDEN", "••••••"} {
		if !strings.Contains(html, want) {
			t.Errorf("viewer app page lacks %q", want)
		}
	}
	html = body(t, e.getAs(t, viewer, paths[1]))
	if !strings.Contains(html, "PLAIN") {
		t.Error("viewer project page lacks the env key")
	}
	html = body(t, e.getAs(t, viewer, paths[2]))
	if !strings.Contains(html, "secretdb") {
		t.Error("viewer database page lacks the name")
	}

	// A member still gets the plain value and the webhook URL, never the secret value.
	html = body(t, e.getAs(t, member, paths[0]))
	for _, want := range []string{"plain-sentinel", "/webhooks/app/whtok-sentinel"} {
		if !strings.Contains(html, want) {
			t.Errorf("member app page lacks %q", want)
		}
	}
	if strings.Contains(html, "secret-sentinel") {
		t.Error("member app page shows a secret env value")
	}
	html = body(t, e.getAs(t, member, paths[2]))
	if !strings.Contains(html, "dbpw-sentinel") {
		t.Error("member database page lacks the connection URL")
	}
}

func TestMemberSeesNoSourceOrDestinationCredentials(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	sp := seedProject(t, e, "acme")
	if _, err := e.store.CreateDestination(ctx, core.Destination{
		Name: "offsite", Endpoint: "https://s3.example.com", Bucket: "b",
		AccessKey: "ak-sentinel", SecretKey: "sk-sentinel",
	}); err != nil {
		t.Fatal(err)
	}
	// A real key, so the page's repository lookup runs without logging an error.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if _, err := e.store.CreateGithubAppSource(ctx, core.GithubAppCreds{
		AppID: 7, Slug: "gh", PrivateKey: pemKey, WebhookSecret: "gs-sentinel",
		ClientID: "cid", ClientSecret: "cs-sentinel", InstallationID: 9,
	}); err != nil {
		t.Fatal(err)
	}
	member := e.signInAs(t, "max", core.RoleProjectMember, sp.Project.ID)
	for _, p := range []string{
		"/apps", fmt.Sprintf("/apps/%d", sp.App.ID), fmt.Sprintf("/databases/%d", sp.Database.ID),
		fmt.Sprintf("/projects/%d", sp.Project.ID),
	} {
		resp := e.getAs(t, member, p)
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s = %d", p, resp.StatusCode)
		}
		html := body(t, resp)
		for _, s := range []string{"ak-sentinel", "sk-sentinel", "PRIVATE KEY", "gs-sentinel", "cs-sentinel"} {
			if strings.Contains(html, s) {
				t.Errorf("member GET %s leaks %q", p, s)
			}
		}
	}
	// The destination name is still offered when adding a backup.
	if html := body(t, e.getAs(t, member, fmt.Sprintf("/databases/%d", sp.Database.ID))); !strings.Contains(html, "offsite") {
		t.Error("member database page does not offer the destination by name")
	}
}

func TestViewerGetsNoChangeControls(t *testing.T) {
	e := newTestEnv(t)
	sp := seedSecrets(t, e)
	db := secretDatabase(t, e, sp.Project.ID)
	viewer := e.signInAs(t, "vera", core.RoleProjectViewer, sp.Project.ID)
	member := e.signInAs(t, "max", core.RoleProjectMember, sp.Project.ID)

	pages := []string{
		fmt.Sprintf("/apps/%d", sp.App.ID),
		fmt.Sprintf("/projects/%d", sp.Project.ID),
		fmt.Sprintf("/databases/%d", db.ID),
		fmt.Sprintf("/databases/%d", sp.Database.ID),
		fmt.Sprintf("/deployments/%d", sp.Deployment.ID),
		"/apps", "/projects", "/databases", "/domains", "/",
	}
	for _, p := range pages {
		resp := e.getAs(t, viewer, p)
		if resp.StatusCode != 200 {
			t.Fatalf("viewer GET %s = %d", p, resp.StatusCode)
		}
		html := strings.ToLower(body(t, resp))
		for _, chunk := range strings.Split(html, "<form")[1:] {
			if !strings.Contains(chunk[:strings.Index(chunk, ">")], `method="post"`) {
				continue
			}
			if !strings.Contains(chunk[:strings.Index(chunk, ">")], `action="/logout"`) {
				t.Errorf("viewer GET %s has a POST form: <form%.80s", p, chunk)
			}
		}
		for _, bad := range []string{"opendomaindialog", "openvolumedialog", "openappdialog", `id="db-dialog"`, "data-env-editbtn>"} {
			if strings.Contains(html, bad) {
				t.Errorf("viewer GET %s has change control %q", p, bad)
			}
		}
	}

	html := body(t, e.getAs(t, member, fmt.Sprintf("/apps/%d", sp.App.ID)))
	if !strings.Contains(html, fmt.Sprintf(`action="/apps/%d/deploy"`, sp.App.ID)) {
		t.Error("member app page lacks the deploy form")
	}
	// A member can't create projects or delete one: that is for project admins.
	html = body(t, e.getAs(t, member, "/projects"))
	if strings.Contains(html, `action="/projects"`) {
		t.Error("member sees the new project form")
	}
	html = body(t, e.getAs(t, member, fmt.Sprintf("/projects/%d", sp.Project.ID)))
	if strings.Contains(html, fmt.Sprintf("/projects/%d/delete", sp.Project.ID)) {
		t.Error("member sees project delete")
	}
	admin := e.signInAs(t, "pat", core.RoleProjectAdmin, sp.Project.ID)
	if html := body(t, e.getAs(t, admin, "/projects")); !strings.Contains(html, `action="/projects"`) {
		t.Error("project admin lacks the new project form")
	}
	if html := body(t, e.getAs(t, admin, fmt.Sprintf("/projects/%d", sp.Project.ID))); !strings.Contains(html, fmt.Sprintf("/projects/%d/delete", sp.Project.ID)) && !strings.Contains(html, "Delete them before") {
		t.Error("project admin lacks project delete")
	}
}

func TestSidebarMatchesRole(t *testing.T) {
	e := newTestEnv(t)
	sp := seedProject(t, e, "acme")
	member := e.signInAs(t, "max", core.RoleProjectMember, sp.Project.ID)
	viewer := e.signInAs(t, "vera", core.RoleProjectViewer, sp.Project.ID)
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)

	href := func(path string) string { return `href="` + path + `"` }
	adminOnly := []string{"/settings", "/containers", "/metrics", "/volumes", "/registry"}

	html := body(t, e.getAs(t, member, "/"))
	for _, p := range append(adminOnly, "/users") {
		if strings.Contains(html, href(p)) {
			t.Errorf("member sidebar links %s", p)
		}
	}
	for _, p := range []string{"/account", "/projects", "/apps", "/databases", "/domains", "/templates"} {
		if !strings.Contains(html, href(p)) {
			t.Errorf("member sidebar lacks %s", p)
		}
	}
	html = body(t, e.getAs(t, viewer, "/"))
	if strings.Contains(html, href("/templates")) {
		t.Error("viewer sidebar links /templates")
	}
	if !strings.Contains(html, href("/projects")) {
		t.Error("viewer sidebar lacks /projects")
	}
	html = body(t, e.getAs(t, admin, "/"))
	for _, p := range append(adminOnly, "/account", "/templates", "/projects") {
		if !strings.Contains(html, href(p)) {
			t.Errorf("system admin sidebar lacks %s", p)
		}
	}
}

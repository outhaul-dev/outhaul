package catalog

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

// Every embedded template must load and render cleanly — a malformed catalog
// entry is a build defect this test turns into a red CI, not a runtime 500.
func TestCatalogLoadsAndRenders(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("embedded catalog is empty")
	}
	for _, tmpl := range all {
		r, err := Render(tmpl, "demo", "203.0.113.7")
		if err != nil {
			t.Errorf("%s: Render: %v", tmpl.ID, err)
			continue
		}
		if len(r.Domains) == 0 {
			t.Errorf("%s: rendered no domains", tmpl.ID)
		}
		for _, d := range r.Domains {
			if !strings.HasSuffix(d.Host, ".sslip.io") {
				t.Errorf("%s: generated host %q is not an sslip.io name", tmpl.ID, d.Host)
			}
			if !strings.Contains(d.Host, "203-0-113-7") {
				t.Errorf("%s: host %q does not embed the server IP", tmpl.ID, d.Host)
			}
		}
		for _, e := range r.Env {
			if strings.Contains(e.Value, "${") {
				t.Errorf("%s: env %s did not fully resolve: %q", tmpl.ID, e.Key, e.Value)
			}
			// The pipeline's .env is the only channel into compose
			// interpolation; an env key the compose file never reads is a
			// manifest typo.
			if !strings.Contains(tmpl.Compose, "${"+e.Key+"}") {
				t.Errorf("%s: env %s is not referenced by the compose file", tmpl.ID, e.Key)
			}
		}
		// And the reverse: every ${VAR} the compose file wants must be fed by
		// the manifest, or the stack starts with empty config.
		for _, ref := range regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`).FindAllStringSubmatch(tmpl.Compose, -1) {
			found := false
			for _, e := range r.Env {
				if e.Key == ref[1] {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: compose references ${%s} but the manifest sets no such env", tmpl.ID, ref[1])
			}
		}
	}
}

func TestRenderGeneratesFreshSecrets(t *testing.T) {
	tmpl, err := Get("umami")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	a, err := Render(tmpl, "one", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(tmpl, "one", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Env[0].Value == b.Env[0].Value {
		t.Errorf("two renders produced the same secret %q", a.Env[0].Value)
	}
	for _, e := range a.Env {
		if !e.Secret {
			t.Errorf("umami env %s should be marked secret", e.Key)
		}
	}
}

func TestExpandHelpers(t *testing.T) {
	if v := expand("${password:20}", nil, "app", ""); len(v) != 20 || strings.ContainsAny(v, "${}") {
		t.Errorf("password:20 = %q", v)
	}
	if v := expand("${hash:12}", nil, "app", ""); len(v) != 12 {
		t.Errorf("hash:12 = %q, want 12 hex chars", v)
	}
	b64 := expand("${base64:16}", nil, "app", "")
	if raw, err := base64.StdEncoding.DecodeString(b64); err != nil || len(raw) != 16 {
		t.Errorf("base64:16 = %q (decodes to %d bytes, err %v)", b64, len(raw), err)
	}
	uuid := expand("${uuid}", nil, "app", "")
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(uuid) {
		t.Errorf("uuid = %q, not a v4 UUID", uuid)
	}
	if v := expand("${email}", nil, "app", "203.0.113.7"); !strings.Contains(v, "@") {
		t.Errorf("email = %q", v)
	}

	// Domains embed app name and (dash-slugged) server IP.
	d := expand("${domain}", nil, "blog", "203.0.113.7")
	if !strings.HasPrefix(d, "blog-") || !strings.HasSuffix(d, "-203-0-113-7.sslip.io") {
		t.Errorf("domain = %q, want blog-<hash>-203-0-113-7.sslip.io", d)
	}
	// Without a server IP the name is still a valid editable domain.
	d = expand("${domain}", nil, "blog", "")
	if !strings.HasPrefix(d, "blog-") || !strings.HasSuffix(d, ".sslip.io") {
		t.Errorf("domain without IP = %q", d)
	}

	// Variable references resolve; unknown placeholders pass through for
	// compose-level interpolation.
	vars := map[string]string{"main_domain": "x.example.com"}
	if v := expand("https://${main_domain}", vars, "app", ""); v != "https://x.example.com" {
		t.Errorf("var reference = %q", v)
	}
	if v := expand("${NOT_A_THING}", vars, "app", ""); v != "${NOT_A_THING}" {
		t.Errorf("unknown placeholder = %q, want passthrough", v)
	}
}

func TestGetUnknownTemplate(t *testing.T) {
	if _, err := Get("no-such-template"); err == nil {
		t.Fatal("Get(no-such-template) should fail")
	}
}

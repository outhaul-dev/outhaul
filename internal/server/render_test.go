package server

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRenderTemplateErrorReturns500 reproduces the "superfluous WriteHeader"
// defect: a template that fails partway through execution must not leave the
// client with a committed 200 and half-written body. render buffers the output,
// so a mid-execution error becomes a clean 500 with no partial content.
func TestRenderTemplateErrorReturns500(t *testing.T) {
	e := newTestEnv(t)
	// Writes some output, then errors at runtime (index out of range).
	e.srv.pages["boom"] = template.Must(template.New("base").Parse(
		`{{define "base"}}partial-output{{index .Bad 5}}{{end}}`))

	rec := httptest.NewRecorder()
	e.srv.render(rec, http.StatusOK, "boom", map[string]any{"Bad": []int{1}})

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("mid-render template error should yield 500, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "partial-output") {
		t.Errorf("partial template output must not reach the client on error: %q", rec.Body.String())
	}
}

// TestRenderSuccessWritesStatusAndBody guards the happy path: a successful
// render still writes the requested status, the HTML content type, and the body.
func TestRenderSuccessWritesStatusAndBody(t *testing.T) {
	e := newTestEnv(t)
	e.srv.pages["okpage"] = template.Must(template.New("base").Parse(
		`{{define "base"}}hello {{.Who}}{{end}}`))

	rec := httptest.NewRecorder()
	e.srv.render(rec, http.StatusTeapot, "okpage", map[string]any{"Who": "world"})

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418", rec.Code)
	}
	if rec.Body.String() != "hello world" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "hello world")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
}

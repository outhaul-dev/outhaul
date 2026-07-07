package server

import (
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

func TestIsStatusTemplateFunc(t *testing.T) {
	fn, ok := templateFuncs()["isStatus"]
	if !ok {
		t.Fatal("isStatus template func not registered")
	}
	f := fn.(func(core.DeployStatus, string) bool)
	if !f(core.StatusRunning, "running") {
		t.Fatal(`running should match "running"`)
	}
	if f(core.StatusFailed, "running") {
		t.Fatal(`failed should not match "running"`)
	}
}

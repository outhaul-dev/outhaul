package compose

import (
	"reflect"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// run-once.json is the recorded output of
//
//	docker compose -p demo -f compose.yaml config --format json --no-env-resolution
//
// (Docker Compose v5.6.0) for a stack whose `api` waits on `web` in the short
// form, `batch` states nothing and nothing waits on it, `db` says
// restart: "no", `migrate` is waited on to complete, `web` waits on `migrate`
// with service_completed_successfully, and `worker` sets
// deploy.restart_policy. The source file is in the plan's Task 2.
func TestRestartExtras(t *testing.T) {
	m, err := ParseModel(readFixture(t, "run-once.json"))
	if err != nil {
		t.Fatal(err)
	}
	extras, left, err := RestartExtras(m, "unless-stopped")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Extra{
		"api":   {Restart: "unless-stopped"},
		"batch": {Restart: "unless-stopped"},
		"web":   {Restart: "unless-stopped"},
	}
	if !reflect.DeepEqual(extras, want) {
		t.Errorf("extras = %+v, want %+v", extras, want)
	}
	wantLeft := []LeftAlone{
		{Service: "db", Reason: LeftAloneOwnPolicy},
		{Service: "migrate", Reason: LeftAloneWaitedOn},
		{Service: "worker", Reason: LeftAloneOwnPolicy},
	}
	if !reflect.DeepEqual(left, wantLeft) {
		t.Errorf("left = %+v, want %+v", left, wantLeft)
	}
}

// A depends_on Compose could not have printed fails the selection rather than
// guessing which service is waited on, and the message quotes no value.
func TestRestartExtrasRefusesAnUnreadableDependsOn(t *testing.T) {
	m, err := ParseModel([]byte(`{"services": {
		"web": {"image": "nginx", "depends_on": "db"},
		"db": {"image": "postgres:16"}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = RestartExtras(m, "unless-stopped")
	if err == nil {
		t.Fatal("RestartExtras accepted a depends_on it cannot read")
	}
	if !strings.Contains(err.Error(), `service "web"`) || !strings.Contains(err.Error(), "depends_on") {
		t.Errorf("error = %q, want it to name service \"web\" and depends_on", err)
	}
	if strings.Contains(err.Error(), "db") {
		t.Errorf("error = %q, want it to quote no value from the file", err)
	}
}

func TestRestartExtrasWithoutTheSetting(t *testing.T) {
	m, err := ParseModel(readFixture(t, "run-once.json"))
	if err != nil {
		t.Fatal(err)
	}
	extras, left, err := RestartExtras(m, "")
	if extras != nil || left != nil || err != nil {
		t.Errorf("RestartExtras(m, \"\") = %+v, %+v, %v; want nil, nil, nil", extras, left, err)
	}
}

func TestMergeExtras(t *testing.T) {
	if got := MergeExtras(nil, nil); got != nil {
		t.Errorf("MergeExtras(nil, nil) = %+v, want nil", got)
	}

	limits := map[string]Extra{
		"web":    {Limits: core.Limits{MilliCPUs: 500}},
		"worker": {Limits: core.Limits{Memory: 1 << 30}, InDeploy: true},
	}
	restart := map[string]Extra{
		"api":    {Restart: "unless-stopped"},
		"web":    {Restart: "unless-stopped"},
		"worker": {Restart: "unless-stopped"},
	}
	want := map[string]Extra{
		"api":    {Restart: "unless-stopped"},
		"web":    {Limits: core.Limits{MilliCPUs: 500}, Restart: "unless-stopped"},
		"worker": {Limits: core.Limits{Memory: 1 << 30}, InDeploy: true, Restart: "unless-stopped"},
	}
	if got := MergeExtras(limits, restart); !reflect.DeepEqual(got, want) {
		t.Errorf("MergeExtras(limits, restart) = %+v, want %+v", got, want)
	}
	// The other order changes nothing: each field comes from the map that has it.
	if got := MergeExtras(restart, limits); !reflect.DeepEqual(got, want) {
		t.Errorf("MergeExtras(restart, limits) = %+v, want %+v", got, want)
	}
}

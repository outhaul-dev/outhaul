package compose

import (
	"reflect"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

var testLimit = core.Limits{MilliCPUs: 1000, Memory: 1 << 30, Pids: 512}

// modelOf parses a services object into a Model, as `docker compose config`
// would print it.
func modelOf(t *testing.T, services string) Model {
	t.Helper()
	m, err := ParseModel([]byte(`{"name":"outhaul-shop","services":` + services + `}`))
	if err != nil {
		t.Fatalf("ParseModel: %v", err)
	}
	return m
}

func TestLimitExtras(t *testing.T) {
	cases := []struct {
		name     string
		services string
		want     map[string]Extra
		// errWant lists strings the error must name; empty means no error.
		errWant []string
	}{
		{
			name:     "nothing set",
			services: `{"web":{"image":"nginx"}}`,
			want:     map[string]Extra{"web": {Limits: testLimit}},
		},
		{
			name:     "cpus set by the service",
			services: `{"web":{"image":"nginx","cpus":2}}`,
			want:     map[string]Extra{"web": {Limits: core.Limits{Memory: 1 << 30, Pids: 512}}},
		},
		{
			name:     "mem_limit set by the service",
			services: `{"web":{"image":"nginx","mem_limit":"512m"}}`,
			want:     map[string]Extra{"web": {Limits: core.Limits{MilliCPUs: 1000, Pids: 512}}},
		},
		{
			name:     "pids_limit -1 is still set",
			services: `{"web":{"image":"nginx","pids_limit":-1}}`,
			want:     map[string]Extra{"web": {Limits: core.Limits{MilliCPUs: 1000, Memory: 1 << 30}}},
		},
		{
			name:     "deploy limits block with cpus",
			services: `{"web":{"image":"nginx","deploy":{"resources":{"limits":{"cpus":2}}}}}`,
			want: map[string]Extra{"web": {
				InDeploy: true,
				Limits:   core.Limits{Memory: 1 << 30, Pids: 512},
			}},
		},
		{
			name:     "all three set by the service",
			services: `{"web":{"image":"nginx","cpus":1,"mem_limit":"1g","pids_limit":512}}`,
			want:     nil,
		},
		{
			name:     "only the service that needs one",
			services: `{"api":{"image":"nginx","cpus":1,"mem_limit":"1g","pids_limit":64},"web":{"image":"nginx"}}`,
			want:     map[string]Extra{"web": {Limits: testLimit}},
		},
		{
			name:     "mem_limit above the limit is the service's own",
			services: `{"web":{"image":"nginx","mem_limit":"2g"}}`,
			want:     map[string]Extra{"web": {Limits: core.Limits{MilliCPUs: 1000, Pids: 512}}},
		},
		{
			name:     "reservation above the limit Outhaul would add",
			services: `{"web":{"image":"nginx","mem_reservation":"2147483648"}}`,
			errWant:  []string{`service "web"`, "2147483648", "1073741824"},
		},
		{
			name:     "a service with its own memory limit may reserve more",
			services: `{"web":{"image":"nginx","mem_limit":"1073741824","mem_reservation":"2147483648"}}`,
			want:     map[string]Extra{"web": {Limits: core.Limits{MilliCPUs: 1000, Pids: 512}}},
		},
		{
			name:     "deploy reservation above the limit",
			services: `{"web":{"image":"nginx","deploy":{"resources":{"reservations":{"memory":"2147483648"}}}}}`,
			errWant:  []string{`service "web"`, "2147483648", "1073741824"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LimitExtras(modelOf(t, tc.services), testLimit)
			if len(tc.errWant) > 0 {
				if err == nil {
					t.Fatalf("LimitExtras = %+v, want an error naming %v", got, tc.errWant)
				}
				for _, want := range tc.errWant {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not name %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("LimitExtras: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("LimitExtras = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLimitExtrasZeroLimitReturnsNothing(t *testing.T) {
	got, err := LimitExtras(modelOf(t, `{"web":{"image":"nginx"}}`), core.Limits{})
	if err != nil {
		t.Fatalf("LimitExtras: %v", err)
	}
	if got != nil {
		t.Errorf("LimitExtras with no limit = %+v, want nil", got)
	}
}

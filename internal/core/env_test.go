package core

import (
	"reflect"
	"strings"
	"testing"
)

func TestResolveEnv(t *testing.T) {
	project := []EnvVar{
		{Key: "DB_URL", Value: "postgres://db:5432/app", IsSecret: false},
		{Key: "API_KEY", Value: "s3cr3t", IsSecret: true},
	}
	tests := []struct {
		name string
		in   EnvVar
		want EnvVar
	}{
		{
			name: "plain value passes through",
			in:   EnvVar{Key: "LOG_LEVEL", Value: "info"},
			want: EnvVar{Key: "LOG_LEVEL", Value: "info"},
		},
		{
			name: "reference is replaced",
			in:   EnvVar{Key: "DATABASE_URL", Value: "${{project.DB_URL}}"},
			want: EnvVar{Key: "DATABASE_URL", Value: "postgres://db:5432/app"},
		},
		{
			name: "reference embedded in a larger value",
			in:   EnvVar{Key: "DSN", Value: "${{project.DB_URL}}?sslmode=disable"},
			want: EnvVar{Key: "DSN", Value: "postgres://db:5432/app?sslmode=disable"},
		},
		{
			name: "secret project var makes the resolved var secret",
			in:   EnvVar{Key: "UPSTREAM_KEY", Value: "${{project.API_KEY}}"},
			want: EnvVar{Key: "UPSTREAM_KEY", Value: "s3cr3t", IsSecret: true},
		},
		{
			name: "app secret flag is never cleared",
			in:   EnvVar{Key: "TOKEN", Value: "${{project.DB_URL}}", IsSecret: true},
			want: EnvVar{Key: "TOKEN", Value: "postgres://db:5432/app", IsSecret: true},
		},
		{
			name: "malformed reference is left literal",
			in:   EnvVar{Key: "RAW", Value: "${{project.lower}} ${project.DB_URL} $DB_URL"},
			want: EnvVar{Key: "RAW", Value: "${{project.lower}} ${project.DB_URL} $DB_URL"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveEnv([]EnvVar{tt.in}, project)
			if err != nil {
				t.Fatalf("ResolveEnv: %v", err)
			}
			if !reflect.DeepEqual(got, []EnvVar{tt.want}) {
				t.Errorf("got %+v, want %+v", got[0], tt.want)
			}
		})
	}
}

func TestResolveEnvMultipleReferencesOneValue(t *testing.T) {
	project := []EnvVar{{Key: "HOST", Value: "db"}, {Key: "PORT_NUM", Value: "5432"}}
	got, err := ResolveEnv([]EnvVar{{Key: "ADDR", Value: "${{project.HOST}}:${{project.PORT_NUM}}"}}, project)
	if err != nil {
		t.Fatalf("ResolveEnv: %v", err)
	}
	if got[0].Value != "db:5432" {
		t.Errorf("Value = %q, want db:5432", got[0].Value)
	}
}

func TestResolveEnvUndefinedReferenceErrors(t *testing.T) {
	_, err := ResolveEnv([]EnvVar{{Key: "DATABASE_URL", Value: "${{project.MISSING}}"}}, nil)
	if err == nil {
		t.Fatal("expected an error for an undefined project variable")
	}
	if !strings.Contains(err.Error(), "MISSING") || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("error should name the missing variable and the referencing key: %v", err)
	}
}

func TestResolveEnvNotRecursive(t *testing.T) {
	// Placeholders inside project values are not expanded — one level only.
	project := []EnvVar{
		{Key: "A", Value: "${{project.B}}"},
		{Key: "B", Value: "deep"},
	}
	got, err := ResolveEnv([]EnvVar{{Key: "X", Value: "${{project.A}}"}}, project)
	if err != nil {
		t.Fatalf("ResolveEnv: %v", err)
	}
	if got[0].Value != "${{project.B}}" {
		t.Errorf("Value = %q, want the literal placeholder (no recursion)", got[0].Value)
	}
}

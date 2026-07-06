package github

import (
	"context"
	"testing"
)

func TestFakeUpsertPRComment(t *testing.T) {
	f := &Fake{}
	if err := f.UpsertPRComment(context.Background(), "tok", "me/app", 42, "hello"); err != nil {
		t.Fatal(err)
	}
	if got := f.LastComment("me/app", 42); got != "hello" {
		t.Fatalf("comment = %q", got)
	}
	if err := f.UpsertPRComment(context.Background(), "tok", "me/app", 42, "updated"); err != nil {
		t.Fatal(err)
	}
	if got := f.LastComment("me/app", 42); got != "updated" {
		t.Fatalf("comment not updated: %q", got)
	}
}

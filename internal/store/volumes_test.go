package store

import (
	"context"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

func TestDeriveVolumeName(t *testing.T) {
	cases := map[string]string{
		"/data":         "outhaul-web-data",
		"/var/lib/data": "outhaul-web-var-lib-data",
		"/app/Uploads/": "outhaul-web-app-uploads",
		"/":             "outhaul-web-", // degenerate: an empty slug leaves a trailing dash
	}
	for path, want := range cases {
		if got := deriveVolumeName("web", path); got != want {
			t.Errorf("deriveVolumeName(web, %q) = %q, want %q", path, got, want)
		}
	}
}

func TestAddListGetUpdateDeleteVolume(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	app := testApp(t, st, "web", core.KindNixpacks, "")

	v, err := st.AddVolume(ctx, app.ID, "/data")
	if err != nil {
		t.Fatalf("AddVolume: %v", err)
	}
	if v.ID == 0 || v.Name != "outhaul-web-data" || v.MountPath != "/data" {
		t.Fatalf("AddVolume returned %+v", v)
	}

	list, err := st.ListVolumes(ctx, app.ID)
	if err != nil || len(list) != 1 || list[0].Name != "outhaul-web-data" {
		t.Fatalf("ListVolumes = %+v, err %v", list, err)
	}

	if err := st.UpdateVolume(ctx, app.ID, v.ID, "/data2"); err != nil {
		t.Fatalf("UpdateVolume: %v", err)
	}
	got, err := st.GetVolume(ctx, app.ID, v.ID)
	if err != nil || got.MountPath != "/data2" || got.Name != "outhaul-web-data" {
		t.Fatalf("after update: %+v, err %v", got, err)
	}

	if err := st.DeleteVolume(ctx, app.ID, v.ID); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if list, _ := st.ListVolumes(ctx, app.ID); len(list) != 0 {
		t.Fatalf("expected no volumes after delete, got %+v", list)
	}
}

func TestAddVolumeRejectsDuplicateMountPath(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	app := testApp(t, st, "web", core.KindNixpacks, "")
	if _, err := st.AddVolume(ctx, app.ID, "/data"); err != nil {
		t.Fatalf("first AddVolume: %v", err)
	}
	if _, err := st.AddVolume(ctx, app.ID, "/data"); err == nil {
		t.Fatal("expected a UNIQUE(app_id, mount_path) violation on duplicate path")
	}
}

func TestListAllVolumesTagsAppIdentity(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	web := testApp(t, st, "web", core.KindNixpacks, "")
	if _, err := st.AddVolume(ctx, web.ID, "/data"); err != nil {
		t.Fatalf("AddVolume: %v", err)
	}
	all, err := st.ListAllVolumes(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("ListAllVolumes = %+v, err %v", all, err)
	}
	if all[0].AppName != "web" || all[0].AppKind != core.KindNixpacks {
		t.Errorf("listing not tagged with app identity: %+v", all[0])
	}
}

func TestDeleteAppClearsVolumes(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	app := testApp(t, st, "web", core.KindNixpacks, "")
	if _, err := st.AddVolume(ctx, app.ID, "/data"); err != nil {
		t.Fatalf("AddVolume: %v", err)
	}
	if err := st.DeleteApp(ctx, app.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	all, _ := st.ListAllVolumes(ctx)
	if len(all) != 0 {
		t.Fatalf("volumes not cleared on app delete: %+v", all)
	}
}

package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/migrations"
	"github.com/greenbeanorg/skep/pkg/models"
)

func TestTemplateStore(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db, migrations.Agent()); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO nodes (id, name, is_local) VALUES ('n1', 'node1', 1)`)
	s := NewTemplateStore(db)
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	insert := func(tpl models.Template) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := s.InsertTx(ctx, tx, "n1", tpl); err != nil {
			return err
		}
		return tx.Commit()
	}
	a := models.Template{ID: "3f2a9c1e-7b4d-4e8f-9a01-23456789abcd", Name: "minimal-15.1", Pool: "skeptest",
		Source: models.TemplateSourcePkgbase, Packages: []string{"FreeBSD-set-minimal"}, CreatedAt: at}
	b := models.Template{ID: "4f2a9c1e-7b4d-4e8f-9a01-23456789abcd", Name: "dist-15.1", Pool: "skeptest",
		Source: models.TemplateSourceDist, CreatedAt: at}
	for _, tpl := range []models.Template{a, b} {
		if err := insert(tpl); err != nil {
			t.Fatal(err)
		}
	}
	dup := a
	dup.ID = "5f2a9c1e-7b4d-4e8f-9a01-23456789abcd"
	var me *models.Error
	if err := insert(dup); !errors.As(err, &me) || me.Code != models.CodeTemplateNameTaken {
		t.Fatalf("a repeated name: %v", err)
	}

	got, err := s.Get(ctx, a.ID)
	if err != nil || got.Name != a.Name || got.Pool != "skeptest" || got.Provisioned || len(got.Packages) != 1 || got.Release != "" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if got, _ := s.Get(ctx, b.ID); got.ArchiveSHA256 != "" {
		t.Fatalf("the placeholder checksum is shown: %q", got.ArchiveSHA256)
	}
	if _, err := s.Get(ctx, "nope"); !errors.Is(err, models.ErrNotFound) {
		t.Fatalf("Get missing = %v", err)
	}

	page, next, err := s.List(ctx, 1, "")
	if err != nil || len(page) != 1 || page[0].Name != "dist-15.1" || next == "" {
		t.Fatalf("page 1 = %v, %q, %v", page, next, err)
	}
	page, next, err = s.List(ctx, 1, next)
	if err != nil || len(page) != 1 || page[0].Name != "minimal-15.1" || next != "" {
		t.Fatalf("page 2 = %v, %q, %v", page, next, err)
	}
	if _, _, err := s.List(ctx, 1, "!!"); err == nil {
		t.Fatal("a bad cursor was accepted")
	}

	if err := s.SetProvisioned(ctx, a.ID, "15.1-RELEASE-p6", "", 132<<20, at); err != nil {
		t.Fatal(err)
	}
	sum := "ab" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"
	if err := s.SetProvisioned(ctx, b.ID, "15.1-RELEASE-p6", sum, 1, at); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, a.ID); !got.Provisioned || got.Release != "15.1-RELEASE-p6" || got.UsedBytes != 132<<20 {
		t.Fatalf("provisioned = %+v", got)
	}
	if got, _ := s.Get(ctx, b.ID); got.ArchiveSHA256 != sum {
		t.Fatalf("dist checksum = %q", got.ArchiveSHA256)
	}
	if err := s.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, a.ID); !errors.Is(err, models.ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM storage_pools WHERE name = 'skeptest' AND type = 'zfs'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("pool rows = %d, %v", n, err)
	}
}

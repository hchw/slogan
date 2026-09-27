package db

import (
	"context"
	"testing"
	"testing/fstest"
)

func TestMigrationChecksumDriftIsRejected(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	first := fstest.MapFS{"0001.sql": &fstest.MapFile{Data: []byte("CREATE TABLE checksum_probe (id integer);")}}
	if _, err := Migrate(ctx, pool, first); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	changed := fstest.MapFS{"0001.sql": &fstest.MapFile{Data: []byte("CREATE TABLE checksum_probe (id bigint);")}}
	if _, err := Migrate(ctx, pool, changed); err == nil {
		t.Fatal("expected checksum drift to be rejected")
	}
}

package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/storage/sqlite"
	"github.com/frankbardon/aperture/storage/storagetest"
)

func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) model.Storage {
		// A per-subtest temp-file database gives each case full isolation while
		// still exercising real on-disk SQLite I/O.
		dsn := "file:" + filepath.Join(t.TempDir(), "aperture.db")
		s, err := sqlite.Open(dsn)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if err := s.Setup(t.Context()); err != nil {
			t.Fatalf("setup: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestOpenMemory(t *testing.T) {
	s, err := sqlite.OpenMemory()
	if err != nil {
		t.Fatalf("open memory: %v", err)
	}
	defer s.Close()
	if err := s.Setup(t.Context()); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := s.PutObjectType(t.Context(), model.ObjectType{Name: "document", Actions: []string{"read"}}); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.GetObjectType(t.Context(), "document")
	if err != nil || !got.HasAction("read") {
		t.Fatalf("get = %+v, err %v", got, err)
	}
}

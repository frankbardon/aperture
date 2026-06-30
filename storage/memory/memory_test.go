package memory_test

import (
	"testing"

	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/storage/memory"
	"github.com/frankbardon/aperture/storage/storagetest"
)

func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) model.Storage {
		s := memory.New()
		if err := s.Setup(t.Context()); err != nil {
			t.Fatalf("setup: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

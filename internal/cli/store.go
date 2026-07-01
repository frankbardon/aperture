package cli

import (
	"context"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/storage/memory"
	"github.com/frankbardon/aperture/storage/sqlite"
)

// buildStore constructs and initialises a model.Storage for a command, then
// seeds it. This is the manual constructor-DI seam the check and serve commands
// share: choose a backend from --store, run Setup, and load a model from --seed
// (or the embedded example when --seed is empty).
//
//   - storeDSN == ""  -> in-memory backend (storage/memory), ideal for the demo.
//   - storeDSN != ""  -> SQLite backend at the DSN (storage/sqlite).
//   - seedPath == ""  -> load the embedded example fixture (account "acme").
//   - seedPath != ""  -> load the file, format inferred from its extension.
//
// On any failure the caller gets an APERTURE_BOOT error and the partially
// constructed store is closed.
func buildStore(ctx context.Context, storeDSN, seedPath string) (model.Storage, error) {
	store, err := openStore(storeDSN)
	if err != nil {
		return nil, err
	}
	if err := store.Setup(ctx); err != nil {
		_ = store.Close()
		return nil, aerr.Wrap(aerr.APERTURE_BOOT, "cli: storage setup failed", err)
	}
	if err := loadSeed(ctx, store, seedPath); err != nil {
		_ = store.Close()
		return nil, aerr.Wrap(aerr.APERTURE_BOOT, "cli: seeding the model failed", err)
	}
	return store, nil
}

// openStore selects the storage backend from the DSN.
func openStore(storeDSN string) (model.Storage, error) {
	if storeDSN == "" {
		return memory.New(), nil
	}
	store, err := sqlite.Open(storeDSN)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_BOOT, "cli: open sqlite store", err)
	}
	return store, nil
}

// loadSeed loads the model from the seed file, or the embedded example when no
// path is given.
func loadSeed(ctx context.Context, store model.Storage, seedPath string) error {
	if seedPath == "" {
		return seed.Load(ctx, store, seed.Example, seed.FormatYAML)
	}
	return seed.LoadFile(ctx, store, seedPath)
}

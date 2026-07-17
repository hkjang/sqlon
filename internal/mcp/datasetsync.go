package mcp

import (
	"context"
	"os"
	"path/filepath"

	"sqlon/internal/catalog"
)

// Dataset persistence in auth mode
//
// When a meta DB is active, Postgres (jamypg_datasets) is the source of truth
// for the editable JSON datasets. To reuse the entire existing file-based
// loader, validation, hot-swap, and rollback machinery unchanged, the meta DB
// rows are *materialized to files* in the data dir before every catalog
// compile. The data dir therefore acts as a cache; the DB is authoritative.
//
//	startup: importDatasetsToDB (seed DB from any files not yet stored)
//	         → materializeDatasets (dump DB rows to files)
//	         → catalog.Load(dataDir)
//	put/remove (meta): write DB → materialize that file → reload
//
// feedback/audit/backups stay file-only (runtime logs, not catalog inputs).

// datasetsInDB reports whether dataset persistence is delegated to the meta DB.
func (s *Server) datasetsInDB() bool { return s.Meta != nil }

// importDatasetsToDB seeds the meta DB from on-disk files for any editable
// dataset not yet present in the DB (first-run migration). Existing DB rows
// are left untouched so the DB stays authoritative across restarts.
func (s *Server) importDatasetsToDB(ctx context.Context) error {
	dataDir := s.cat().DataDir
	for _, d := range catalog.FileBackedDatasets() {
		if _, err := s.Meta.Store.GetDataset(ctx, d.Name); err == nil {
			continue // already in DB
		}
		b, err := os.ReadFile(filepath.Join(dataDir, d.File))
		if err != nil {
			continue // file absent — nothing to import for this dataset
		}
		if err := s.Meta.Store.PutDataset(ctx, d.Name, b, "import"); err != nil {
			return err
		}
	}
	return nil
}

// materializeDatasets writes every DB-stored dataset to its file in the data
// dir so the standard loader can compile from disk.
func (s *Server) materializeDatasets(ctx context.Context) error {
	dataDir := s.cat().DataDir
	rows, err := s.Meta.Store.ListDatasets(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		file, ok := catalog.DatasetFileByName(row.Name)
		if !ok {
			continue
		}
		if err := os.WriteFile(filepath.Join(dataDir, file), row.Content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// persistDatasetsToDB pushes the current on-disk content of the named dataset
// files back into the meta DB (the source of truth in auth mode). Direct
// file-writing apply paths (metadata-sync apply, review apply, OpenMetadata
// import, golden promote) MUST call this before any reload: reload
// re-materializes files FROM the DB, so an unpersisted file write would be
// silently reverted. No-op in standalone mode. Accepts dataset names or file
// names (registry-resolved).
func (s *Server) persistDatasetsToDB(namesOrFiles ...string) error {
	if !s.datasetsInDB() {
		return nil
	}
	dataDir := s.cat().DataDir
	for _, n := range namesOrFiles {
		var name, file string
		for _, d := range catalog.FileBackedDatasets() {
			if d.Name == n || d.File == n {
				name, file = d.Name, d.File
				break
			}
		}
		if file == "" {
			continue // not a registry dataset (e.g. workspace-only file)
		}
		b, err := os.ReadFile(filepath.Join(dataDir, file))
		if err != nil {
			continue // file absent — nothing to persist
		}
		if err := s.Meta.Store.PutDataset(context.Background(), name, b, "apply"); err != nil {
			return err
		}
	}
	return nil
}

// syncAndReload materializes DB datasets to disk and recompiles the catalog,
// hot-swapping on success. Used at startup and after DB-routed changes.
func (s *Server) syncAndReload(ctx context.Context) error {
	if err := s.materializeDatasets(ctx); err != nil {
		return err
	}
	cat, err := catalog.Load(s.cat().DataDir)
	if err != nil {
		return err
	}
	s.setCatalog(cat)
	return nil
}

// InitDatasetStore performs first-run import + materialize + reload so the
// running catalog reflects the DB. Called by main after EnableMeta.
func (s *Server) InitDatasetStore(ctx context.Context) error {
	if !s.datasetsInDB() {
		return nil
	}
	if err := s.importDatasetsToDB(ctx); err != nil {
		return err
	}
	return s.syncAndReload(ctx)
}

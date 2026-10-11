//go:build realdata

package dbwrite

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chaosben/jwlSyncFolderMerger/internal/jwlfile"
	"github.com/chaosben/jwlSyncFolderMerger/internal/merge"
	"github.com/chaosben/jwlSyncFolderMerger/internal/model"
)

// TestRoundTripRealData loads every .jwlibrary file in $JWLSFM_REALDATA,
// writes it back through dbwrite and checks that nothing changed.
// Run with: JWLSFM_REALDATA=/path go test -tags realdata ./internal/dbwrite
func TestRoundTripRealData(t *testing.T) {
	dir := os.Getenv("JWLSFM_REALDATA")
	if dir == "" {
		t.Skip("JWLSFM_REALDATA not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.jwlibrary"))
	if len(files) == 0 {
		t.Fatalf("no .jwlibrary files in %s", dir)
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			work := t.TempDir()
			f, err := jwlfile.Open(path, work)
			if err != nil {
				t.Fatal(err)
			}
			conn, _ := jwlfile.OpenDB(f.DBPath)
			orig, err := model.Load(conn)
			conn.Close()
			if err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(work, "out.db")
			if _, err := Write(f.DBPath, out, orig, Options{Marker: "test-gen", LastModified: time.Now()}); err != nil {
				t.Fatal(err)
			}
			conn, _ = jwlfile.OpenDB(out)
			back, err := model.Load(conn)
			conn.Close()
			if err != nil {
				t.Fatal(err)
			}
			if back.Marker != "test-gen" {
				t.Errorf("marker = %q", back.Marker)
			}
			if !merge.SameContent(orig, back) {
				t.Error("content changed in round trip")
			}
			if len(back.Locations) != len(orig.Locations) {
				t.Errorf("locations %d -> %d", len(orig.Locations), len(back.Locations))
			}
			t.Logf("schema %d marks %d notes %d tagmaps %d warnings %d", orig.SchemaVersion, len(orig.UserMarks), len(orig.Notes), len(orig.TagMaps), len(orig.Warnings))
		})
	}
}

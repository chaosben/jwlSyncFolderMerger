// Package jwlfile reads and writes .jwlibrary archives: a zip file holding
// manifest.json, the SQLite user database and media files.
package jwlfile

import (
	"archive/zip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chaosben/jwlSyncFolderMerger/internal/jwltime"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

const (
	ManifestName   = "manifest.json"
	DefaultDBName  = "userData.db"
	ThumbnailName  = "default_thumbnail.png"
	maxEntrySize   = 1 << 30 // guards against zip bombs
	maxArchiveSize = 4 << 30
)

// Manifest mirrors manifest.json.
type Manifest struct {
	Name           string         `json:"name"`
	CreationDate   string         `json:"creationDate"`
	Version        int            `json:"version"`
	Type           int            `json:"type"`
	UserDataBackup UserDataBackup `json:"userDataBackup"`
}

type UserDataBackup struct {
	LastModifiedDate string `json:"lastModifiedDate"`
	DeviceName       string `json:"deviceName"`
	DatabaseName     string `json:"databaseName"`
	Hash             string `json:"hash"`
	SchemaVersion    int    `json:"schemaVersion"`
}

// File is an extracted .jwlibrary archive.
type File struct {
	Path          string // the archive on disk
	Dir           string // extraction directory
	Manifest      Manifest
	DBPath        string
	DBHash        string            // SHA-256 of the checkpointed database (manifest hashes are unreliable)
	Modified      time.Time         // parsed userDataBackup.lastModifiedDate
	SchemaVersion int               // PRAGMA user_version
	Files         map[string]string // other entries (media, thumbnail): name -> path on disk
}

// Device returns the device name from the manifest.
func (f *File) Device() string { return f.Manifest.UserDataBackup.DeviceName }

// OpenDB opens a SQLite database file.
func OpenDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// Open extracts the archive at path into a new directory below workDir and
// folds a pending WAL into the database file.
func Open(path, workDir string) (*File, error) {
	dir, err := os.MkdirTemp(workDir, "jwl-")
	if err != nil {
		return nil, err
	}
	f := &File{Path: path, Dir: dir, Files: map[string]string{}}
	if err := f.extract(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if err := f.prepareDB(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return f, nil
}

func (f *File) extract() error {
	zr, err := zip.OpenReader(f.Path)
	if err != nil {
		return err
	}
	defer zr.Close()

	var total int64
	extracted := map[string]string{}
	for _, e := range zr.File {
		name := e.Name
		if e.FileInfo().IsDir() {
			continue
		}
		if name == "" || strings.ContainsAny(name, `/\:`) || name == "." || name == ".." {
			return fmt.Errorf("unexpected entry %q in archive", name)
		}
		if e.UncompressedSize64 > maxEntrySize {
			return fmt.Errorf("entry %q too large", name)
		}
		total += int64(e.UncompressedSize64)
		if total > maxArchiveSize {
			return errors.New("archive too large")
		}
		dst := filepath.Join(f.Dir, name)
		if err := extractEntry(e, dst); err != nil {
			return err
		}
		extracted[name] = dst
	}

	raw, err := os.ReadFile(extracted[ManifestName])
	if err != nil {
		return errors.New("manifest.json missing")
	}
	if err := json.Unmarshal(raw, &f.Manifest); err != nil {
		return fmt.Errorf("manifest.json: %w", err)
	}
	dbName := f.Manifest.UserDataBackup.DatabaseName
	if dbName == "" {
		dbName = DefaultDBName
	}
	f.DBPath = extracted[dbName]
	if f.DBPath == "" {
		return fmt.Errorf("database %q missing", dbName)
	}
	if f.Modified, err = jwltime.Parse(f.Manifest.UserDataBackup.LastModifiedDate); err != nil {
		return fmt.Errorf("manifest lastModifiedDate: %w", err)
	}
	for name, p := range extracted {
		switch name {
		case ManifestName, dbName, dbName + "-wal", dbName + "-shm", dbName + "-journal":
		default:
			f.Files[name] = p
		}
	}
	return nil
}

func extractEntry(e *zip.File, dst string) error {
	rc, err := e.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(rc, maxEntrySize+1)); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// prepareDB checkpoints the WAL, switches to rollback journal and hashes the file.
func (f *File) prepareDB() error {
	db, err := OpenDB(f.DBPath)
	if err != nil {
		return err
	}
	for _, stmt := range []string{"PRAGMA wal_checkpoint(TRUNCATE)", "PRAGMA journal_mode=DELETE"} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&f.SchemaVersion); err != nil {
		db.Close()
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	f.DBHash, err = HashFile(f.DBPath)
	return err
}

// Close removes the extraction directory.
func (f *File) Close() error { return os.RemoveAll(f.Dir) }

// HashFile returns the lowercase hex SHA-256 of a file.
func HashFile(path string) (string, error) {
	in, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer in.Close()
	h := sha256.New()
	if _, err := io.Copy(h, in); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Write creates a .jwlibrary archive at dest. files maps archive entry names
// to source paths (media files and the thumbnail).
func Write(dest string, m Manifest, dbPath string, files map[string]string) error {
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(out)
	now := time.Now()
	add := func(name, src string) error {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: now})
		if err != nil {
			return err
		}
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(w, in)
		return err
	}
	err = func() error {
		names := make([]string, 0, len(files))
		for n := range files {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			if err := add(name, files[name]); err != nil {
				return fmt.Errorf("add %s: %w", name, err)
			}
		}
		if err := add(m.UserDataBackup.DatabaseName, dbPath); err != nil {
			return err
		}
		raw, err := json.Marshal(m)
		if err != nil {
			return err
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: ManifestName, Method: zip.Deflate, Modified: now})
		if err != nil {
			return err
		}
		_, err = w.Write(raw)
		return err
	}()
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dest)
	}
	return err
}

package backup

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/server/filesystem"
)

func TestBackupGenerateRequiresUuidIdentifier(t *testing.T) {
	tests := map[string]func(string) BackupInterface{
		"base": func(identifier string) BackupInterface {
			return &Backup{Uuid: identifier}
		},
		"local": func(identifier string) BackupInterface {
			return NewLocal(nil, identifier, "")
		},
		"s3": func(identifier string) BackupInterface {
			return NewS3(nil, identifier, "")
		},
	}

	for name, createBackup := range tests {
		t.Run(name, func(t *testing.T) {
			testBackupGenerateRequiresUuidIdentifier(t, createBackup)
		})
	}
}

func TestLocalBackupZipRoundTrip(t *testing.T) {
	config.Set(&config.Configuration{
		AuthenticationToken: "test-token",
		System:              config.SystemConfiguration{BackupDirectory: t.TempDir()},
	})
	root := t.TempDir()
	want := "CatWings ZIP backup contents"
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	fsys, err := filesystem.New(root, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.UnixFS().Close()
	b := NewLocal(nil, "ABCDEFAB-1111-1111-1111-111111111111", "")
	if _, err := b.Generate(context.Background(), fsys, ""); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(b.Path()) != "abcdefab-1111-1111-1111-111111111111.zip" {
		t.Fatalf("unexpected ZIP backup path: %s", b.Path())
	}
	files := 0
	if err := b.Restore(context.Background(), nil, func(name string, _ fs.FileInfo, r io.ReadCloser) error {
		got, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		if name != "file.txt" || string(got) != want {
			t.Fatalf("unexpected restored file: %q = %q", name, got)
		}
		files++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if files != 1 {
		t.Fatalf("expected one restored file, got %d", files)
	}
}

func TestBackupPathUsesBackupDirectory(t *testing.T) {
	backupDir := t.TempDir()
	config.Set(&config.Configuration{
		AuthenticationToken: "test-token",
		System: config.SystemConfiguration{
			BackupDirectory: backupDir,
		},
	})

	for _, identifier := range []string{
		"11111111-1111-1111-1111-111111111111",
		"../target/archive",
		"nested/archive",
	} {
		b := NewLocal(nil, identifier, "")
		rel, err := filepath.Rel(backupDir, b.Path())
		if err != nil {
			t.Fatal(err)
		}
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			t.Fatalf("expected backup path %q to remain under %q", b.Path(), backupDir)
		}
	}
}

func testBackupGenerateRequiresUuidIdentifier(t *testing.T, createBackup func(string) BackupInterface) {
	t.Helper()

	root := t.TempDir()
	backupDir := filepath.Join(root, "backups")
	targetDir := filepath.Join(root, "target")
	serverDir := filepath.Join(root, "server")
	for _, dir := range []string{backupDir, targetDir, serverDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config.Set(&config.Configuration{
		AuthenticationToken: "test-token",
		System: config.SystemConfiguration{
			BackupDirectory: backupDir,
		},
	})

	if err := os.WriteFile(filepath.Join(serverDir, "file.txt"), []byte("server data"), 0o600); err != nil {
		t.Fatal(err)
	}
	fsys, err := filesystem.New(serverDir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	existingArchive := filepath.Join(targetDir, "archive.zip")
	existingArchiveContents := []byte("existing archive")
	if err := os.WriteFile(existingArchive, existingArchiveContents, 0o600); err != nil {
		t.Fatal(err)
	}

	b := createBackup("../target/archive")
	if _, err := b.Generate(context.Background(), fsys, ""); err == nil {
		t.Fatal("expected invalid backup identifier to be rejected")
	}

	got, err := os.ReadFile(existingArchive)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got, existingArchiveContents) {
		return
	}
	t.Fatal("expected backup generation not to overwrite existing archive")
}

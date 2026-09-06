package transfer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/progress"
	"github.com/pterodactyl/wings/server"
	"github.com/pterodactyl/wings/server/filesystem"
)

func TestPushArchiveReportsDestinationError(t *testing.T) {
	cfg := &config.Configuration{AuthenticationToken: "transfer-test"}
	cfg.System.User.Uid = os.Getuid()
	cfg.System.User.Gid = os.Getgid()
	config.Set(cfg)
	fs, err := filesystem.New(t.TempDir(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.UnixFS().Close()
	if err := os.WriteFile(filepath.Join(fs.Path(), "probe.txt"), []byte("transfer contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := server.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	transfer := New(ctx, s)
	transfer.archive = &Archive{archive: &filesystem.Archive{Filesystem: fs, Progress: progress.NewProgress(17)}}
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"checksums don't match","request_id":"transfer-test-request"}`)
	}))
	defer destination.Close()
	_, err = transfer.PushArchiveToTarget(destination.URL, "Bearer test-token")
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "checksums don't match") || !strings.Contains(err.Error(), "transfer-test-request") {
		t.Fatalf("destination diagnostic was lost: %v", err)
	}
}

package router

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	"github.com/gin-gonic/gin"
	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/server"
)

// The current CatWings Panel issues user-bound tokens without a scope claim.
func TestCatWingsBackupDownloadWithoutScope(t *testing.T) {
	backupDir := t.TempDir()
	config.Set(&config.Configuration{
		AuthenticationToken: "test-token",
		System:              config.SystemConfiguration{BackupDirectory: backupDir},
	})
	const backupID = "11111111-1111-1111-1111-111111111111"
	const contents = "CatWings backup download"
	if err := os.WriteFile(filepath.Join(backupDir, backupID+".zip"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	client := backupTestRemoteClient{}
	manager := server.NewEmptyManager(client)
	s, err := server.New(client)
	if err != nil {
		t.Fatal(err)
	}
	s.Config().Uuid = "server"
	manager.Add(s)
	token, err := jwt.Sign(map[string]interface{}{
		"iat":         time.Now().Add(time.Second).Unix(),
		"exp":         time.Now().Add(time.Minute).Unix(),
		"server_uuid": "server", "user_uuid": "user",
		"backup_uuid": backupID, "unique_id": t.Name(),
	}, config.GetJwtAlgorithm())
	if err != nil {
		t.Fatal(err)
	}
	download := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Set("manager", manager)
		c.Set("api_client", client)
		c.Request = httptest.NewRequest(http.MethodGet, "/download/backup?token="+string(token), nil)
		getDownloadBackup(c)
		return recorder
	}
	response := download()
	if response.Code != http.StatusOK || response.Body.String() != contents {
		t.Fatalf("legacy Panel backup download failed: %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Header().Get("Content-Disposition"), backupID+".zip") {
		t.Fatal("download did not retain the ZIP filename")
	}
	if replay := download(); replay.Code != http.StatusNotFound {
		t.Fatalf("expected replayed token to be rejected, got %d", replay.Code)
	}
}

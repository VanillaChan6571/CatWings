package router

import (
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/server"
)

func TestPostUpdateConfigurationRotatesCredentials(t *testing.T) {
	t.Setenv("WINGS_TOKEN_ID", "")
	t.Setenv("WINGS_TOKEN", "")

	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthenticationTokenId = "old-id"
	cfg.AuthenticationToken = "old-token"
	cfg.System.RootDirectory = "/local/root"
	cfg.System.LogDirectory = "/local/logs"
	cfg.System.Data = "/local/data"
	cfg.System.ArchiveDirectory = "/local/archives"
	cfg.System.BackupDirectory = "/local/backups"
	cfg.System.TmpDirectory = "/local/tmp"
	cfg.System.Passwd.Directory = "/local/passwd"
	cfg.System.MachineID.Directory = "/local/machine-id"
	if err := cfg.ResolveToken(false); err != nil {
		t.Fatal(err)
	}
	config.Set(cfg)

	credentials := make(chan [2]string, 1)
	manager := server.NewEmptyManager(backupTestRemoteClient{credentials: credentials})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("manager", manager)
	c.Request = httptest.NewRequest("POST", "/api/update", strings.NewReader(`{
		"token_id":"new-id","token":"new-token",
		"system": {
			"root_directory":"/remote/root", "log_directory":"/remote/logs",
			"data":"/remote/data", "archive_directory":"/remote/archives",
			"backup_directory":"/remote/backups", "tmp_directory":"/remote/tmp",
			"passwd":{"directory":"/remote/passwd"},
			"machine_id":{"directory":"/remote/machine-id"}
		}
	}`))
	c.Request.Header.Set("Content-Type", "application/json")

	postUpdateConfiguration(c)

	if recorder.Code != 200 {
		t.Fatalf("expected successful update, got status %d", recorder.Code)
	}
	updated := config.Get()
	if updated.Token.ID != "new-id" || updated.Token.Token != "new-token" {
		t.Fatalf("unexpected resolved credentials: %#v", updated.Token)
	}
	paths := func(c *config.Configuration) []string {
		return []string{c.System.RootDirectory, c.System.LogDirectory, c.System.Data,
			c.System.ArchiveDirectory, c.System.BackupDirectory, c.System.TmpDirectory,
			c.System.Passwd.Directory, c.System.MachineID.Directory}
	}
	if !reflect.DeepEqual(paths(updated), paths(cfg)) {
		t.Fatalf("credential rotation changed local paths: %v", paths(updated))
	}
	if cfg.Token.ID != "old-id" || cfg.Token.Token != "old-token" {
		t.Fatal("update mutated the previous configuration")
	}
	select {
	case got := <-credentials:
		if got != [2]string{"new-id", "new-token"} {
			t.Fatalf("unexpected client credentials: %#v", got)
		}
	default:
		t.Fatal("expected client credentials to be rotated")
	}
}

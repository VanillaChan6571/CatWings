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

// The Panel only sends a handful of top level keys. Anything it leaves out has to
// survive the update: a constrained payload built from value types silently reset
// docker, throttles and most of system to their zero values on every save.
func TestPostUpdateConfigurationPreservesUnsentFields(t *testing.T) {
	t.Setenv("WINGS_TOKEN_ID", "")
	t.Setenv("WINGS_TOKEN", "")

	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.AppName = "Pterodactyl"
	cfg.AuthenticationTokenId = "old-id"
	cfg.AuthenticationToken = "old-token"
	cfg.Api.UploadLimit = 100
	cfg.Docker.Network.Name = "pterodactyl_nw"
	cfg.Docker.Network.Interface = "172.18.0.1"
	cfg.Docker.Network.Driver = "bridge"
	cfg.Docker.ContainerPidLimit = 512
	cfg.Throttles.Enabled = true
	cfg.Throttles.Lines = 2000
	cfg.RemoteQuery.Timeout = 30
	cfg.System.Username = "pterodactyl"
	cfg.System.Timezone = "America/Denver"
	cfg.System.User.Uid = 999
	cfg.System.User.Gid = 987
	cfg.System.Sftp.Address = "0.0.0.0"
	cfg.System.Sftp.Port = 2022
	cfg.System.Backups.CompressionLevel = "best_speed"
	cfg.AllowedOrigins = []string{"https://nekohosting.gg"}
	if err := cfg.ResolveToken(false); err != nil {
		t.Fatal(err)
	}
	config.Set(cfg)

	manager := server.NewEmptyManager(backupTestRemoteClient{credentials: make(chan [2]string, 1)})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("manager", manager)
	// This is the exact shape Panel's Node::getConfiguration() emits.
	c.Request = httptest.NewRequest("POST", "/api/update", strings.NewReader(`{
		"debug": false,
		"uuid": "93bc69b1-747a-43fb-99b0-235c9650bdb8",
		"token_id": "new-id",
		"token": "new-token",
		"api": {
			"host": "0.0.0.0", "port": 8080,
			"ssl": {"enabled": true, "cert": "/etc/letsencrypt/live/x/fullchain.pem", "key": "/etc/letsencrypt/live/x/privkey.pem"},
			"upload_limit": 980
		},
		"system": {"data": "/var/lib/pterodactyl/volumes", "sftp": {"bind_port": 2022}},
		"allowed_mounts": [],
		"remote": "https://panel.example.com"
	}`))
	c.Request.Header.Set("Content-Type", "application/json")

	postUpdateConfiguration(c)

	if recorder.Code != 200 {
		t.Fatalf("expected successful update, got status %d", recorder.Code)
	}
	updated := config.Get()

	// What the Panel did send must be applied.
	if updated.Api.UploadLimit != 980 {
		t.Errorf("upload_limit not applied: got %d, want 980", updated.Api.UploadLimit)
	}
	if updated.PanelLocation != "https://panel.example.com" {
		t.Errorf("remote not applied: got %q", updated.PanelLocation)
	}

	// Everything it left out must survive untouched.
	for _, tc := range []struct {
		field string
		got   any
		want  any
	}{
		{"app_name", updated.AppName, "Pterodactyl"},
		{"docker.network.name", updated.Docker.Network.Name, "pterodactyl_nw"},
		{"docker.network.interface", updated.Docker.Network.Interface, "172.18.0.1"},
		{"docker.network.driver", updated.Docker.Network.Driver, "bridge"},
		{"docker.container_pid_limit", updated.Docker.ContainerPidLimit, int64(512)},
		{"throttles.enabled", updated.Throttles.Enabled, true},
		{"throttles.lines", updated.Throttles.Lines, uint64(2000)},
		{"remote_query.timeout", updated.RemoteQuery.Timeout, 30},
		{"system.username", updated.System.Username, "pterodactyl"},
		{"system.timezone", updated.System.Timezone, "America/Denver"},
		{"system.user.uid", updated.System.User.Uid, 999},
		{"system.user.gid", updated.System.User.Gid, 987},
		{"system.backups.compression_level", updated.System.Backups.CompressionLevel, "best_speed"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s was clobbered: got %v, want %v", tc.field, tc.got, tc.want)
		}
	}

	// A partially sent block must merge, not replace: bind_port arrives, bind_address does not.
	if updated.System.Sftp.Port != 2022 {
		t.Errorf("sftp bind_port not applied: got %d", updated.System.Sftp.Port)
	}
	if updated.System.Sftp.Address != "0.0.0.0" {
		t.Errorf("sftp bind_address was clobbered by a partial block: got %q", updated.System.Sftp.Address)
	}

	if len(updated.AllowedOrigins) != 1 || updated.AllowedOrigins[0] != "https://nekohosting.gg" {
		t.Errorf("allowed_origins was clobbered: got %v", updated.AllowedOrigins)
	}
}

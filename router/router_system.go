package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/apex/log"
	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/router/middleware"
	"github.com/pterodactyl/wings/router/tokens"
	"github.com/pterodactyl/wings/server"
	"github.com/pterodactyl/wings/server/installer"
	"github.com/pterodactyl/wings/system"
)

// Returns information about the system that wings is running on.
func getSystemInformation(c *gin.Context) {
	i, err := system.GetSystemInformation()
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	if c.Query("v") == "2" {
		c.JSON(http.StatusOK, i)
		return
	}

	c.JSON(http.StatusOK, struct {
		Architecture  string `json:"architecture"`
		CPUCount      int    `json:"cpu_count"`
		KernelVersion string `json:"kernel_version"`
		OS            string `json:"os"`
		Version       string `json:"version"`
	}{
		Architecture:  i.System.Architecture,
		CPUCount:      i.System.CPUThreads,
		KernelVersion: i.System.KernelVersion,
		OS:            i.System.OSType,
		Version:       i.Version,
	})
}

// Returns all the servers that are registered and configured correctly on
// this wings instance.
func getAllServers(c *gin.Context) {
	servers := middleware.ExtractManager(c).All()
	out := make([]server.APIResponse, len(servers), len(servers))
	for i, v := range servers {
		out[i] = v.ToAPIResponse()
	}
	c.JSON(http.StatusOK, out)
}

// Creates a new server on the wings daemon and begins the installation process
// for it.
func postCreateServer(c *gin.Context) {
	manager := middleware.ExtractManager(c)

	details := installer.ServerDetails{}
	if err := c.BindJSON(&details); err != nil {
		return
	}

	install, err := installer.New(c.Request.Context(), manager, details)
	if err != nil {
		if installer.IsValidationError(err) {
			c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{
				"error": "The data provided in the request could not be validated.",
			})
			return
		}

		middleware.CaptureAndAbort(c, err)
		return
	}

	// Plop that server instance onto the request so that it can be referenced in
	// requests from here-on out.
	manager.Add(install.Server())

	// Begin the installation process in the background to not block the request
	// cycle. If there are any errors they will be logged and communicated back
	// to the Panel where a reinstall may take place.
	go func(i *installer.Installer) {
		if err := i.Server().CreateEnvironment(); err != nil {
			i.Server().Log().WithField("error", err).Error("failed to create server environment during install process")
			return
		}

		if err := i.Server().Install(); err != nil {
			log.WithFields(log.Fields{"server": i.Server().ID(), "error": err}).Error("failed to run install process for server")
			return
		}

		if i.StartOnCompletion {
			log.WithField("server_id", i.Server().ID()).Debug("starting server after successful installation")
			if err := i.Server().HandlePowerAction(server.PowerActionStart, 30); err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					log.WithFields(log.Fields{"server_id": i.Server().ID(), "action": "start"}).Warn("could not acquire a lock while attempting to perform a power action")
				} else {
					log.WithFields(log.Fields{"server_id": i.Server().ID(), "action": "start", "error": err}).Error("encountered error processing a server power action in the background")
				}
			}
		} else {
			log.WithField("server_id", i.Server().ID()).Debug("skipping automatic start after successful server installation")
		}
	}(install)

	c.Status(http.StatusAccepted)
}

type postUpdateConfigurationResponse struct {
	Applied bool `json:"applied"`
}

// panelUpdateConfigurationPayload is a constrained request payload for /api/update.
// It intentionally excludes local filesystem path fields.
//
// Every field is a pointer, a slice, or a json.RawMessage so a key the Panel omits
// stays nil and is left alone. Value types made "absent" and "sent as zero"
// indistinguishable: the Panel only sends eight top level keys, so every update
// blanked docker, throttles, remote_query and most of system.
type panelUpdateConfigurationPayload struct {
	Debug                 *bool                           `json:"debug"`
	AppName               *string                         `json:"app_name"`
	Uuid                  *string                         `json:"uuid"`
	AuthenticationTokenId *string                         `json:"token_id"`
	AuthenticationToken   *string                         `json:"token"`
	Api                   json.RawMessage                 `json:"api"`
	Docker                json.RawMessage                 `json:"docker"`
	Throttles             json.RawMessage                 `json:"throttles"`
	Remote                *string                         `json:"remote"`
	RemoteQuery           json.RawMessage                 `json:"remote_query"`
	AllowedMounts         []string                        `json:"allowed_mounts"`
	AllowedOrigins        []string                        `json:"allowed_origins"`
	AllowCORSPrivateNet   *bool                           `json:"allow_cors_private_network"`
	IgnorePanelUpdates    *bool                           `json:"ignore_panel_config_updates"`
	System                *panelUpdateSystemConfiguration `json:"system"`
}

type panelUpdateSystemConfiguration struct {
	Username               *string                     `json:"username"`
	Timezone               *string                     `json:"timezone"`
	User                   *panelUpdateSystemUser      `json:"user"`
	Passwd                 *panelUpdateSystemPasswd    `json:"passwd"`
	MachineID              *panelUpdateSystemMachineID `json:"machine_id"`
	DiskCheckInterval      *int64                      `json:"disk_check_interval"`
	ActivitySendInterval   *int                        `json:"activity_send_interval"`
	ActivitySendCount      *int                        `json:"activity_send_count"`
	CheckPermissionsOnBoot *bool                       `json:"check_permissions_on_boot"`
	EnableLogRotate        *bool                       `json:"enable_log_rotate"`
	WebsocketLogCount      *int                        `json:"websocket_log_count"`
	Sftp                   json.RawMessage             `json:"sftp"`
	CrashDetection         json.RawMessage             `json:"crash_detection"`
	Backups                json.RawMessage             `json:"backups"`
	Transfers              json.RawMessage             `json:"transfers"`
	OpenatMode             *string                     `json:"openat_mode"`
}

type panelUpdateSystemRootless struct {
	Enabled      *bool `json:"enabled"`
	ContainerUID *int  `json:"container_uid"`
	ContainerGID *int  `json:"container_gid"`
}

type panelUpdateSystemUser struct {
	Rootless *panelUpdateSystemRootless `json:"rootless"`
	Uid      *int                       `json:"uid"`
	Gid      *int                       `json:"gid"`
}

type panelUpdateSystemPasswd struct {
	Enable *bool `json:"enabled"`
}

type panelUpdateSystemMachineID struct {
	Enable *bool `json:"enabled"`
}

// mergePanelConfigurationBlock decodes a nested configuration block onto the value
// it already holds, so fields the Panel did not mention survive the update.
func mergePanelConfigurationBlock(raw json.RawMessage, target any, name string) error {
	if raw == nil {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return errors.New("config: invalid " + name + " block in panel configuration update: " + err.Error())
	}
	return nil
}

// applyPanelUpdateConfigurationPayload copies only the values the Panel actually
// sent onto cfg. Anything absent from the request is left as it was.
func applyPanelUpdateConfigurationPayload(cfg *config.Configuration, payload *panelUpdateConfigurationPayload) error {
	if payload.Debug != nil {
		cfg.Debug = *payload.Debug
	}
	if payload.AppName != nil {
		cfg.AppName = *payload.AppName
	}
	if payload.Uuid != nil {
		cfg.Uuid = *payload.Uuid
	}
	if payload.AuthenticationTokenId != nil {
		cfg.AuthenticationTokenId = *payload.AuthenticationTokenId
	}
	if payload.AuthenticationToken != nil {
		cfg.AuthenticationToken = *payload.AuthenticationToken
	}
	if payload.Remote != nil {
		cfg.PanelLocation = *payload.Remote
	}
	if payload.AllowedMounts != nil {
		cfg.AllowedMounts = payload.AllowedMounts
	}
	if payload.AllowedOrigins != nil {
		cfg.AllowedOrigins = payload.AllowedOrigins
	}
	if payload.AllowCORSPrivateNet != nil {
		cfg.AllowCORSPrivateNetwork = *payload.AllowCORSPrivateNet
	}
	if payload.IgnorePanelUpdates != nil {
		cfg.IgnorePanelConfigUpdates = *payload.IgnorePanelUpdates
	}

	if err := mergePanelConfigurationBlock(payload.Api, &cfg.Api, "api"); err != nil {
		return err
	}
	if err := mergePanelConfigurationBlock(payload.Docker, &cfg.Docker, "docker"); err != nil {
		return err
	}
	if err := mergePanelConfigurationBlock(payload.Throttles, &cfg.Throttles, "throttles"); err != nil {
		return err
	}
	if err := mergePanelConfigurationBlock(payload.RemoteQuery, &cfg.RemoteQuery, "remote_query"); err != nil {
		return err
	}

	if payload.System == nil {
		return nil
	}
	system := payload.System

	if system.Username != nil {
		cfg.System.Username = *system.Username
	}
	if system.Timezone != nil {
		cfg.System.Timezone = *system.Timezone
	}
	if system.DiskCheckInterval != nil {
		cfg.System.DiskCheckInterval = *system.DiskCheckInterval
	}
	if system.ActivitySendInterval != nil {
		cfg.System.ActivitySendInterval = *system.ActivitySendInterval
	}
	if system.ActivitySendCount != nil {
		cfg.System.ActivitySendCount = *system.ActivitySendCount
	}
	if system.CheckPermissionsOnBoot != nil {
		cfg.System.CheckPermissionsOnBoot = *system.CheckPermissionsOnBoot
	}
	if system.EnableLogRotate != nil {
		cfg.System.EnableLogRotate = *system.EnableLogRotate
	}
	if system.WebsocketLogCount != nil {
		cfg.System.WebsocketLogCount = *system.WebsocketLogCount
	}
	if system.OpenatMode != nil {
		cfg.System.OpenatMode = *system.OpenatMode
	}
	if system.Passwd != nil && system.Passwd.Enable != nil {
		cfg.System.Passwd.Enable = *system.Passwd.Enable
	}
	if system.MachineID != nil && system.MachineID.Enable != nil {
		cfg.System.MachineID.Enable = *system.MachineID.Enable
	}
	if system.User != nil {
		if system.User.Uid != nil {
			cfg.System.User.Uid = *system.User.Uid
		}
		if system.User.Gid != nil {
			cfg.System.User.Gid = *system.User.Gid
		}
		if system.User.Rootless != nil {
			if system.User.Rootless.Enabled != nil {
				cfg.System.User.Rootless.Enabled = *system.User.Rootless.Enabled
			}
			if system.User.Rootless.ContainerUID != nil {
				cfg.System.User.Rootless.ContainerUID = *system.User.Rootless.ContainerUID
			}
			if system.User.Rootless.ContainerGID != nil {
				cfg.System.User.Rootless.ContainerGID = *system.User.Rootless.ContainerGID
			}
		}
	}

	if err := mergePanelConfigurationBlock(system.Sftp, &cfg.System.Sftp, "system.sftp"); err != nil {
		return err
	}
	if err := mergePanelConfigurationBlock(system.CrashDetection, &cfg.System.CrashDetection, "system.crash_detection"); err != nil {
		return err
	}
	if err := mergePanelConfigurationBlock(system.Backups, &cfg.System.Backups, "system.backups"); err != nil {
		return err
	}
	if err := mergePanelConfigurationBlock(system.Transfers, &cfg.System.Transfers, "system.transfers"); err != nil {
		return err
	}

	return nil
}

// Updates the running configuration for this Wings instance.
func postUpdateConfiguration(c *gin.Context) {
	current := config.Get()
	cfg := *current
	payload := panelUpdateConfigurationPayload{}

	if cfg.IgnorePanelConfigUpdates {
		c.JSON(http.StatusOK, postUpdateConfigurationResponse{
			Applied: false,
		})
		return
	}

	if err := c.BindJSON(&payload); err != nil {
		return
	}

	if err := applyPanelUpdateConfigurationPayload(&cfg, &payload); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	// Preserve local filesystem paths from existing daemon config.
	cfg.System.RootDirectory = current.System.RootDirectory
	cfg.System.LogDirectory = current.System.LogDirectory
	cfg.System.Data = current.System.Data
	cfg.System.ArchiveDirectory = current.System.ArchiveDirectory
	cfg.System.BackupDirectory = current.System.BackupDirectory
	cfg.System.TmpDirectory = current.System.TmpDirectory
	cfg.System.Passwd.Directory = current.System.Passwd.Directory
	cfg.System.MachineID.Directory = current.System.MachineID.Directory

	// Keep the SSL certificates the same since the Panel will send through Lets Encrypt
	// default locations. However, if we picked a different location manually we don't
	// want to override that.
	//
	// If you pass through manual locations in the API call this logic will be skipped.
	if strings.HasPrefix(cfg.Api.Ssl.KeyFile, "/etc/letsencrypt/live/") {
		cfg.Api.Ssl.KeyFile = current.Api.Ssl.KeyFile
		cfg.Api.Ssl.CertificateFile = current.Api.Ssl.CertificateFile
	}

	// The token that everything authenticates against is a derived value that is
	// not part of the payload sent by the Panel, so it has to be re-resolved from
	// the new token values.
	if err := cfg.ResolveToken(true); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	// Refuse to go any further with a token we could never authenticate against.
	if cfg.Token.ID == "" || cfg.Token.Token == "" {
		middleware.CaptureAndAbort(c, errors.New("config: refusing to apply an update with an empty authentication token"))
		return
	}

	tokenId, token := cfg.Token.ID, cfg.Token.Token

	// Try to write this new configuration to the disk before updating our global
	// state with it.
	if err := config.WriteToDisk(&cfg); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	// Since we wrote it to the disk successfully now update the global configuration
	// state to use this new configuration struct.
	config.Set(&cfg)

	// Requests we make back to the Panel use credentials that were captured when
	// the client was created at boot, so they have to be rotated explicitly.
	middleware.ExtractManager(c).Client().SetCredentials(tokenId, token)

	c.JSON(http.StatusOK, postUpdateConfigurationResponse{
		Applied: true,
	})
}

func postDeauthorizeUser(c *gin.Context) {
	var data struct {
		User    string   `json:"user"`
		Servers []string `json:"servers"`
	}

	if err := c.BindJSON(&data); err != nil {
		return
	}

	// todo: disconnect websockets more gracefully
	m := middleware.ExtractManager(c)
	if len(data.Servers) > 0 {
		for _, uuid := range data.Servers {
			if s, ok := m.Get(uuid); ok {
				tokens.DenyForServer(s.ID(), data.User)
				s.Websockets().CancelAll()
				s.Sftp().Cancel(data.User)
			}
		}
	} else {
		for _, s := range m.All() {
			tokens.DenyForServer(s.ID(), data.User)
			s.Websockets().CancelAll()
			s.Sftp().Cancel(data.User)
		}
	}

	c.Status(http.StatusNoContent)
}

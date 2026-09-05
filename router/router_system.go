package router

import (
	"context"
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
type panelUpdateConfigurationPayload struct {
	Debug                 bool                            `json:"debug"`
	AppName               string                          `json:"app_name"`
	Uuid                  string                          `json:"uuid"`
	AuthenticationTokenId string                          `json:"token_id"`
	AuthenticationToken   string                          `json:"token"`
	Api                   config.ApiConfiguration         `json:"api"`
	Docker                config.DockerConfiguration      `json:"docker"`
	Throttles             config.ConsoleThrottles         `json:"throttles"`
	Remote                string                          `json:"remote"`
	RemoteQuery           config.RemoteQueryConfiguration `json:"remote_query"`
	AllowedMounts         []string                        `json:"allowed_mounts"`
	AllowedOrigins        []string                        `json:"allowed_origins"`
	AllowCORSPrivateNet   bool                            `json:"allow_cors_private_network"`
	IgnorePanelUpdates    bool                            `json:"ignore_panel_config_updates"`
	System                panelUpdateSystemConfiguration  `json:"system"`
}

type panelUpdateSystemConfiguration struct {
	Username               string                     `json:"username"`
	Timezone               string                     `json:"timezone"`
	User                   panelUpdateSystemUser      `json:"user"`
	Passwd                 panelUpdateSystemPasswd    `json:"passwd"`
	MachineID              panelUpdateSystemMachineID `json:"machine_id"`
	DiskCheckInterval      int64                      `json:"disk_check_interval"`
	ActivitySendInterval   int                        `json:"activity_send_interval"`
	ActivitySendCount      int                        `json:"activity_send_count"`
	CheckPermissionsOnBoot bool                       `json:"check_permissions_on_boot"`
	EnableLogRotate        bool                       `json:"enable_log_rotate"`
	WebsocketLogCount      int                        `json:"websocket_log_count"`
	Sftp                   config.SftpConfiguration   `json:"sftp"`
	CrashDetection         config.CrashDetection      `json:"crash_detection"`
	Backups                config.Backups             `json:"backups"`
	Transfers              config.Transfers           `json:"transfers"`
	OpenatMode             string                     `json:"openat_mode"`
}

type panelUpdateSystemUser struct {
	Rootless struct {
		Enabled      bool `json:"enabled"`
		ContainerUID int  `json:"container_uid"`
		ContainerGID int  `json:"container_gid"`
	} `json:"rootless"`
	Uid int `json:"uid"`
	Gid int `json:"gid"`
}

type panelUpdateSystemPasswd struct {
	Enable bool `json:"enabled"`
}

type panelUpdateSystemMachineID struct {
	Enable bool `json:"enabled"`
}

func applyPanelUpdateConfigurationPayload(cfg *config.Configuration, payload *panelUpdateConfigurationPayload) {
	cfg.Debug = payload.Debug
	cfg.AppName = payload.AppName
	cfg.Uuid = payload.Uuid
	cfg.AuthenticationTokenId = payload.AuthenticationTokenId
	cfg.AuthenticationToken = payload.AuthenticationToken
	cfg.Api = payload.Api
	cfg.Docker = payload.Docker
	cfg.Throttles = payload.Throttles
	cfg.PanelLocation = payload.Remote
	cfg.RemoteQuery = payload.RemoteQuery
	cfg.AllowedMounts = payload.AllowedMounts
	cfg.AllowedOrigins = payload.AllowedOrigins
	cfg.AllowCORSPrivateNetwork = payload.AllowCORSPrivateNet
	cfg.IgnorePanelConfigUpdates = payload.IgnorePanelUpdates

	cfg.System.Username = payload.System.Username
	cfg.System.Timezone = payload.System.Timezone
	cfg.System.User.Rootless.Enabled = payload.System.User.Rootless.Enabled
	cfg.System.User.Rootless.ContainerUID = payload.System.User.Rootless.ContainerUID
	cfg.System.User.Rootless.ContainerGID = payload.System.User.Rootless.ContainerGID
	cfg.System.User.Uid = payload.System.User.Uid
	cfg.System.User.Gid = payload.System.User.Gid
	cfg.System.Passwd.Enable = payload.System.Passwd.Enable
	cfg.System.MachineID.Enable = payload.System.MachineID.Enable
	cfg.System.DiskCheckInterval = payload.System.DiskCheckInterval
	cfg.System.ActivitySendInterval = payload.System.ActivitySendInterval
	cfg.System.ActivitySendCount = payload.System.ActivitySendCount
	cfg.System.CheckPermissionsOnBoot = payload.System.CheckPermissionsOnBoot
	cfg.System.EnableLogRotate = payload.System.EnableLogRotate
	cfg.System.WebsocketLogCount = payload.System.WebsocketLogCount
	cfg.System.Sftp = payload.System.Sftp
	cfg.System.CrashDetection = payload.System.CrashDetection
	cfg.System.Backups = payload.System.Backups
	cfg.System.Transfers = payload.System.Transfers
	cfg.System.OpenatMode = payload.System.OpenatMode
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

	applyPanelUpdateConfigurationPayload(&cfg, &payload)

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

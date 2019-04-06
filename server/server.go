package server

import (
	"github.com/remeh/sizedwaitgroup"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"os"
	"path"
	"strings"
)

// High level definition for a server instance being controlled by Wings.
type Server struct {
	// The unique identifier for the server that should be used when referencing
	// it aganist the Panel API (and internally). This will be used when naming
	// docker containers as well as in log output.
	Uuid string

	// Wether or not the server is in a suspended state. Suspended servers cannot
	// be started or modified except in certain scenarios by an admin user.
	Suspended bool

	// The power state of the server.
	State int

	// The command that should be used when booting up the server instance.
	Invocation string

	// An array of environment variables that should be passed along to the running
	// server process.
	EnvVars map[string]string `yaml:"env"`

	Build       *BuildSettings
	Allocations *Allocations

	Container struct {
		// Defines the Docker image that will be used for this server
		Image string
	}

	environment Environment

	fs *Filesystem
}

// The build settings for a given server that impact docker container creation and
// resource limits for a server instance.
type BuildSettings struct {
	// The total amount of memory in megabytes that this server is allowed to
	// use on the host system.
	MemoryLimit int64 `yaml:"memory"`

	// The amount of additional swap space to be provided to a container instance.
	Swap int64

	// The relative weight for IO operations in a container. This is relative to other
	// containers on the system and should be a value between 10 and 1000.
	IoWeight uint16 `yaml:"io"`

	// The percentage of CPU that this instance is allowed to consume relative to
	// the host. A value of 200% represents complete utilization of two cores. This
	// should be a value between 1 and THREAD_COUNT * 100.
	CpuLimit int64 `yaml:"cpu"`

	// The amount of disk space in megabytes that a server is allowed to use.
	DiskSpace int64 `yaml:"disk"`
}

// Converts the CPU limit for a server build into a number that can be better understood
// by the Docker environment. If there is no limit set, return -1 which will indicate to
// Docker that it has unlimited CPU quota.
func (b *BuildSettings) ConvertedCpuLimit() int64 {
	if b.CpuLimit == 0 {
		return -1
	}

	return b.CpuLimit * 1000
}

// Returns the amount of swap available as a total in bytes. This is returned as the amount
// of memory available to the server initially, PLUS the amount of additional swap to include
// which is the format used by Docker.
func (b *BuildSettings) ConvertedSwap() int64 {
	if b.Swap < 0 {
		return -1
	}

	return (b.Swap * 1000000) + (b.MemoryLimit * 1000000)
}

// Defines the allocations available for a given server. When using the Docker environment
// driver these correspond to mappings for the container that allow external connections.
type Allocations struct {
	// Defines the default allocation that should be used for this server. This is
	// what will be used for {SERVER_IP} and {SERVER_PORT} when modifying configuration
	// files or the startup arguments for a server.
	DefaultMapping struct {
		Ip   string
		Port int
	} `yaml:"default"`

	// Mappings contains all of the ports that should be assigned to a given server
	// attached to the IP they correspond to.
	Mappings map[string][]int
}

// Iterates over a given directory and loads all of the servers listed before returning
// them to the calling function.
func LoadDirectory(dir string, cfg DockerConfiguration) ([]*Server, error) {
	// We could theoretically use a standard wait group here, however doing
	// that introduces the potential to crash the program due to too many
	// open files. This wouldn't happen on a small setup, but once the daemon is
	// handling many servers you run that risk.
	//
	// For now just process 10 files at a time, that should be plenty fast to
	// read and parse the YAML. We should probably make this configurable down
	// the road to help big instances scale better.
	wg := sizedwaitgroup.New(10)

	f, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var servers []*Server

	for _, file := range f {
		if !strings.HasSuffix(file.Name(), ".yml") || file.IsDir() {
			continue
		}

		wg.Add()
		// For each of the YAML files we find, parse it and create a new server
		// configuration object that can then be returned to the caller.
		go func(file os.FileInfo) {
			defer wg.Done()

			b, err := ioutil.ReadFile(path.Join(dir, file.Name()))
			if err != nil {
				zap.S().Errorw("failed to read server configuration file, skipping...", zap.Error(err))
				return
			}

			s, err := FromConfiguration(b, cfg)
			if err != nil {
				zap.S().Errorw("failed to parse server configuration, skipping...", zap.Error(err))
				return
			}

			servers = append(servers, s)
		}(file)
	}

	// Wait until we've processed all of the configuration files in the directory
	// before continuing.
	wg.Wait()

	return servers, nil
}

// Initalizes a server using a data byte array. This will be marshaled into the
// given struct using a YAML marshaler. This will also configure the given environment
// for a server.
func FromConfiguration(data []byte, cfg DockerConfiguration) (*Server, error) {
	s := &Server{}

	if err := yaml.Unmarshal(data, s); err != nil {
		return nil, err
	}

	// Right now we only support a Docker based environment, so I'm going to hard code
	// this logic in. When we're ready to support other environment we'll need to make
	// some modifications here obviously.
	var env Environment
	if t, err := NewDockerEnvironment(cfg); err != nil {
		t.Server = s
		env = t
	} else {
		return nil, err
	}

	s.environment = env

	s.fs = &Filesystem{
		// @todo adjust this to be configuration provided!
		Root: "/srv/daemon-data",
		Server: s,
	}

	return s, nil
}

func (s *Server) Filesystem() *Filesystem {
	return s.fs
}

// Determine if the server is bootable in it's current state or not. This will not
// indicate why a server is not bootable, only if it is.
func (s *Server) IsBootable() bool {
	return s.environment.Exists()
}

// Initalizes a server instance. This will run through and ensure that the environment
// for the server is setup, and that all of the necessary files are created.
func (s *Server) CreateEnvironment() error {
	return s.environment.Create()
}
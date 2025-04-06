package backup

import (
	"context"
	"io"
	"os"
	"strings"

	"emperror.dev/errors"
	"github.com/juju/ratelimit"
	"github.com/mholt/archives"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/server/filesystem"
)

// BackupCompletingEvent is published when a backup is being finalized
const BackupCompletingEvent = "backup completing"

// Backup struct update
type Backup struct {
	// The UUID of this backup object. This must line up with a backup from
	// the panel instance.
	Uuid string `json:"uuid"`

	// An array of files to ignore when generating this backup. This should be
	// compatible with a standard .gitignore structure.
	Ignore string `json:"ignore"`

	client     remote.Client
	adapter    AdapterType
	logContext map[string]interface{}
	format     ArchiveFormat // Add format field
}

// DefaultFormat is initially set based on config, can be changed later
func init() {
	// Set default format based on config, default to ZIP if not specified
	format := config.Get().System.ArchiveFormat
	switch strings.ToLower(format) {
	case "zip":
		DefaultFormat = FormatZip
	case "tar.gz":
		DefaultFormat = FormatTarGz
	default:
		DefaultFormat = FormatZip
	}
}

// Path returns the path for this specific backup.
func (b *Backup) Path() string {
	// Use the format to determine the correct extension
	if b.format == "" {
		b.format = DefaultFormat
	}
	return b.format.GetPathWithExtension(config.Get().System.BackupDirectory, b.Identifier())
}

// NewS3 update to include format
func NewS3(client remote.Client, uuid string, ignore string) *S3Backup {
	return &S3Backup{
		Backup: Backup{
			client:  client,
			Uuid:    uuid,
			Ignore:  ignore,
			adapter: S3BackupAdapter,
			format:  DefaultFormat, // Use default format
		},
	}
}

// NewLocal update to include format
func NewLocal(client remote.Client, uuid string, ignore string) *LocalBackup {
	return &LocalBackup{
		Backup: Backup{
			client:  client,
			Uuid:    uuid,
			Ignore:  ignore,
			adapter: LocalBackupAdapter,
			format:  DefaultFormat, // Use default format
		},
	}
}

// LocateLocal updated to handle multiple formats
func LocateLocal(client remote.Client, uuid string) (*LocalBackup, os.FileInfo, error) {
	// Try with the default format first
	b := NewLocal(client, uuid, "")
	st, err := os.Stat(b.Path())

	// If not found with default format, try the alternate format
	if err != nil && os.IsNotExist(err) {
		alternateFormat := FormatTarGz
		if DefaultFormat == FormatTarGz {
			alternateFormat = FormatZip
		}

		b.format = alternateFormat
		st, err = os.Stat(b.Path())
		if err != nil {
			return nil, nil, err
		}
	} else if err != nil {
		return nil, nil, err
	}

	if st.IsDir() {
		return nil, nil, errors.New("invalid archive, is directory")
	}

	return b, st, nil
}

// Restore method update to handle different formats
func (b *LocalBackup) Restore(ctx context.Context, _ io.Reader, callback RestoreCallback) error {
	f, err := os.Open(b.Path())
	if err != nil {
		return err
	}
	defer f.Close()

	var reader io.Reader = f
	// Apply rate limiting if configured
	if writeLimit := int64(config.Get().System.Backups.WriteLimit * 1024 * 1024); writeLimit > 0 {
		reader = ratelimit.Reader(f, ratelimit.NewBucketWithRate(float64(writeLimit), writeLimit))
	}

	// Determine the format from the file path
	format := GetFormatFromPath(b.Path()).GetFormat()
	extraction, ok := format.(archives.Extraction)
	if !ok {
		return errors.New("format does not support extraction")
	}

	// Extract the archive
	if err := extraction.Extract(ctx, reader, func(ctx context.Context, f archives.FileInfo) error {
		r, err := f.Open()
		if err != nil {
			return err
		}
		defer r.Close()

		return callback(f.NameInArchive, f.FileInfo, r)
	}); err != nil {
		return err
	}
	return nil
}

// S3Backup.Restore method needs similar updates to handle both formats

// Update S3Backup.Generate to support ZIP format
func (s *S3Backup) Generate(ctx context.Context, fsys *filesystem.Filesystem, ignore string) (*ArchiveDetails, error) {
	defer s.Remove()

	a := &filesystem.Archive{
		Filesystem: fsys,
		Ignore:     ignore,
		Format:     s.format, // Use the format from the backup
	}

	s.log().WithField("path", s.Path()).Info("creating backup for server")

	// Start progress monitoring for ZIP files
	if s.format == FormatZip {
		monitor := NewZipProgressMonitor(s.Path(), s.Identifier(), fsys.EventBus())
		defer monitor.Stop()
		monitor.Start(ctx)
	}

	if err := a.Create(ctx, s.Path()); err != nil {
		return nil, err
	}
	s.log().Info("created backup successfully")

	rc, err := os.Open(s.Path())
	if err != nil {
		return nil, errors.Wrap(err, "backup: could not read archive from disk")
	}
	defer rc.Close()

	parts, err := s.generateRemoteRequest(ctx, rc)
	if err != nil {
		return nil, err
	}
	ad, err := s.Details(ctx, parts)
	if err != nil {
		return nil, errors.WrapIf(err, "backup: failed to get archive details after upload")
	}
	return ad, nil
}

// Similar updates needed for LocalBackup.Generate
func (b *LocalBackup) Generate(ctx context.Context, fsys *filesystem.Filesystem, ignore string) (*ArchiveDetails, error) {
	a := &filesystem.Archive{
		Filesystem: fsys,
		Ignore:     ignore,
		Format:     b.format, // Use the format from the backup
	}

	b.log().WithField("path", b.Path()).Info("creating backup for server")

	// Start progress monitoring for ZIP files
	if b.format == FormatZip {
		monitor := NewZipProgressMonitor(b.Path(), b.Identifier(), fsys.EventBus())
		defer monitor.Stop()
		monitor.Start(ctx)
	}

	if err := a.Create(ctx, b.Path()); err != nil {
		return nil, err
	}
	b.log().Info("created backup successfully")

	ad, err := b.Details(ctx, nil)
	if err != nil {
		return nil, errors.WrapIf(err, "backup: failed to get archive details for local backup")
	}
	return ad, nil
}

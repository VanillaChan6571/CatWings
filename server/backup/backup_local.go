package backup

import (
	"context"
	"io"
	"os"

	"emperror.dev/errors"
	"github.com/juju/ratelimit"
	"github.com/mholt/archives"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/server/filesystem"
)

type LocalBackup struct {
	Backup
}

var _ BackupInterface = (*LocalBackup)(nil)

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

// LocateLocal finds the backup for a server and returns the local path. This
// will obviously only work if the backup was created as a local backup.
// LocateLocal update to detect format from file
func LocateLocal(client remote.Client, uuid string) (*LocalBackup, os.FileInfo, error) {
	// Try with the default format first
	b := NewLocal(client, uuid, "")
	st, err := os.Stat(b.Path())

	// If not found, try the alternate format
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

// Remove removes a backup from the system.
func (b *LocalBackup) Remove() error {
	return os.Remove(b.Path())
}

// WithLogContext attaches additional context to the log output for this backup.
func (b *LocalBackup) WithLogContext(c map[string]interface{}) {
	b.logContext = c
}

// Generate generates a backup of the selected files and pushes it to the
// defined location for this instance.
func (b *LocalBackup) Generate(ctx context.Context, fsys *filesystem.Filesystem, ignore string) (*ArchiveDetails, error) {
	a := &filesystem.Archive{
		Filesystem: fsys,
		Ignore:     ignore,
	}

	b.log().WithField("path", b.Path()).Info("creating backup for server")
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

// Restore will walk over the archive and call the callback function for each
// file encountered.
// Restore method update to handle different formats
func (b *LocalBackup) Restore(ctx context.Context, _ io.Reader, callback RestoreCallback) error {
	f, err := os.Open(b.Path())
	if err != nil {
		return err
	}
	defer f.Close()

	var reader io.Reader = f
	// Steal the logic we use for making backups which will be applied when restoring
	// this specific backup. This allows us to prevent overloading the disk unintentionally.
	if writeLimit := int64(config.Get().System.Backups.WriteLimit * 1024 * 1024); writeLimit > 0 {
		reader = ratelimit.Reader(f, ratelimit.NewBucketWithRate(float64(writeLimit), writeLimit))
	}

	// Use the format from the backup path
	format := GetFormatFromPath(b.Path()).GetFormat()
	if err := format.(archives.Extraction).Extract(ctx, reader, func(ctx context.Context, f archives.FileInfo) error {
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

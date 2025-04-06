package filesystem

import (
	"archive/tar"
	"compress/flate"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/juju/ratelimit"
	"github.com/klauspost/pgzip"
	ignore "github.com/sabhiram/go-gitignore"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/events"
	"github.com/pterodactyl/wings/internal/progress"
	"github.com/pterodactyl/wings/internal/ufs"
	"github.com/pterodactyl/wings/server/backup"
)

const memory = 4 * 1024

var pool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, memory)
		return b
	},
}

// TarProgress .
type TarProgress struct {
	*tar.Writer
	p *progress.Progress
}

// NewTarProgress .
func NewTarProgress(w *tar.Writer, p *progress.Progress) *TarProgress {
	if p != nil {
		p.Writer = w
	}
	return &TarProgress{
		Writer: w,
		p:      p,
	}
}

// Write .
func (p *TarProgress) Write(v []byte) (int, error) {
	if p.p == nil {
		return p.Writer.Write(v)
	}
	return p.p.Write(v)
}

// Add format and events fields to Archive struct
type Archive struct {
	// Filesystem to create the archive with.
	Filesystem *Filesystem

	// Ignore is a gitignore string (most likely read from a file) of files to ignore
	// from the archive.
	Ignore string

	// BaseDirectory .
	BaseDirectory string

	// Files specifies the files to archive, this takes priority over the Ignore
	// option, if unspecified, all files in the BaseDirectory will be archived
	// unless Ignore is set.
	Files []string

	// Progress wraps the writer of the archive to pass through the progress tracker.
	Progress *progress.Progress

	// Format specifies the archive format to use
	Format backup.ArchiveFormat

	// EventBus for sending progress notifications
	events *events.Bus

	w interface{} // Either *TarProgress or *zip.Writer
}

// EventBus returns the event bus used by this filesystem
func (fs *Filesystem) EventBus() *events.Bus {
	return fs.eventBus
}

// Create creates an archive at dst with all the files defined in the
// included Files array.
//
// THIS IS UNSAFE TO USE IF `dst` IS PROVIDED BY A USER! ONLY USE THIS WITH
// CONTROLLED PATHS!
func (a *Archive) Create(ctx context.Context, dst string) error {
	// If format is not set, use the default
	if a.Format == "" {
		a.Format = backup.DefaultFormat
	}

	// Using os.OpenFile here is expected, as long as `dst` is not a user
	// provided path.
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	// Select a writer based off of the WriteLimit configuration option. If there is no
	// write limit, use the file as the writer.
	var writer io.Writer
	if writeLimit := int64(config.Get().System.Backups.WriteLimit * 1024 * 1024); writeLimit > 0 {
		// Token bucket with a capacity of "writeLimit" MiB, adding "writeLimit" MiB/s
		// and then wrap the file writer with the token bucket limiter.
		writer = ratelimit.Writer(f, ratelimit.NewBucketWithRate(float64(writeLimit), writeLimit))
	} else {
		writer = f
	}

	return a.Stream(ctx, writer)
}

type walkFunc func(dirfd int, name, relative string, d ufs.DirEntry) error

// Stream streams the creation of the archive to the given writer.
func (a *Archive) Stream(ctx context.Context, w io.Writer) error {
	if a.Filesystem == nil {
		return errors.New("filesystem: archive.Filesystem is unset")
	}

	// If format is not set, use the default
	if a.Format == "" {
		a.Format = backup.DefaultFormat
	}

	// The base directory may come with a prefixed `/`, strip it to prevent
	// problems.
	a.BaseDirectory = strings.TrimPrefix(a.BaseDirectory, "/")

	if filesLen := len(a.Files); filesLen > 0 {
		files := make([]string, filesLen)
		for i, f := range a.Files {
			if !strings.HasPrefix(f, a.Filesystem.Path()) {
				files[i] = f
				continue
			}
			files[i] = strings.TrimPrefix(strings.TrimPrefix(f, a.Filesystem.Path()), "/")
		}
		a.Files = files
	}

	fs := a.Filesystem.unixFS

	// Initialize the appropriate archive writer based on format
	switch a.Format {
	case backup.FormatZip:
		// Choose which compression level to use based on the compression_level configuration option
		var compressionLevel int
		switch config.Get().System.Backups.CompressionLevel {
		case "none":
			compressionLevel = zip.Store
		case "best_compression":
			compressionLevel = zip.BestCompression
		default:
			compressionLevel = zip.BestSpeed
		}

		// Create a new ZIP writer around the provided writer
		zw := zip.NewWriter(w)
		zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
			return flate.NewWriter(out, compressionLevel)
		})
		defer zw.Close()
		a.w = zw

	case backup.FormatTarGz:
		// Choose which compression level to use based on the compression_level configuration option
		var compressionLevel int
		switch config.Get().System.Backups.CompressionLevel {
		case "none":
			compressionLevel = pgzip.NoCompression
		case "best_compression":
			compressionLevel = pgzip.BestCompression
		default:
			compressionLevel = pgzip.BestSpeed
		}

		// Create a new gzip writer around the file.
		gw, _ := pgzip.NewWriterLevel(w, compressionLevel)
		_ = gw.SetConcurrency(1<<20, 1)
		defer gw.Close()

		// Create a new tar writer around the gzip writer.
		tw := tar.NewWriter(gw)
		defer tw.Close()

		a.w = NewTarProgress(tw, a.Progress)
	}

	// If we're specifically looking for only certain files, or have requested
	// that certain files be ignored we'll update the callback function to reflect
	// that request.
	var callback walkFunc
	if len(a.Files) == 0 && len(a.Ignore) > 0 {
		i := ignore.CompileIgnoreLines(strings.Split(a.Ignore, "\n")...)
		callback = a.callback(func(_ int, _, relative string, _ ufs.DirEntry) error {
			if i.MatchesPath(relative) {
				return SkipThis
			}
			return nil
		})
	} else if len(a.Files) > 0 {
		callback = a.withFilesCallback()
	} else {
		callback = a.callback()
	}

	// Open the base directory we were provided.
	dirfd, name, closeFd, err := fs.SafePath(a.BaseDirectory)
	defer closeFd()
	if err != nil {
		return err
	}

	// Recursively walk the base directory.
	return fs.WalkDirat(dirfd, name, func(dirfd int, name, relative string, d ufs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return callback(dirfd, name, relative, d)
		}
	})
}

// Callback function used to determine if a given file should be included in the archive
// being generated.
func (a *Archive) callback(opts ...walkFunc) walkFunc {
	// Get the base directory we need to strip when walking.
	//
	// This is important as when we are walking, the last part of the base directory
	// is present on all the paths we walk.
	var base string
	if a.BaseDirectory != "" {
		base = filepath.Base(a.BaseDirectory) + "/"
	}
	return func(dirfd int, name, relative string, d ufs.DirEntry) error {
		// Skip directories because we are walking them recursively.
		if d.IsDir() {
			return nil
		}

		// If base isn't empty, strip it from the relative path. This fixes an
		// issue when creating an archive starting from a nested directory.
		//
		// See https://github.com/pterodactyl/panel/issues/5030 for more details.
		if base != "" {
			relative = strings.TrimPrefix(relative, base)
		}

		// Call the additional options passed to this callback function. If any of them return
		// a non-nil error we will exit immediately.
		for _, opt := range opts {
			if err := opt(dirfd, name, relative, d); err != nil {
				if err == SkipThis {
					return nil
				}
				return err
			}
		}

		// Add the file to the archive, if it is nested in a directory,
		// the directory will be automatically "created" in the archive.
		return a.addToArchive(dirfd, name, relative, d)
	}
}

var SkipThis = errors.New("skip this")

// Pushes only files defined in the Files key to the final archive.
func (a *Archive) withFilesCallback() walkFunc {
	return a.callback(func(_ int, _, relative string, _ ufs.DirEntry) error {
		for _, f := range a.Files {
			// Allow exact file matches, otherwise check if file is within a parent directory.
			//
			// The slashes are added in the prefix checks to prevent partial name matches from being
			// included in the archive.
			if f != relative && !strings.HasPrefix(strings.TrimSuffix(relative, "/")+"/", strings.TrimSuffix(f, "/")+"/") {
				continue
			}

			// Once we have a match return a nil value here so that the loop stops and the
			// call to this function will correctly include the file in the archive. If there
			// are no matches we'll never make it to this line, and the final error returned
			// will be the ufs.SkipDir error.
			return nil
		}

		return SkipThis
	})
}

// Adds a given file path to the final archive being created.
// Modify addToArchive method to handle different archive formats
func (a *Archive) addToArchive(dirfd int, name, relative string, entry ufs.DirEntry) error {
	s, err := entry.Info()
	if err != nil {
		if errors.Is(err, ufs.ErrNotExist) {
			return nil
		}
		return errors.WrapIff(err, "failed executing os.Lstat on '%s'", name)
	}

	// Skip socket files as they are unsupported by archives
	if s.Mode()&fs.ModeSocket != 0 {
		return nil
	}

	// Resolve the symlink target if the file is a symlink.
	var target string
	if s.Mode()&fs.ModeSymlink != 0 {
		target, err = os.Readlink(s.Name())
		if err != nil {
			if !os.IsNotExist(err) {
				log.WithField("name", name).WithField("readlink_err", err.Error()).Warn("failed reading symlink for target path; skipping...")
			}
			return nil
		}
	}

	switch a.Format {
	case backup.FormatZip:
		return a.addToZip(dirfd, name, relative, s, target)
	default: // FormatTarGz
		return a.addToTarGz(dirfd, name, relative, s, target)
	}
}

// addToZip adds a file to a ZIP archive
func (a *Archive) addToZip(dirfd int, name, relative string, s os.FileInfo, target string) error {
	// Get ZIP header
	header, err := zip.FileInfoHeader(s)
	if err != nil {
		return errors.WrapIff(err, "failed to get zip.FileInfoHeader for '%s'", name)
	}

	// Set the name in the header
	header.Name = relative

	// For symlinks, store the target path
	if s.Mode()&fs.ModeSymlink != 0 {
		header.Method = zip.Store
		header.Name = header.Name + ".symlink"
	} else if s.Mode().IsDir() {
		// If it's a directory, ensure it ends with a slash
		header.Name = strings.TrimSuffix(header.Name, "/") + "/"
		header.Method = zip.Store
	} else {
		// Use deflate for regular files
		header.Method = zip.Deflate
	}

	// Create the file entry in the ZIP
	zw, ok := a.w.(*zip.Writer)
	if !ok {
		return errors.New("archive writer is not a ZIP writer")
	}

	writer, err := zw.CreateHeader(header)
	if err != nil {
		return errors.WrapIff(err, "failed to create zip entry for '%s'", relative)
	}

	// If it's a symlink, write the target path
	if s.Mode()&fs.ModeSymlink != 0 {
		_, err = writer.Write([]byte(target))
		return err
	}

	// If it's a directory or zero-byte file, no need to write anything
	if s.IsDir() || s.Size() == 0 {
		return nil
	}

	// Open and copy the file
	f, err := a.Filesystem.unixFS.OpenFileat(dirfd, name, ufs.O_RDONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.WrapIff(err, "failed to open '%s' for copying", relative)
	}
	defer f.Close()

	// Get buffer for copying
	var buf []byte
	if s.Size() < memory {
		buf = make([]byte, s.Size())
	} else {
		buf = pool.Get().([]byte)
		defer func() {
			buf = make([]byte, memory)
			pool.Put(buf)
		}()
	}

	// Copy the file to the ZIP
	if _, err := io.CopyBuffer(writer, io.LimitReader(f, s.Size()), buf); err != nil {
		return errors.WrapIff(err, "failed to copy '%s' to zip archive", relative)
	}

	return nil
}

// addToTarGz adds a file to a tar.gz archive (existing implementation)
func (a *Archive) addToTarGz(dirfd int, name, relative string, s os.FileInfo, target string) error {
	// Get the tar FileInfoHeader in order to add the file to the archive.
	header, err := tar.FileInfoHeader(s, filepath.ToSlash(target))
	if err != nil {
		return errors.WrapIff(err, "failed to get tar#FileInfoHeader for '%s'", name)
	}

	// Fix the header name if the file is not a symlink.
	if s.Mode()&fs.ModeSymlink == 0 {
		header.Name = relative
	}

	tp, ok := a.w.(*TarProgress)
	if !ok {
		return errors.New("archive writer is not a TarProgress writer")
	}

	// Write the tar FileInfoHeader to the archive.
	if err := tp.WriteHeader(header); err != nil {
		return errors.WrapIff(err, "failed to write tar#FileInfoHeader for '%s'", name)
	}

	// If the size of the file is less than 1 (most likely for symlinks), skip writing the file.
	if header.Size < 1 {
		return nil
	}

	// If the buffer size is larger than the file size, create a smaller buffer to hold the file.
	var buf []byte
	if header.Size < memory {
		buf = make([]byte, header.Size)
	} else {
		// Get a fixed-size buffer from the pool to save on allocations.
		buf = pool.Get().([]byte)
		defer func() {
			buf = make([]byte, memory)
			pool.Put(buf)
		}()
	}

	// Open the file.
	f, err := a.Filesystem.unixFS.OpenFileat(dirfd, name, ufs.O_RDONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.WrapIff(err, "failed to open '%s' for copying", header.Name)
	}
	defer f.Close()

	// Copy the file's contents to the archive using our buffer.
	if _, err := io.CopyBuffer(tp, io.LimitReader(f, header.Size), buf); err != nil {
		return errors.WrapIff(err, "failed to copy '%s' to archive", header.Name)
	}
	return nil
}

// CompressFiles update to handle both formats
func (fs *Filesystem) CompressFiles(dir string, paths []string) (ufs.FileInfo, error) {
	format := backup.DefaultFormat
	a := &Archive{
		Filesystem:    fs,
		BaseDirectory: dir,
		Files:         paths,
		Format:        format,
		events:        fs.EventBus(),
	}

	// Determine archive name with the correct extension
	d := path.Join(
		dir,
		fmt.Sprintf("archive-%s%s",
			strings.ReplaceAll(time.Now().Format(time.RFC3339), ":", ""),
			format.GetExtension()),
	)

	f, err := fs.unixFS.OpenFile(d, ufs.O_WRONLY|ufs.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cw := ufs.NewCountedWriter(f)

	// If it's ao ZIP file, start progress monitoring
	if format == backup.FormatZip {
		monitor := backup.NewZipProgressMonitor(d, filepath.Base(d), fs.EventBus())
		defer monitor.Stop()
		monitor.Start(context.Background())
	}

	if err := a.Stream(context.Background(), cw); err != nil {
		return nil, err
	}

	if !fs.unixFS.CanFit(cw.BytesWritten()) {
		_ = fs.unixFS.Remove(d)
		return nil, newFilesystemError(ErrCodeDiskSpace, nil)
	}

	fs.unixFS.Add(cw.BytesWritten())
	return f.Stat()
}

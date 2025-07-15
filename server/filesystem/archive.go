package filesystem

import (
	"archive/tar"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/juju/ratelimit"
	"github.com/klauspost/pgzip"
	ignore "github.com/sabhiram/go-gitignore"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/progress"
	"github.com/pterodactyl/wings/internal/ufs"
)

const memory = 4 * 1024

var pool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, memory)
		return b
	},
}

// SkipThis is used as a return value to indicate that a file should be skipped.
var SkipThis = errors.New("skip this file")

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

// Archive represents the original tar.gz archive used for transfers
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

	w *TarProgress
}

// Create creates an archive at dst with all the files defined in the
// included Files array.
//
// THIS IS UNSAFE TO USE IF `dst` IS PROVIDED BY A USER! ONLY USE THIS WITH
// CONTROLLED PATHS!
func (a *Archive) Create(ctx context.Context, dst string) error {
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

// Stream streams the creation of the archive to the given writer.
func (a *Archive) Stream(ctx context.Context, w io.Writer) error {
	if a.Filesystem == nil {
		return errors.New("filesystem: archive.Filesystem is unset")
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

	fs := a.Filesystem.unixFS

	// FIXED: Use WalkDir instead of WalkDirat to avoid file descriptor management issues
	// WalkDir is simpler and doesn't have the complex dirfd lifecycle problems
	baseDir := a.BaseDirectory
	if baseDir == "" {
		baseDir = "."
	}

	// Prepare ignore matcher if needed
	var ignoreMatcher *ignore.GitIgnore
	if len(a.Files) == 0 && len(a.Ignore) > 0 {
		ignoreMatcher = ignore.CompileIgnoreLines(strings.Split(a.Ignore, "\n")...)
	}

	return fs.WalkDir(baseDir, func(path string, d ufs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Calculate relative path for the archive
		var relative string
		if path == baseDir || path == "." {
			relative = "."
		} else if baseDir == "." {
			relative = path
		} else {
			// Remove the base directory prefix to get relative path
			if strings.HasPrefix(path, baseDir+"/") {
				relative = strings.TrimPrefix(path, baseDir+"/")
			} else if path == baseDir {
				relative = "."
			} else {
				relative = path
			}
		}

		// Apply file filtering logic - skip files/directories that shouldn't be included
		if len(a.Files) == 0 && len(a.Ignore) > 0 {
			// Use ignore patterns
			if ignoreMatcher != nil && ignoreMatcher.MatchesPath(relative) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		} else if len(a.Files) > 0 {
			// Use specific file list
			found := false
			for _, f := range a.Files {
				// Check if the current file or directory is in the list of files to archive.
				if f == relative {
					found = true
					break
				}
				// Check if the current file or directory is a parent of any file in the list.
				if strings.HasPrefix(f, relative+"/") {
					found = true
					break
				}
				// Check if the current file or directory is a child of any file in the list.
				if strings.HasPrefix(relative, f+"/") || f == "." {
					found = true
					break
				}
			}
			if !found {
				// Skip this file/directory and its contents if it's a directory
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		return a.addToArchive(path, relative, d)
	})
}

// addToArchive adds a file to the archive using safe path-based operations
func (a *Archive) addToArchive(fullPath, relative string, d ufs.DirEntry) error {
	// FIXED: Get file info directly from filesystem path instead of using DirEntry.Info()
	// This avoids the "bad file descriptor" issue with the DirEntry
	absolutePath := filepath.Join(a.Filesystem.Path(), fullPath)
	s, err := os.Lstat(absolutePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.WrapIff(err, "failed to get file info for '%s'", fullPath)
	}

	// Error will come from tar#FileInfoHeader: "archive/tar: sockets not supported"
	if s.Mode()&fs.ModeSocket != 0 {
		return nil
	}

	// Resolve the symlink target if the file is a symlink.
	var target string
	if s.Mode()&fs.ModeSymlink != 0 {
		// Use the full filesystem path for reading symlinks
		absolutePath := filepath.Join(a.Filesystem.Path(), fullPath)
		target, err = os.Readlink(absolutePath)
		if err != nil {
			if !os.IsNotExist(err) {
				log.WithField("path", fullPath).WithField("readlink_err", err.Error()).Warn("failed reading symlink for target path; skipping...")
			}
			return nil
		}
	}

	// Get the tar FileInfoHeader in order to add the file to the archive.
	header, err := tar.FileInfoHeader(s, filepath.ToSlash(target))
	if err != nil {
		return errors.WrapIff(err, "failed to get tar#FileInfoHeader for '%s'", fullPath)
	}

	// Fix the header name if the file is not a symlink.
	if s.Mode()&fs.ModeSymlink == 0 {
		header.Name = relative
	}

	// Write the tar FileInfoHeader to the archive.
	if err := a.w.WriteHeader(header); err != nil {
		return errors.WrapIff(err, "failed to write tar#FileInfoHeader for '%s'", fullPath)
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

	// Open the file using path-based operation instead of dirfd-based
	f, err := a.Filesystem.unixFS.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.WrapIff(err, "failed to open '%s' for copying", header.Name)
	}
	defer f.Close()

	// Copy the file's contents to the archive using our buffer.
	if _, err := io.CopyBuffer(a.w, io.LimitReader(f, header.Size), buf); err != nil {
		return errors.WrapIff(err, "failed to copy '%s' to archive", header.Name)
	}
	return nil
}

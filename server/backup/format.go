package backup

import (
	"io"
	"path"
	"strings"

	"emperror.dev/errors"
	"github.com/mholt/archives"
)

// ArchiveFormat defines the type of archive format to use
type ArchiveFormat string

const (
	// FormatTarGz is the traditional tar.gz format
	FormatTarGz ArchiveFormat = "tar.gz"
	// FormatZip is the ZIP format
	FormatZip ArchiveFormat = "zip"
)

// ErrUnknownArchiveFormat is returned when the archive format cannot be determined
var ErrUnknownArchiveFormat = errors.New("unknown archive format")

// GetExtension returns the file extension for the given format
func (f ArchiveFormat) GetExtension() string {
	switch f {
	case FormatZip:
		return ".zip"
	default:
		return ".tar.gz"
	}
}

// GetFormat returns the archive format implementation to use
func (f ArchiveFormat) GetFormat() archives.Format {
	switch f {
	case FormatZip:
		return archives.Zip{}
	default:
		return archives.CompressedArchive{
			Compression: archives.Gz{},
			Archival:    archives.Tar{},
			Extraction:  archives.Tar{},
		}
	}
}

// DefaultFormat is the default archive format to use
var DefaultFormat = FormatZip

// GetFormatFromPath determines the format from a file path
func GetFormatFromPath(p string) ArchiveFormat {
	if strings.HasSuffix(p, ".zip") {
		return FormatZip
	}
	return FormatTarGz
}

// GetPathWithExtension returns a path with the correct extension for the format
func (f ArchiveFormat) GetPathWithExtension(basePath, identifier string) string {
	return path.Join(basePath, identifier+f.GetExtension())
}

// DetectFormat attempts to determine the format of an archive from its content
func DetectFormat(r io.Reader) (ArchiveFormat, error) {
	// Try to peek at the first few bytes to determine the format
	buffer := make([]byte, 512)
	n, err := r.Read(buffer)
	if err != nil && err != io.EOF {
		return "", err
	}

	// ZIP files start with PK signature
	if n >= 4 && buffer[0] == 0x50 && buffer[1] == 0x4B && buffer[2] == 0x03 && buffer[3] == 0x04 {
		return FormatZip, nil
	}

	// Check for gzip signature (1F 8B)
	if n >= 2 && buffer[0] == 0x1F && buffer[1] == 0x8B {
		return FormatTarGz, nil
	}

	return "", ErrUnknownArchiveFormat
}

package backup

import (
	"github.com/pterodactyl/wings/config"
)

func init() {
	// Set the default format based on configuration
	format := config.Get().System.ArchiveFormat
	switch format {
	case "zip":
		DefaultFormat = FormatZip
	case "tar.gz":
		DefaultFormat = FormatTarGz
	default:
		// If unknown format is specified, default to ZIP
		DefaultFormat = FormatZip
	}
}

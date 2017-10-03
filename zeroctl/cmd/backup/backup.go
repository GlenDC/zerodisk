package backup

import (
	"github.com/zero-os/0-Disk/config"
	"github.com/zero-os/0-Disk/nbd/ardb/backup"
)

// see `init` and `parsePosArguments` for more information
// about the meaning of each config property.
var vdiskCmdCfg struct {
	VdiskID             string                 // required
	SourceConfig        config.SourceConfig    // optional
	SnapshotID          string                 // optional
	BackupStorageConfig backup.StorageConfig   // optional
	PrivateKey          backup.CryptoKey       // optional
	CompressionType     backup.CompressionType // optional
	JobCount            int                    // optional
	Force               bool                   //optional
}

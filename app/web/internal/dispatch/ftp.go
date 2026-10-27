package dispatch

import (
	"context"

	"github.com/flyingEirc/Intern/internal/storage/ftp"
)

// ftpConfig is the narrow open request the dispatch package passes on. Keeping
// it local lets tests replace the open call without the storage package.
type ftpConfig struct {
	Address, Username, Password, BasePath, InstanceID, TLSMode string
}

// ftpOpen opens a real FTP repository. It is a variable so integration tests
// that already hold an open repository can reuse it instead of dialing again.
var ftpOpen = func(ctx context.Context, cfg ftpConfig) (*ftp.Repository, error) {
	return ftp.Open(ctx, ftp.Config{
		Address: cfg.Address, Username: cfg.Username, Password: cfg.Password,
		BasePath: cfg.BasePath, InstanceID: cfg.InstanceID, TLSMode: cfg.TLSMode,
	})
}

package dispatch

import (
	"context"

	"github.com/flyingEirc/Intern/internal/replication"
	"github.com/flyingEirc/Intern/internal/storage/ftp"
)

// ftpConfig is the narrow open request the dispatch package passes on. Keeping
// it local lets tests replace the open call without the storage package. TLSMode
// and CAPem are the repository's own frozen transport choice: dispatch never
// picks a mode and never supplies a trust bundle of its own.
type ftpConfig struct {
	Address, Username, Password, BasePath, InstanceID, TLSMode, CAPem string
}

// ftpOpen opens a real FTP or FTPS repository. It is a variable so integration
// tests that already hold an open repository can reuse it instead of dialing
// again.
var ftpOpen = func(ctx context.Context, cfg ftpConfig) (*ftp.Repository, error) {
	return ftp.Open(ctx, ftp.Config{
		Address: cfg.Address, Username: cfg.Username, Password: cfg.Password,
		BasePath: cfg.BasePath, InstanceID: cfg.InstanceID,
		TLSMode: cfg.TLSMode, CAPem: cfg.CAPem,
	})
}

// openRemote builds the Remote a replicator copies to. Production wraps the
// FTP repository ftpOpen returned; tests replace this with an in-memory Remote
// so a policy created after Start can be replicated without a live FTP server.
var openRemote = defaultOpenRemote

func defaultOpenRemote(ctx context.Context, cfg ftpConfig) (replication.Remote, func() error, error) {
	repo, err := ftpOpen(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	return replication.NewFTPRemote(repo), repo.Close, nil
}

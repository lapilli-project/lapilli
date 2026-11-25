package serve

import (
	"context"

	"lapilli.dev/internal/deploys"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// The deploys file is read by internal/deploys, which lapilli verdict reads
// it with too.

// LoadDeploys reads a deploys file and each history's lines; sum is a digest
// of the file and its histories.
func LoadDeploys(path string) ([]ledger.Deploy, string, error) { return deploys.Load(path) }

// readRegular reads a regular file, 8 MB at most.
func readRegular(path string) ([]byte, error) { return deploys.ReadRegular(path) }

// syncDeploys reads the deploys file again, and keeps what it holds: when
// it and its histories are as they were, nothing more is read or written.
func (s *Server) syncDeploys(ctx context.Context, o verdict.Options) error {
	f, err := deploys.Read(s.cfg.Deploys)
	if err != nil {
		return err
	}
	read := ledger.DeploysRead{At: s.now(), Sum: f.Sum}
	unchanged, err := s.l.DeploysUnchanged(ctx, read, o)
	if err != nil {
		return err
	}
	if !unchanged {
		ds, err := f.Deploys()
		if err != nil {
			return err
		}
		changed, err := s.l.SyncDeploys(ctx, ds, o, read)
		if err != nil {
			return err
		}
		if changed {
			s.log.Info("deploys file read again", "deploys", len(ds))
		}
	}
	s.deploysRead.Store(read.At.Unix())
	return nil
}

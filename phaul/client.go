package phaul

import (
	"fmt"
	"path/filepath"

	"github.com/checkpoint-restore/go-criu/v6"
	"github.com/checkpoint-restore/go-criu/v6/crit"
	"github.com/checkpoint-restore/go-criu/v6/crit/images"
	"google.golang.org/protobuf/proto"
)

const (
	minPagesWritten uint64 = 64
	maxIters        int    = 8
	maxGrowDelta    int64  = 32
)

// Client struct
type Client struct {
	local  Local
	remote Remote
	cfg    Config
}

// MakePhaulClient function
// Main entry point. Caller should create the client object by
// passing here local, remote and comm. See comment in corresponding
// interfaces/structs for explanation.
//
// Then call client.Migrate() and enjoy :)
func MakePhaulClient(l Local, r Remote, c Config) (*Client, error) {
	return &Client{local: l, remote: r, cfg: c}, nil
}

func isLastIter(iter int, stats *images.DumpStatsEntry, prevStats *images.DumpStatsEntry) bool {
	if iter >= maxIters {
		fmt.Printf("`- max iters reached\n")
		return true
	}

	pagesWritten := stats.GetPagesWritten()
	if pagesWritten < minPagesWritten {
		fmt.Printf("`- tiny pre-dump (%d) reached\n", int(pagesWritten))
		return true
	}

	pagesDelta := int64(pagesWritten) - int64(prevStats.GetPagesWritten())
	if pagesDelta >= maxGrowDelta {
		fmt.Printf("`- grow iter (%d) reached\n", int(pagesDelta))
		return true
	}

	return false
}

// Migrate function
func (pc *Client) Migrate() error {
	criu := criu.MakeCriu()
	psi := images.CriuPageServerInfo{
		Fd: proto.Int32(int32(pc.cfg.Memfd)),
	}
	opts := &images.CriuOpts{
		Pid:      proto.Int32(int32(pc.cfg.Pid)),
		LogLevel: proto.Int32(4),
		LogFile:  proto.String("pre-dump.log"),
		Ps:       &psi,
	}

	err := criu.Prepare()
	if err != nil {
		return err
	}

	defer criu.Cleanup()

	imgs, err := preparePhaulImages(pc.cfg.Wdir)
	if err != nil {
		return err
	}
	prevStats := &images.DumpStatsEntry{}
	iter := 0

	for {
		err = pc.remote.StartIter()
		if err != nil {
			return err
		}

		prevP := imgs.lastImagesDir()
		imgDir, err := imgs.openNextDir()
		if err != nil {
			return err
		}

		opts.ImagesDirFd = proto.Int32(int32(imgDir.Fd()))
		if prevP != "" {
			opts.ParentImg = proto.String(prevP)
		}

		err = criu.PreDump(opts, nil)
		imgDir.Close()
		if err != nil {
			return err
		}

		iter++

		err = pc.remote.StopIter()
		if err != nil {
			return err
		}

		// Get dump statistics with crit
		c := crit.New(filepath.Join(imgDir.Name(), "stats-dump"), "", "", false, false)
		statsImg, err := c.Decode()
		if err != nil {
			return err
		}
		stats := statsImg.Entries[0].Message.(*images.StatsEntry).GetDump()

		if isLastIter(iter, stats, prevStats) {
			break
		}

		prevStats = stats
	}

	err = pc.remote.StartIter()
	if err == nil {
		prevP := imgs.lastImagesDir()
		err = pc.local.DumpCopyRestore(criu, pc.cfg, prevP)
		err2 := pc.remote.StopIter()
		if err == nil {
			err = err2
		}
	}

	return err
}

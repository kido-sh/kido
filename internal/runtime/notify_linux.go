package runtime

import (
	"io"

	"github.com/kido-sh/kido/internal/mount"
	"github.com/kido-sh/kido/internal/notify"
)

func statBeneath(dirfd int, rel string) (notify.Record, uint64) {
	return notify.Record{Path: rel}, 0
}

func openNotify(shares []mount.Share, warn io.Writer) (*notifyBoot, error) {
	return nil, nil
}

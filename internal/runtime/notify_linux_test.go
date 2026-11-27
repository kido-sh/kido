package runtime

import (
	"io"
	"testing"

	"github.com/kido-sh/kido/internal/mount"
)

func TestOpenNotifyLinuxNoop(t *testing.T) {
	nb, err := openNotify([]mount.Share{{Name: "m0", Source: t.TempDir()}}, io.Discard)
	if nb != nil || err != nil {
		t.Fatalf("openNotify = %v, %v; want nil, nil", nb, err)
	}
}

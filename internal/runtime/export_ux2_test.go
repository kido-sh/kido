package runtime

import (
	"archive/tar"
	"bytes"
	"io"
	"slices"
	"testing"
)

func TestExportFilter(t *testing.T) {
	var in bytes.Buffer
	tw := tar.NewWriter(&in)
	add := func(h tar.Header, body string) {
		h.Size = int64(len(body))
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	add(tar.Header{Name: "./", Typeflag: tar.TypeDir}, "")
	add(tar.Header{Name: "./etc/", Typeflag: tar.TypeDir}, "")
	add(tar.Header{Name: "./etc/hosts", Typeflag: tar.TypeReg}, "127.0.0.1 localhost\n")
	add(tar.Header{Name: "./etc/hosts.link", Typeflag: tar.TypeLink, Linkname: "./etc/hosts"}, "")
	add(tar.Header{Name: "./dev/null", Typeflag: tar.TypeChar, Devmajor: 3, Devminor: 2}, "")
	add(tar.Header{Name: "./dev/disk0", Typeflag: tar.TypeBlock}, "")
	add(tar.Header{Name: "./../escape", Typeflag: tar.TypeReg}, "x")
	add(tar.Header{Name: "./bad.link", Typeflag: tar.TypeLink, Linkname: "../../etc/passwd"}, "")
	add(tar.Header{Name: "./tmp/fifo", Typeflag: tar.TypeFifo}, "")
	tw.Close()

	var out bytes.Buffer
	var skipped []string
	if err := exportFilter(&in, &out, func(s string) { skipped = append(skipped, s) }); err != nil {
		t.Fatal(err)
	}
	var names []string
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if h.Name == "etc/hosts" {
			if b, _ := io.ReadAll(tr); string(b) != "127.0.0.1 localhost\n" {
				t.Errorf("etc/hosts = %q", b)
			}
		}
		if h.Name == "etc/hosts.link" && h.Linkname != "etc/hosts" {
			t.Errorf("hard link target = %q", h.Linkname)
		}
	}
	if want := []string{"etc/", "etc/hosts", "etc/hosts.link", "tmp/fifo"}; !slices.Equal(names, want) {
		t.Errorf("entries %q, want %q", names, want)
	}
	if want := []string{"device ./dev/null", "device ./dev/disk0", "unsafe entry ../escape", "unsafe entry bad.link"}; !slices.Equal(skipped, want) {
		t.Errorf("skipped %q, want %q", skipped, want)
	}
}

func TestExportStderr(t *testing.T) {
	var skipped []string
	e := &exportStderr{skipped: func(s string) { skipped = append(skipped, s) }, rest: &tailBuffer{max: 1 << 10}}
	for _, chunk := range []string{"kido-skip sock", "et ./var/run/x\ntar: boom\n", "kido-skip device ./a\ntrailing"} {
		e.Write([]byte(chunk))
	}
	e.flush()
	if want := []string{"socket ./var/run/x", "device ./a"}; !slices.Equal(skipped, want) {
		t.Errorf("skipped %q, want %q", skipped, want)
	}
	if got := e.rest.String(); got != "tar: boom\ntrailing\n" {
		t.Errorf("rest = %q", got)
	}
}

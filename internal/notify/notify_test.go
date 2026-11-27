package notify

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestEventRoundTrip(t *testing.T) {
	evs := []Event{{Flags: 0x19100, FileID: 9887233, Path: "/r/p/a b/ü"}, {Flush: true},
		{Flags: FlagMustScanSubDirs, FileID: 1 << 40, Path: "/r"}}
	var b []byte
	for _, e := range evs {
		b = AppendEvent(b, e)
	}
	r := bytes.NewReader(b)
	for _, want := range evs {
		got, err := ReadEvent(r)
		if err != nil || got != want {
			t.Fatalf("ReadEvent = %+v, %v; want %+v", got, err, want)
		}
	}
	if _, err := ReadEvent(r); err != io.EOF {
		t.Fatalf("at the end: %v, want EOF", err)
	}
}

func TestReadEventRefuses(t *testing.T) {
	rec := func(n, kind, flags uint32, id uint64, path string) []byte {
		b := binary.LittleEndian.AppendUint32(nil, n)
		b = binary.LittleEndian.AppendUint32(b, kind)
		b = binary.LittleEndian.AppendUint32(b, flags)
		b = binary.LittleEndian.AppendUint64(b, id)
		return append(b, path...)
	}
	for name, b := range map[string][]byte{
		"length below the header":  rec(15, KindEvent, 0, 0, ""),
		"length past MaxPath":      rec(16+MaxPath+1, KindEvent, 0, 0, strings.Repeat("a", MaxPath+1)),
		"unknown kind":             rec(17, 3, 0, 0, "a"),
		"flush with a path":        rec(17, KindFlush, 0, 0, "a"),
		"flush with flags":         rec(16, KindFlush, 1, 0, ""),
		"flush with a file ID":     rec(16, KindFlush, 0, 7, ""),
		"cut in the header":        rec(17, KindEvent, 0, 0, "a")[:14],
		"cut in the path":          rec(18, KindEvent, 0, 0, "a"),
		"length beyond the stream": rec(100, KindEvent, 0, 0, "abc"),
	} {
		if _, err := ReadEvent(bytes.NewReader(b)); err == nil || err == io.EOF {
			t.Errorf("%s: got %v, want a decoding error", name, err)
		}
	}
}

func TestValidPath(t *testing.T) {
	good := []string{"a", "a/b", "a b/c", strings.Repeat("n", MaxName), strings.Repeat("d/", MaxDepth-1) + "f", ".x", "x..", "ü/ç"}
	for _, p := range good {
		if err := ValidPath(p); err != nil {
			t.Errorf("ValidPath(%q) = %v", p, err)
		}
	}
	bad := []string{"", "/a", "a/", "a//b", ".", "..", "a/./b", "a/../b", "../a", "a/..", "a\x00b",
		strings.Repeat("n", MaxName+1), strings.Repeat("d/", MaxDepth) + "f", strings.Repeat("a", MaxPath+1)}
	for _, p := range bad {
		if ValidPath(p) == nil {
			t.Errorf("ValidPath(%q) accepted it", p)
		}
	}
}

func TestBatchRoundTrip(t *testing.T) {
	recs := []Record{
		{State: Present, Mode: 0o100644, Nlink: 1, Size: 42, MtimeSec: 1795000000, MtimeNsec: 123456789,
			CtimeSec: -1, CtimeNsec: 999999999, Path: "dir/file"},
		{State: Absent, NameChanged: true, Path: "gone"},
		{State: Present, NameChanged: true, Replaced: true, Mode: 0o100600, Path: "swapped"},
		{State: Unknown, Path: "x/y/z"},
		{Flush: true},
	}
	var b []byte
	for _, r := range recs {
		var err error
		if b, err = AppendRecord(b, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ParseBatch(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(recs) {
		t.Fatalf("got %d records, want %d", len(got), len(recs))
	}
	for i := range recs {
		if got[i] != recs[i] {
			t.Errorf("record %d: got %+v, want %+v", i, got[i], recs[i])
		}
	}
	if len(b) != RecordSize("dir/file")+RecordSize("gone")+RecordSize("swapped")+RecordSize("x/y/z")+RecordSize("") {
		t.Errorf("RecordSize disagrees with the encoding: %d bytes", len(b))
	}
}

func TestAppendRecordRefuses(t *testing.T) {
	for name, r := range map[string]Record{
		"bad path":   {State: Absent, Path: "../x"},
		"bad state":  {State: 3, Path: "x"},
		"bad nsec":   {State: Present, Path: "x", MtimeNsec: 1e9},
		"empty path": {State: Absent},
	} {
		if _, err := AppendRecord(nil, r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseBatchRefusesWhole(t *testing.T) {
	good, _ := AppendRecord(nil, Record{State: Absent, Path: "ok"})
	mutate := func(f func(b []byte) []byte) []byte {
		b := append([]byte(nil), good...)
		return append(append([]byte(nil), good...), f(b)...)
	}
	for name, b := range map[string][]byte{
		"empty":               nil,
		"too large":           make([]byte, MaxBatch+1),
		"short header":        good[:recordHeader-1],
		"length mismatch":     mutate(func(b []byte) []byte { b[0]++; return b }),
		"path length too big": mutate(func(b []byte) []byte { b[6] = 200; return b }),
		"unknown flag":        mutate(func(b []byte) []byte { b[4] = 4; return b }),
		"reserved byte":       mutate(func(b []byte) []byte { b[5] = 1; return b }),
		"unknown kind":        mutate(func(b []byte) []byte { b[2] = 9; return b }),
		"bad state":           mutate(func(b []byte) []byte { b[3] = 7; return b }),
		"bad mtime nsec":      mutate(func(b []byte) []byte { binary.LittleEndian.PutUint32(b[40:], 1e9); return b }),
		"dot-dot path":        mutate(func(b []byte) []byte { copy(b[recordHeader:], ".."); return b }),
		"slash in path":       mutate(func(b []byte) []byte { b[recordHeader] = '/'; return b }),
		"nul in path":         mutate(func(b []byte) []byte { b[recordHeader+1] = 0; return b }),
		"flush with fields":   mutate(func(b []byte) []byte { b[2] = recordFlush; return b }),
		"trailing bytes":      append(append([]byte(nil), good...), 1, 2, 3),
	} {
		if _, err := ParseBatch(b); !errors.Is(err, ErrRecord) {
			t.Errorf("%s: got %v, want ErrRecord", name, err)
		}
	}
}

func TestFrames(t *testing.T) {
	batch, _ := AppendRecord(nil, Record{State: Absent, Path: "x"})
	f, err := EncodeFrame("m0", batch)
	if err != nil {
		t.Fatal(err)
	}
	share, got, err := DecodeFrame(f)
	if err != nil || share != "m0" || !bytes.Equal(got, batch) {
		t.Fatalf("DecodeFrame = %q, %v, %v", share, got, err)
	}
	if share, got, err := DecodeFrame(nil); share != "" || got != nil || err != nil {
		t.Fatalf("keepalive: %q, %v, %v", share, got, err)
	}
	for name, p := range map[string][]byte{
		"no name":         {0, 1, 2},
		"name past end":   {5, 'm'},
		"name, no batch":  {2, 'm', '0'},
		"batch too large": append([]byte{2, 'm', '0'}, make([]byte, MaxBatch+1)...),
		"bad name":        append([]byte{2, 'm', '/'}, batch...),
	} {
		if _, _, err := DecodeFrame(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := EncodeFrame("", batch); err == nil {
		t.Error("EncodeFrame accepted an empty share name")
	}
	for _, name := range []string{strings.Repeat("m", 65), "a/b", "m 0", "\x00"} {
		if _, err := EncodeFrame(name, batch); err == nil {
			t.Errorf("EncodeFrame accepted share name %q", name)
		}
	}
}

func TestValidShare(t *testing.T) {
	for name, want := range map[string]bool{
		"m0": true, "A_b.c-9": true, strings.Repeat("x", 64): true, "...": true,
		"": false, strings.Repeat("x", 65): false, "a/b": false, "..x\n": false, "é": false, ".": false, "..": false,
	} {
		if got := ValidShare(name); got != want {
			t.Errorf("ValidShare(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestCutDepth(t *testing.T) {
	deep := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf("d%d", i)
		}
		return strings.Join(parts, "/")
	}
	if p, cut := CutDepth(deep(MaxDepth)); cut || p != deep(MaxDepth) || ValidPath(p) != nil {
		t.Fatalf("%d components were cut: %v", MaxDepth, cut)
	}
	p, cut := CutDepth(deep(MaxDepth + 7))
	if !cut || p != deep(MaxDepth) || ValidPath(p) != nil {
		t.Fatalf("%d components: cut %v to %d components", MaxDepth+7, cut, strings.Count(p, "/")+1)
	}
	if p, cut := CutDepth("a"); cut || p != "a" {
		t.Fatalf("one component: %q %v", p, cut)
	}
}

func FuzzReadEvent(f *testing.F) {
	f.Add(AppendEvent(nil, Event{Flags: 1, Path: "/a/b"}))
	f.Add(AppendEvent(nil, Event{Flush: true}))
	f.Fuzz(func(t *testing.T, b []byte) {
		r := bytes.NewReader(b)
		for {
			e, err := ReadEvent(r)
			if err != nil {
				return
			}
			if len(e.Path) > MaxPath || (e.Flush && (e.Path != "" || e.Flags != 0 || e.FileID != 0)) {
				t.Fatalf("decoded an impossible event %+v", e)
			}
		}
	})
}

func FuzzParseBatch(f *testing.F) {
	seed, _ := AppendRecord(nil, Record{State: Present, Mode: 0o40755, Path: "a/b"})
	seed, _ = AppendRecord(seed, Record{Flush: true})
	f.Add(seed)
	f.Fuzz(func(t *testing.T, b []byte) {
		recs, err := ParseBatch(b)
		if err != nil {
			return
		}
		var again []byte
		for _, r := range recs {
			if !r.Flush && ValidPath(r.Path) != nil {
				t.Fatalf("accepted an invalid path %q", r.Path)
			}
			if again, err = AppendRecord(again, r); err != nil {
				t.Fatalf("a parsed record does not encode: %v", err)
			}
		}
		if !bytes.Equal(again, b) {
			t.Fatalf("parse and encode differ:\n%x\n%x", b, again)
		}
	})
}

// goldenBatch is also decoded by the driver's unit test (darwin, VirtioFS/tests/test_proto.c), so the
// two sides cannot drift apart.
const goldenBatch = "3800010200000800a4810000010000002a00000000000000c086fd6a00000000c186fd6a0000000015cd5b07b168de3a6469722f66696c65340001010100040000000000000000000000000000000000000000000000000000000000000000000000000000000000676f6e65300002000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"

func TestBatchGolden(t *testing.T) {
	var b []byte
	for _, r := range []Record{
		{State: Present, Mode: 0o100644, Nlink: 1, Size: 42, MtimeSec: 1795000000, MtimeNsec: 123456789,
			CtimeSec: 1795000001, CtimeNsec: 987654321, Path: "dir/file"},
		{State: Absent, NameChanged: true, Path: "gone"},
		{Flush: true},
	} {
		var err error
		if b, err = AppendRecord(b, r); err != nil {
			t.Fatal(err)
		}
	}
	if got := hex.EncodeToString(b); got != goldenBatch {
		t.Fatalf("encoding changed:\n got %s\nwant %s", got, goldenBatch)
	}
}

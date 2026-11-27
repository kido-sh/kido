// Package notify carries changes made on the Mac to a container's live mounts. kido-vmm writes a
// record for each FSEvents event to a pipe; the relay turns each into a batch record with the Mac's
// state of the path and sends batches to the guest agent, which hands them to the virtio-fs driver.
// Records only make the guest drop or expire what it cached, so a wrong or lost record costs at
// most a round trip or the server's attribute lifetime, never wrong contents.
package notify

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Records from kido-vmm: a u32 length of what follows it, a u32 kind, the u32 FSEvents flags, the
// u64 file ID (the inode the event was about, 0 if unknown) and the path, little-endian.
const (
	KindEvent uint32 = 1
	KindFlush uint32 = 2

	MaxPath = 4096 // longest path kido-vmm sends; longer ones arrive as flushes
)

// FSEvents flags that mean events were lost or the watched tree changed shape.
const (
	FlagMustScanSubDirs uint32 = 0x01
	FlagUserDropped     uint32 = 0x02
	FlagKernelDropped   uint32 = 0x04
	FlagEventIdsWrapped uint32 = 0x08
	FlagRootChanged     uint32 = 0x20
	FlagMount           uint32 = 0x40
	FlagUnmount         uint32 = 0x80

	FlagsLost = FlagMustScanSubDirs | FlagUserDropped | FlagKernelDropped | FlagEventIdsWrapped |
		FlagRootChanged | FlagMount | FlagUnmount
)

// FSEvents flags that mean the name itself was created, removed or renamed.
const (
	FlagItemCreated uint32 = 0x100
	FlagItemRemoved uint32 = 0x200
	FlagItemRenamed uint32 = 0x800

	FlagsName = FlagItemCreated | FlagItemRemoved | FlagItemRenamed
)

// Event is one record from kido-vmm: a change at Path, or a flush of every share.
type Event struct {
	Flush  bool
	Flags  uint32
	FileID uint64
	Path   string
}

var ErrRecord = errors.New("notify: malformed record")

// ReadEvent reads one record from kido-vmm. Any error leaves the stream unusable.
func ReadEvent(r io.Reader) (Event, error) {
	var h [eventHeader]byte
	if _, err := io.ReadFull(r, h[:4]); err != nil {
		return Event{}, err
	}
	n := binary.LittleEndian.Uint32(h[:4])
	if n < eventHeader-4 || n > eventHeader-4+MaxPath {
		return Event{}, ErrRecord
	}
	if _, err := io.ReadFull(r, h[4:]); err != nil {
		return Event{}, unexpected(err)
	}
	path := make([]byte, n-(eventHeader-4))
	if _, err := io.ReadFull(r, path); err != nil {
		return Event{}, unexpected(err)
	}
	flags, id := binary.LittleEndian.Uint32(h[8:]), binary.LittleEndian.Uint64(h[12:])
	switch binary.LittleEndian.Uint32(h[4:8]) {
	case KindEvent:
		return Event{Flags: flags, FileID: id, Path: string(path)}, nil
	case KindFlush:
		if len(path) != 0 || flags != 0 || id != 0 {
			return Event{}, ErrRecord
		}
		return Event{Flush: true}, nil
	}
	return Event{}, ErrRecord
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

const eventHeader = 20

// AppendEvent encodes e as kido-vmm does.
func AppendEvent(b []byte, e Event) []byte {
	kind, flags, id, path := KindEvent, e.Flags, e.FileID, e.Path
	if e.Flush {
		kind, flags, id, path = KindFlush, 0, 0, ""
	}
	b = binary.LittleEndian.AppendUint32(b, uint32(eventHeader-4+len(path)))
	b = binary.LittleEndian.AppendUint32(b, kind)
	b = binary.LittleEndian.AppendUint32(b, flags)
	b = binary.LittleEndian.AppendUint64(b, id)
	return append(b, path...)
}

// Batch records, which the agent passes to the driver unchanged:
//
//	u16 record length, u8 kind, u8 state, u8 flags, u8 zero, u16 path length, u32 mode, u32 nlink,
//	u64 size, i64 mtime sec, i64 ctime sec, u32 mtime nsec, u32 ctime nsec, path
//
// little-endian. The path is relative to the share root.
const (
	MaxName  = 1023 // the driver's VIRTIOFS_NAME_MAX
	MaxDepth = 64
	MaxBatch = 32 << 10

	recordHeader = 48
)

type State uint8

const (
	Unknown State = 0 // the Mac's state could not be read: always invalidate
	Absent  State = 1
	Present State = 2
)

// Record tells the driver that Path may have changed, or, with Flush, that anything may have.
// NameChanged says the name itself may have been created, removed or renamed, not only what it names;
// Replaced says it now names a different file than before.
type Record struct {
	Flush       bool
	NameChanged bool
	Replaced    bool
	State       State
	Mode        uint32
	Nlink       uint32
	Size        uint64
	MtimeSec    int64
	MtimeNsec   uint32
	CtimeSec    int64
	CtimeNsec   uint32
	Path        string
}

const (
	recordPath  = 1
	recordFlush = 2

	flagName     = 1
	flagReplaced = 2
)

// ValidPath reports whether p can name something below a share root: relative, components of 1
// to MaxName bytes that are neither "." nor "..", no NUL, at most MaxDepth deep.
func ValidPath(p string) error {
	if p == "" || len(p) > MaxPath || strings.IndexByte(p, 0) >= 0 {
		return fmt.Errorf("notify: bad path")
	}
	parts := strings.Split(p, "/")
	if len(parts) > MaxDepth {
		return fmt.Errorf("notify: path deeper than %d", MaxDepth)
	}
	for _, c := range parts {
		if c == "" || c == "." || c == ".." || len(c) > MaxName {
			return fmt.Errorf("notify: bad path component")
		}
	}
	return nil
}

// CutDepth cuts p to its first MaxDepth components, reporting whether it was deeper.
func CutDepth(p string) (string, bool) {
	n := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			if n++; n == MaxDepth {
				return p[:i], true
			}
		}
	}
	return p, false
}

// ValidShare reports whether name can name a share: 1 to 64 letters, digits, '_', '.' or '-', not
// "." or "..".
func ValidShare(name string) bool {
	if name == "" || len(name) > 64 || name == "." || name == ".." {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

// AppendRecord encodes r, refusing a path ValidPath refuses.
func AppendRecord(b []byte, r Record) ([]byte, error) {
	kind, path := byte(recordPath), r.Path
	if r.Flush {
		kind, path, r = recordFlush, "", Record{}
	} else if err := ValidPath(path); err != nil {
		return b, err
	}
	if r.State > Present || r.MtimeNsec >= 1e9 || r.CtimeNsec >= 1e9 {
		return b, ErrRecord
	}
	var flags byte
	if r.NameChanged {
		flags |= flagName
	}
	if r.Replaced {
		flags |= flagReplaced
	}
	b = binary.LittleEndian.AppendUint16(b, uint16(recordHeader+len(path)))
	b = append(b, kind, byte(r.State), flags, 0)
	b = binary.LittleEndian.AppendUint16(b, uint16(len(path)))
	b = binary.LittleEndian.AppendUint32(b, r.Mode)
	b = binary.LittleEndian.AppendUint32(b, r.Nlink)
	b = binary.LittleEndian.AppendUint64(b, r.Size)
	b = binary.LittleEndian.AppendUint64(b, uint64(r.MtimeSec))
	b = binary.LittleEndian.AppendUint64(b, uint64(r.CtimeSec))
	b = binary.LittleEndian.AppendUint32(b, r.MtimeNsec)
	b = binary.LittleEndian.AppendUint32(b, r.CtimeNsec)
	return append(b, path...), nil
}

// RecordSize is the encoded size of a record for path.
func RecordSize(path string) int { return recordHeader + len(path) }

// ParseBatch decodes a batch, refusing it whole if any record is malformed, as the driver does.
func ParseBatch(b []byte) ([]Record, error) {
	if len(b) == 0 || len(b) > MaxBatch {
		return nil, ErrRecord
	}
	var out []Record
	for len(b) > 0 {
		if len(b) < recordHeader {
			return nil, ErrRecord
		}
		n := int(binary.LittleEndian.Uint16(b))
		plen := int(binary.LittleEndian.Uint16(b[6:]))
		if n != recordHeader+plen || n > len(b) || b[4]&^(flagName|flagReplaced) != 0 || b[5] != 0 {
			return nil, ErrRecord
		}
		r := Record{
			NameChanged: b[4]&flagName != 0,
			Replaced:    b[4]&flagReplaced != 0,
			State:       State(b[3]),
			Mode:        binary.LittleEndian.Uint32(b[8:]),
			Nlink:       binary.LittleEndian.Uint32(b[12:]),
			Size:        binary.LittleEndian.Uint64(b[16:]),
			MtimeSec:    int64(binary.LittleEndian.Uint64(b[24:])),
			CtimeSec:    int64(binary.LittleEndian.Uint64(b[32:])),
			MtimeNsec:   binary.LittleEndian.Uint32(b[40:]),
			CtimeNsec:   binary.LittleEndian.Uint32(b[44:]),
			Path:        string(b[recordHeader:n]),
		}
		if r.State > Present || r.MtimeNsec >= 1e9 || r.CtimeNsec >= 1e9 {
			return nil, ErrRecord
		}
		switch b[2] {
		case recordPath:
			if ValidPath(r.Path) != nil {
				return nil, ErrRecord
			}
		case recordFlush:
			if r != (Record{}) {
				return nil, ErrRecord
			}
			r.Flush = true
		default:
			return nil, ErrRecord
		}
		out = append(out, r)
		b = b[n:]
	}
	return out, nil
}

// A notify frame from the relay to the agent carries one share's batch: the length of the share's
// name (u8), the name, then the batch. An empty frame is a keepalive.

// EncodeFrame builds the frame for share's batch.
func EncodeFrame(share string, batch []byte) ([]byte, error) {
	if !ValidShare(share) || len(batch) == 0 || len(batch) > MaxBatch {
		return nil, ErrRecord
	}
	b := make([]byte, 0, 1+len(share)+len(batch))
	b = append(b, byte(len(share)))
	b = append(b, share...)
	return append(b, batch...), nil
}

// DecodeFrame splits a frame into its share and batch; a keepalive gives "" and nil.
func DecodeFrame(p []byte) (string, []byte, error) {
	if len(p) == 0 {
		return "", nil, nil
	}
	n := int(p[0])
	if 1+n >= len(p) || len(p)-1-n > MaxBatch || !ValidShare(string(p[1:1+n])) {
		return "", nil, ErrRecord
	}
	return string(p[1 : 1+n]), p[1+n:], nil
}

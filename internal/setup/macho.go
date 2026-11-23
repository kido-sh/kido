package setup

import (
	"bytes"
	"debug/macho"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// HypervisorEntitlement lets a process use Hypervisor.framework; QEMU's HVF accelerator needs it.
const HypervisorEntitlement = "com.apple.security.hypervisor"

const VirtualizationEntitlement = "com.apple.security.virtualization"

const (
	lcLoadDylib                = 0xc
	lcReexportDylib            = 0x8000001f
	lcLoadUpwardDylib          = 0x80000023
	lcRpath                    = 0x8000001c
	lcCodeSignature            = 0x1d
	csMagicEmbeddedSignature   = 0xfade0cc0
	csMagicEmbeddedEntitlement = 0xfade7171
	csSlotEntitlements         = 5
	maxSignatureSize           = 16 << 20
	maxLibraries               = 512
)

// openArm64 opens the arm64 slice of a thin or universal Mach-O file.
func openArm64(path string) (*macho.File, io.ReaderAt, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, nil, err
	}
	slice := io.NewSectionReader(f, 0, fi.Size())
	if fat, err := macho.NewFatFile(f); err == nil {
		slice = nil
		for _, a := range fat.Arches {
			if a.Cpu == macho.CpuArm64 {
				slice = io.NewSectionReader(f, int64(a.Offset), int64(a.Size))
				break
			}
		}
		if slice == nil {
			f.Close()
			return nil, nil, nil, errors.New("the universal binary has no arm64 slice")
		}
	} else if !errors.Is(err, macho.ErrNotFat) {
		f.Close()
		return nil, nil, nil, fmt.Errorf("not a Mach-O file: %v", err)
	}
	m, err := macho.NewFile(slice)
	if err != nil {
		f.Close()
		return nil, nil, nil, fmt.Errorf("not a Mach-O file: %v", err)
	}
	if m.Cpu != macho.CpuArm64 {
		f.Close()
		return nil, nil, nil, fmt.Errorf("built for %v, not arm64", m.Cpu)
	}
	return m, slice, f.Close, nil
}

// HasEntitlement reports whether the code signature of the arm64 slice at path grants key (as
// `codesign -d --entitlements -` shows it), without passing path to another program.
func HasEntitlement(path, key string) (bool, error) {
	m, slice, closeFile, err := openArm64(path)
	if err != nil {
		return false, err
	}
	defer closeFile()
	for _, l := range m.Loads {
		raw := l.Raw()
		if len(raw) < 16 || m.ByteOrder.Uint32(raw) != lcCodeSignature {
			continue
		}
		off, size := m.ByteOrder.Uint32(raw[8:]), m.ByteOrder.Uint32(raw[12:])
		if size > maxSignatureSize {
			return false, errors.New("code signature too large")
		}
		sig := make([]byte, size)
		if _, err := slice.ReadAt(sig, int64(off)); err != nil {
			return false, fmt.Errorf("reading the code signature: %v", err)
		}
		plist, err := signatureEntitlements(sig)
		if err != nil || plist == nil {
			return false, err
		}
		return plistKeyIsTrue(plist, key)
	}
	return false, errors.New("the binary is not signed")
}

// signatureEntitlements returns the XML entitlements blob of a code signature super blob (all
// fields big-endian), or nil when it has none.
func signatureEntitlements(sig []byte) ([]byte, error) {
	be := binary.BigEndian
	if len(sig) < 12 || be.Uint32(sig) != csMagicEmbeddedSignature {
		return nil, errors.New("unrecognized code signature")
	}
	count := int(be.Uint32(sig[8:]))
	for i := 0; i < count; i++ {
		idx := 12 + 8*i
		if idx+8 > len(sig) {
			return nil, errors.New("truncated code signature")
		}
		if be.Uint32(sig[idx:]) != csSlotEntitlements {
			continue
		}
		off := int(be.Uint32(sig[idx+4:]))
		if off+8 > len(sig) || be.Uint32(sig[off:]) != csMagicEmbeddedEntitlement {
			return nil, errors.New("malformed entitlements in the code signature")
		}
		end := off + int(be.Uint32(sig[off+4:]))
		if end < off+8 || end > len(sig) {
			return nil, errors.New("malformed entitlements in the code signature")
		}
		return sig[off+8 : end], nil
	}
	return nil, nil
}

func plistKeyIsTrue(doc []byte, key string) (bool, error) {
	var plist struct {
		Dict struct {
			Items []struct {
				XMLName xml.Name
				Text    string `xml:",chardata"`
			} `xml:",any"`
		} `xml:"dict"`
	}
	if err := xml.NewDecoder(bytes.NewReader(doc)).Decode(&plist); err != nil {
		return false, fmt.Errorf("parsing the entitlements: %v", err)
	}
	items := plist.Dict.Items
	for i := 0; i+1 < len(items); i++ {
		if items[i].XMLName.Local == "key" && strings.TrimSpace(items[i].Text) == key {
			return items[i+1].XMLName.Local == "true", nil
		}
	}
	return false, nil
}

// Libraries is what CheckLibraries found.
type Libraries struct {
	// Missing are load commands (as written in the binary) that resolve to no file, so dyld
	// refuses to start the program.
	Missing []string
	// External are libraries found outside the executable's install tree, such as Homebrew's:
	// the program runs here but is not self-contained.
	External []string
}

// CheckLibraries follows the non-weak dylib load commands of the executable at exe and of every
// non-system library it reaches. Libraries under /usr/lib and /System live in the dyld shared
// cache and count as present. The install tree is the parent of exe's directory.
func CheckLibraries(exe string) (Libraries, error) {
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	exeDir := filepath.Dir(exe)
	tree := filepath.Dir(exeDir) + string(filepath.Separator)
	exeRpaths, err := imageRpaths(exe, exeDir)
	if err != nil {
		return Libraries{}, err
	}
	missing, external := map[string]bool{}, map[string]bool{}
	seen := map[string]bool{exe: true}
	queue := []string{exe}
	for len(queue) > 0 {
		image := queue[0]
		queue = queue[1:]
		deps, rpaths, err := imageDeps(image, exeDir)
		if err != nil {
			if image == exe {
				return Libraries{}, err
			}
			continue
		}
		rpaths = append(rpaths, exeRpaths...)
		for _, dep := range deps {
			if isSystemLibrary(dep) {
				continue
			}
			resolved := resolveDep(dep, exeDir, filepath.Dir(image), rpaths)
			if resolved == "" {
				missing[dep] = true
				continue
			}
			if !strings.HasPrefix(resolved, tree) {
				external[resolved] = true
			}
			if !seen[resolved] && len(seen) < maxLibraries {
				seen[resolved] = true
				queue = append(queue, resolved)
			}
		}
	}
	return Libraries{Missing: sortedKeys(missing), External: sortedKeys(external)}, nil
}

// HomebrewFormulas names the Homebrew formulas that library paths come from, e.g.
// /opt/homebrew/opt/glib/lib/libglib-2.0.0.dylib -> glib.
func HomebrewFormulas(paths []string) []string {
	formulas := map[string]bool{}
	for _, p := range paths {
		for _, prefix := range []string{"/opt/homebrew/opt/", "/opt/homebrew/Cellar/", "/usr/local/opt/", "/usr/local/Cellar/"} {
			if rest, ok := strings.CutPrefix(p, prefix); ok {
				if name, _, ok := strings.Cut(rest, "/"); ok && name != "" {
					formulas[name] = true
				}
				break
			}
		}
	}
	return sortedKeys(formulas)
}

func isSystemLibrary(path string) bool {
	return strings.HasPrefix(path, "/usr/lib/") || strings.HasPrefix(path, "/System/")
}

// resolveDep finds the file a load command names, or "".
func resolveDep(dep, exeDir, loaderDir string, rpaths []string) string {
	var candidates []string
	switch {
	case strings.HasPrefix(dep, "@executable_path/"):
		candidates = []string{filepath.Join(exeDir, strings.TrimPrefix(dep, "@executable_path/"))}
	case strings.HasPrefix(dep, "@loader_path/"):
		candidates = []string{filepath.Join(loaderDir, strings.TrimPrefix(dep, "@loader_path/"))}
	case strings.HasPrefix(dep, "@rpath/"):
		for _, rp := range rpaths {
			candidates = append(candidates, filepath.Join(rp, strings.TrimPrefix(dep, "@rpath/")))
		}
	case filepath.IsAbs(dep):
		candidates = []string{dep}
	}
	for _, c := range candidates {
		if isRegular(c) {
			if abs, err := filepath.EvalSymlinks(c); err == nil {
				return abs
			}
			return c
		}
	}
	return ""
}

// imageRpaths returns the LC_RPATH entries of a Mach-O image, with @executable_path and
// @loader_path expanded.
func imageRpaths(path, exeDir string) ([]string, error) {
	_, rpaths, err := imageDeps(path, exeDir)
	return rpaths, err
}

// imageDeps returns the libraries a Mach-O image loads (weak loads excluded: their absence does not
// stop the program) and its expanded LC_RPATH entries.
func imageDeps(path, exeDir string) (deps, rpaths []string, err error) {
	m, _, closeFile, err := openArm64(path)
	if err != nil {
		return nil, nil, err
	}
	defer closeFile()
	for _, l := range m.Loads {
		raw := l.Raw()
		if len(raw) < 12 {
			continue
		}
		switch m.ByteOrder.Uint32(raw) {
		case lcLoadDylib, lcReexportDylib, lcLoadUpwardDylib:
			if s := loadString(raw, m.ByteOrder.Uint32(raw[8:])); s != "" {
				deps = append(deps, s)
			}
		case lcRpath:
			s := loadString(raw, m.ByteOrder.Uint32(raw[8:]))
			switch {
			case strings.HasPrefix(s, "@executable_path"):
				s = filepath.Join(exeDir, strings.TrimPrefix(s, "@executable_path"))
			case strings.HasPrefix(s, "@loader_path"):
				s = filepath.Join(filepath.Dir(path), strings.TrimPrefix(s, "@loader_path"))
			}
			if s != "" {
				rpaths = append(rpaths, s)
			}
		}
	}
	return deps, rpaths, nil
}

// loadString reads the NUL-terminated string at offset inside a load command.
func loadString(raw []byte, offset uint32) string {
	if offset >= uint32(len(raw)) {
		return ""
	}
	s := raw[offset:]
	if i := bytes.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return string(s)
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

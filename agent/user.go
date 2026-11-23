package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var (
	passwdFile = "/etc/passwd"
	groupFile  = "/etc/group"
	namePat    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,254}$`)
)

type account struct {
	uid, gid uint32
	groups   []uint32
	home     string
}

// lookupUser resolves "user[:group]", each part a name or a number, the way Docker does: a uid
// missing from /etc/passwd runs with gid 0 and home "/".
func lookupUser(spec string) (*account, error) {
	userPart, groupPart, hasGroup := strings.Cut(spec, ":")
	if userPart == "" || (hasGroup && groupPart == "") {
		return nil, fmt.Errorf("invalid user %q", spec)
	}
	a := &account{home: "/"}
	name := ""
	if id, ok := parseID(userPart); ok {
		a.uid = id
		if e, err := findPasswd(func(e []string) bool { v, ok := parseDBID(e[2]); return ok && v == id }); err == nil && e != nil {
			name, a.gid, a.home = e[0], mustDBID(e[3]), e[5]
		}
	} else {
		if !namePat.MatchString(userPart) {
			return nil, fmt.Errorf("invalid user name %q", userPart)
		}
		e, err := findPasswd(func(e []string) bool { return e[0] == userPart })
		if err != nil {
			return nil, err
		}
		if e == nil {
			return nil, fmt.Errorf("unable to find user %s: no matching entries in passwd file", userPart)
		}
		v, ok := parseDBID(e[2])
		if !ok {
			return nil, fmt.Errorf("bad uid for %s in %s", userPart, passwdFile)
		}
		name, a.uid, a.gid, a.home = e[0], v, mustDBID(e[3]), e[5]
	}

	groups, err := readDB(groupFile, 4)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if hasGroup {
		if id, ok := parseID(groupPart); ok {
			a.gid = id
		} else {
			if !namePat.MatchString(groupPart) {
				return nil, fmt.Errorf("invalid group name %q", groupPart)
			}
			found := false
			for _, g := range groups {
				if g[0] == groupPart {
					if v, ok := parseDBID(g[2]); ok {
						a.gid, found = v, true
						break
					}
				}
			}
			if !found {
				return nil, fmt.Errorf("unable to find group %s: no matching entries in group file", groupPart)
			}
		}
	}

	a.groups = []uint32{a.gid}
	if name != "" {
		for _, g := range groups {
			gid, ok := parseDBID(g[2])
			if !ok || gid == a.gid {
				continue
			}
			for _, m := range strings.Split(g[3], ",") {
				if strings.TrimSpace(m) == name {
					a.groups = append(a.groups, gid)
					break
				}
			}
		}
	}
	if a.home == "" {
		a.home = "/"
	}
	return a, nil
}

func parseID(s string) (uint32, bool) {
	if s == "" || strings.TrimLeft(s, "0123456789") != "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 32)
	return uint32(v), err == nil
}

// parseDBID accepts the negative ids of the Darwin databases (nobody is -2).
func parseDBID(s string) (uint32, bool) {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || v < -1<<31 || v > 1<<32-1 {
		return 0, false
	}
	return uint32(v), true
}

func mustDBID(s string) uint32 {
	v, _ := parseDBID(s)
	return v
}

func findPasswd(match func([]string) bool) ([]string, error) {
	entries, err := readDB(passwdFile, 7)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if match(e) {
			return e, nil
		}
	}
	return nil, nil
}

// readDB reads a colon-separated database, skipping comments and lines with too few fields.
func readDB(path string, fields int) ([][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		e := strings.Split(line, ":")
		if len(e) < fields {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

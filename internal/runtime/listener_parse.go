package runtime

import (
	"fmt"
	"strconv"
	"strings"
)

func parseProcNetTCPInodes(data string, port int, ipv6 bool) []string {
	if port < 1 || port > 65535 {
		return nil
	}
	wantLocal := fmt.Sprintf("0100007F:%04X", port)
	if ipv6 {
		wantLocal = fmt.Sprintf("0000000000000000FFFF00000100007F:%04X", port)
	}
	var inodes []string
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || !strings.HasSuffix(fields[0], ":") {
			continue
		}
		if !strings.EqualFold(fields[1], wantLocal) || !strings.EqualFold(fields[3], "0A") {
			continue
		}
		inode := fields[9]
		if n, err := strconv.ParseUint(inode, 10, 64); err != nil || n == 0 {
			continue
		}
		inodes = append(inodes, inode)
	}
	return inodes
}

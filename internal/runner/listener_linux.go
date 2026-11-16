package runner

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func listenerPIDs(ctx context.Context, host string, port int) ([]int, error) {
	return linuxListenerPIDs(ctx, "/proc", host, port)
}

func linuxListenerPIDs(ctx context.Context, proc, host string, port int) ([]int, error) {
	inodes := make(map[string]bool)
	for _, table := range []string{"tcp", "tcp6"} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(filepath.Join(proc, "net", table))
		if table == "tcp6" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		found, err := parseTCPListeners(string(data), net.ParseIP(host), port)
		if err != nil {
			return nil, err
		}
		for _, inode := range found {
			inodes[inode] = false
		}
	}
	if len(inodes) == 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(proc)
	if err != nil {
		return nil, err
	}
	var owners []int
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		fds := filepath.Join(proc, entry.Name(), "fd")
		files, err := os.ReadDir(fds)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
				continue
			}
			return nil, err
		}
		owned := false
		for _, file := range files {
			link, err := os.Readlink(filepath.Join(fds, file.Name()))
			if err != nil {
				if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
					continue
				}
				return nil, err
			}
			inode, socket := strings.CutPrefix(link, "socket:[")
			inode, closed := strings.CutSuffix(inode, "]")
			if _, matches := inodes[inode]; socket && closed && matches {
				inodes[inode] = true
				owned = true
			}
		}
		if owned {
			owners = append(owners, pid)
		}
	}
	for inode, found := range inodes {
		if !found {
			return nil, fmt.Errorf("owner unavailable for listening socket %s", inode)
		}
	}
	return owners, nil
}

func parseTCPListeners(output string, host net.IP, port int) ([]string, error) {
	_, rows, ok := strings.Cut(output, "\n")
	if !ok {
		return nil, errors.New("invalid TCP table")
	}
	var inodes []string
	for row := range strings.SplitSeq(rows, "\n") {
		fields := strings.Fields(row)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 4 {
			return nil, errors.New("invalid TCP row")
		}
		if fields[3] != "0A" {
			continue
		}
		address, localPort, ok := strings.Cut(fields[1], ":")
		parsedPort, err := strconv.ParseUint(localPort, 16, 16)
		if !ok || err != nil || len(fields) < 10 {
			return nil, errors.New("invalid TCP listener")
		}
		if int(parsedPort) != port {
			continue
		}
		ip, err := hex.DecodeString(address)
		if err != nil || (len(ip) != net.IPv4len && len(ip) != net.IPv6len) {
			return nil, errors.New("invalid TCP address")
		}
		for i := 0; i < len(ip); i += 4 {
			binary.BigEndian.PutUint32(ip[i:], binary.NativeEndian.Uint32(ip[i:]))
		}
		if host.To4() == nil && net.IP(ip).To4() != nil {
			continue
		}
		if !net.IP(ip).Equal(host) && !net.IP(ip).IsUnspecified() && !host.IsUnspecified() {
			continue
		}
		if inode, err := strconv.ParseUint(fields[9], 10, 64); err != nil || inode == 0 {
			return nil, errors.New("invalid listening socket inode")
		}
		inodes = append(inodes, fields[9])
	}
	return inodes, nil
}

func processParents(ctx context.Context) (map[int]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	parents := make(map[int]int)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			continue
		}
		if err != nil {
			return nil, err
		}
		parent, err := parseProcessParent(string(data))
		if err != nil {
			return nil, err
		}
		parents[pid] = parent
	}
	return parents, nil
}

func parseProcessParent(stat string) (int, error) {
	_, fields, ok := strings.CutLast(stat, ")")
	values := strings.Fields(fields)
	if !ok || len(values) < 2 {
		return 0, errors.New("invalid process stat")
	}
	parent, err := strconv.Atoi(values[1])
	if err != nil || parent < 0 {
		return 0, errors.New("invalid parent PID")
	}
	return parent, nil
}

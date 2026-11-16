//go:build windows

package runner

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

func listenerPIDs(ctx context.Context, host string, port int) ([]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	query := windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")
	family, stride, portOffset, pidOffset := uintptr(windows.AF_INET), 24, 8, 20
	if net.ParseIP(host).To4() == nil {
		family, stride, portOffset, pidOffset = windows.AF_INET6, 56, 20, 52
	}
	var size uint32
	code, _, _ := query.Call(0, uintptr(unsafe.Pointer(&size)), 0, family, 3, 0)
	if windows.Errno(code) != windows.ERROR_INSUFFICIENT_BUFFER {
		return nil, fmt.Errorf("TCP table sizing returned %d", code)
	}
	buffer := make([]byte, size)
	code, _, _ = query.Call(uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&size)), 0, family, 3, 0)
	if code != 0 {
		return nil, fmt.Errorf("TCP table lookup returned %d", code)
	}
	count := int(binary.LittleEndian.Uint32(buffer[:4]))
	var pids []int
	for i := range count {
		row := buffer[4+i*stride : 4+(i+1)*stride]
		address := net.IP(row[4:8])
		if family == windows.AF_INET6 {
			address = net.IP(row[:16])
		}
		if int(binary.BigEndian.Uint16(row[portOffset:])) == port && (address.Equal(net.ParseIP(host)) || address.IsUnspecified() || net.ParseIP(host).IsUnspecified()) {
			pids = append(pids, int(binary.LittleEndian.Uint32(row[pidOffset:])))
		}
	}
	return pids, nil
}

func processParents(ctx context.Context) (map[int]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{}
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, err
	}
	parents := make(map[int]int)
	for {
		parents[int(entry.ProcessID)] = int(entry.ParentProcessID)
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return parents, nil
			}
			return nil, err
		}
	}
}

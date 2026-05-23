//go:build windows

package lifecycle

func stopProcessGroup(pid int) bool {
	return false
}

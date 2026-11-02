package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type processState int

const (
	processUnknown processState = iota
	processMatched
	processMismatched
	processAbsent
)

func (s processState) String() string {
	switch s {
	case processMatched:
		return "matched"
	case processMismatched:
		return "mismatched"
	case processAbsent:
		return "absent"
	default:
		return "unknown"
	}
}

// routerProcess is opened once per PID so inspection, termination, and exit
// confirmation refer to the same process wherever the OS provides a handle.
type routerProcess interface {
	Inspect(stableBinary string) (processState, error)
	Terminate() error
	Close() error
}

// parseProcArgs2Path returns the executable path from a Darwin KERN_PROCARGS2
// buffer: a 4-byte C int argc followed by the NUL-terminated exec path.
func parseProcArgs2Path(buf []byte) (string, error) {
	if len(buf) <= 4 {
		return "", errors.New("kern.procargs2 buffer is too short")
	}
	path, _, found := bytes.Cut(buf[4:], []byte{0})
	if !found {
		return "", errors.New("kern.procargs2 executable path is not terminated")
	}
	if len(path) == 0 {
		return "", errors.New("kern.procargs2 executable path is empty")
	}
	return string(path), nil
}

// compareUnixExecutable compares by resolved path, then by file identity.
// A deleted file still matches by its resolved path, so a router whose binary
// was replaced or removed is recognized.
func compareUnixExecutable(exe, stableBinary string) (processState, error) {
	resolvedExe, err := resolveExecutablePath(exe)
	if err != nil {
		return processUnknown, err
	}
	resolvedStable, err := resolveExecutablePath(stableBinary)
	if err != nil {
		return processUnknown, err
	}
	if resolvedExe == resolvedStable {
		return processMatched, nil
	}
	exeInfo, err := os.Stat(resolvedExe)
	if err != nil {
		return processUnknown, err
	}
	stableInfo, err := os.Stat(resolvedStable)
	if err != nil {
		return processUnknown, err
	}
	if os.SameFile(exeInfo, stableInfo) {
		return processMatched, nil
	}
	return processMismatched, nil
}

func resolveExecutablePath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(path)), nil
}

// linuxExecutableLink strips the " (deleted)" marker the kernel appends to
// /proc/<pid>/exe when the running binary was unlinked or replaced.
func linuxExecutableLink(target string, lstat func(string) (os.FileInfo, error)) string {
	trimmed, deleted := strings.CutSuffix(target, " (deleted)")
	if !deleted {
		return target
	}
	if _, err := lstat(target); err == nil {
		return target
	}
	return trimmed
}

// windowsExecutableMatches also accepts the rename-aside backup names created
// by replaceInstalledFileForGOOS while the old router image is still running.
func windowsExecutableMatches(exe, stableBinary string) bool {
	if strings.EqualFold(exe, stableBinary) {
		return true
	}
	exeDir, exeBase := splitWindowsPath(exe)
	stableDir, stableBase := splitWindowsPath(stableBinary)
	if stableBase == "" || !strings.EqualFold(exeDir, stableDir) {
		return false
	}
	exeBase = strings.ToLower(exeBase)
	prefix := strings.ToLower(stableBase) + "."
	return strings.HasPrefix(exeBase, prefix) && strings.HasSuffix(exeBase, ".bak") && len(exeBase) > len(prefix)+len(".bak")
}

func splitWindowsPath(path string) (dir, base string) {
	i := strings.LastIndexAny(path, `\/`)
	return path[:i+1], path[i+1:]
}

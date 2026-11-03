package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	RouterPIDFilename           = "router.pid"
	routerRuntimeLockName       = "router.runtime.lock"
	routerRuntimeLockWait       = tokenLockTimeout
	maxPIDRecordBytes     int64 = 64
)

type PIDRecordState int

const (
	PIDRecordMissing PIDRecordState = iota
	PIDRecordPresent
	PIDRecordUnknown
)

// PIDRecord is router.pid as read once. Unknown records are never removed by
// gohere; they need diagnosis.
type PIDRecord struct {
	State   PIDRecordState
	PID     int
	ModTime time.Time
	Err     error
}

var ErrRuntimeRecordChanged = errors.New("router runtime record changed")

func ReadPIDRecord(stateDir string) PIDRecord {
	path := filepath.Join(stateDir, RouterPIDFilename)
	data, err := readSmallFile(path, maxPIDRecordBytes)
	if errors.Is(err, os.ErrNotExist) {
		return PIDRecord{State: PIDRecordMissing}
	}
	if err != nil {
		return PIDRecord{State: PIDRecordUnknown, Err: fmt.Errorf("read %s: %w", path, err)}
	}
	pid, err := parsePIDRecord(data)
	if err != nil {
		return PIDRecord{State: PIDRecordUnknown, Err: fmt.Errorf("%s: %w", path, err)}
	}
	record := PIDRecord{State: PIDRecordPresent, PID: pid}
	if info, err := os.Stat(path); err == nil {
		record.ModTime = info.ModTime()
	}
	return record
}

func parsePIDRecord(data []byte) (int, error) {
	text := bytes.TrimSpace(data)
	if len(text) == 0 {
		return 0, errors.New("router PID record is empty")
	}
	for _, c := range text {
		if c < '0' || c > '9' {
			return 0, errors.New("router PID record is not a positive decimal PID")
		}
	}
	pid, err := strconv.Atoi(string(text))
	if err != nil || pid <= 0 {
		return 0, errors.New("router PID record is not a positive decimal PID")
	}
	return pid, nil
}

func readSmallFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("record is too large")
	}
	return data, nil
}

// LockRuntimeRecords serializes writes and conditional removals of router.pid
// and router.instance so a stale writer cannot erase a newer router's records.
// The state directory must already exist; locking never creates it.
func LockRuntimeRecords(stateDir string) (func(), error) {
	lockPath := filepath.Join(stateDir, routerRuntimeLockName)
	unlock, err := acquireStateLock(lockPath, routerRuntimeLockWait)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("timed out waiting for router runtime lock: %s", lockPath)
	}
	return unlock, err
}

func WritePIDRecord(stateDir string, pid int) error {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	unlock, err := LockRuntimeRecords(stateDir)
	if err != nil {
		return err
	}
	defer unlock()
	return writePIDFile(filepath.Join(stateDir, RouterPIDFilename), pid)
}

// RemovePIDRecordIf removes router.pid only if it still names pid. A missing
// record is success; a changed or unreadable record is preserved.
func RemovePIDRecordIf(stateDir string, pid int) error {
	unlock, err := LockRuntimeRecords(stateDir)
	if err != nil {
		return err
	}
	defer unlock()
	return removePIDRecordIfLocked(stateDir, pid)
}

func removePIDRecordIfLocked(stateDir string, pid int) error {
	record := ReadPIDRecord(stateDir)
	switch {
	case record.State == PIDRecordMissing:
		return nil
	case record.State == PIDRecordUnknown:
		return record.Err
	case record.PID != pid:
		return fmt.Errorf("%w: router.pid now names PID %d", ErrRuntimeRecordChanged, record.PID)
	}
	err := os.Remove(filepath.Join(stateDir, RouterPIDFilename))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func removeInstanceRecordIfLocked(stateDir, instanceID string) error {
	path := filepath.Join(stateDir, RouterInstanceFilename)
	data, err := readSmallFile(path, 1024)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if string(bytes.TrimSpace(data)) != instanceID {
		return fmt.Errorf("%w: router.instance was replaced", ErrRuntimeRecordChanged)
	}
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func publishRuntimeRecords(stateDir string, pid int, instanceID string) error {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	unlock, err := LockRuntimeRecords(stateDir)
	if err != nil {
		return err
	}
	defer unlock()
	if err := writePIDFile(filepath.Join(stateDir, RouterPIDFilename), pid); err != nil {
		return err
	}
	if err := writeRouterInstance(filepath.Join(stateDir, RouterInstanceFilename), instanceID); err != nil {
		return errors.Join(err, removePIDRecordIfLocked(stateDir, pid))
	}
	return nil
}

// releaseRuntimeRecords removes only this router's own records, so an old
// router closing late cannot erase a newer router's PID or instance.
func releaseRuntimeRecords(stateDir string, pid int, instanceID string) error {
	if _, err := os.Stat(stateDir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	unlock, err := LockRuntimeRecords(stateDir)
	if err != nil {
		return err
	}
	defer unlock()
	return errors.Join(removePIDRecordIfLocked(stateDir, pid), removeInstanceRecordIfLocked(stateDir, instanceID))
}

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/roie/gohere/internal/companion"
	"github.com/roie/gohere/internal/router"
	"github.com/roie/gohere/internal/wsledge"
)

type wslPublicTransport string

const (
	wslTransportDirect wslPublicTransport = "direct"
	wslTransportEdge   wslPublicTransport = "stdio-edge"
)

var (
	probeWSLRouterIdentityFunc   = wsledge.ProbeRouterIdentity
	startWSLEdgeFunc             = wsledge.StartDetached
	wslEdgeRunningFunc           = wsledge.Running
	ensureWSLPublicTransportFunc = ensureWSLPublicTransport
)

func ensureWSLPublicTransport(ctx context.Context, info companion.Info, companionBinary string) (wslPublicTransport, error) {
	matched, _, directErr := probeWSLRouterIdentityFunc(ctx, "http://127.0.0.1", info.RouterInstanceID)
	integrationDir := filepath.Join(router.DefaultStateDir(), wslIntegrationDirname)
	if matched {
		if wslEdgeRunningFunc(integrationDir) {
			return wslTransportEdge, nil
		}
		return wslTransportDirect, nil
	}
	if strings.TrimSpace(companionBinary) == "" {
		return "", errors.New("Windows companion path is unavailable for the WSL loopback edge")
	}
	edgeBinary := filepath.Join(integrationDir, "bin", wslEdgeBinaryName)
	if !exists(edgeBinary) {
		return "", errors.New("WSL loopback edge is not installed; run gohere setup")
	}
	logPath := filepath.Join(integrationDir, "edge.log")
	_, startErr := startWSLEdgeFunc(ctx, edgeBinary, companionBinary, logPath)

	deadline := time.Now().Add(routerStartTimeout)
	var lastErr error = directErr
	for time.Now().Before(deadline) {
		matched, observed, err := probeWSLRouterIdentityFunc(ctx, "http://127.0.0.1", info.RouterInstanceID)
		if matched {
			return wslTransportEdge, nil
		}
		if err != nil {
			lastErr = err
		} else if observed != "" {
			lastErr = fmt.Errorf("observed router instance %s, expected %s", observed, info.RouterInstanceID)
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	if startErr != nil {
		lastErr = errors.Join(startErr, lastErr)
	}
	if detail := edgeLogTail(logPath, 4096); detail != "" {
		lastErr = errors.Join(lastErr, errors.New(detail))
	}
	return "", fmt.Errorf("WSL loopback edge did not become ready: %w", lastErr)
}

func inspectWSLPublicTransport(ctx context.Context, info companion.Info) (wslPublicTransport, string) {
	matched, observed, err := probeWSLRouterIdentityFunc(ctx, "http://127.0.0.1", info.RouterInstanceID)
	if matched {
		integrationDir := filepath.Join(router.DefaultStateDir(), wslIntegrationDirname)
		if wslEdgeRunningFunc(integrationDir) {
			return wslTransportEdge, ""
		}
		return wslTransportDirect, ""
	}
	if err != nil {
		return "", err.Error()
	}
	if observed != "" {
		return "", fmt.Sprintf("router identity mismatch: observed %s", observed)
	}
	return "", "router identity was not returned"
}

func ServeWSLEdge(ctx context.Context, companionBinary string) error {
	if !detectWSLFunc() {
		return errors.New("gohere loopback edge requires WSL2")
	}
	if !detectWSL2Func() {
		return errors.New("gohere loopback edge does not support WSL1")
	}
	integrationDir := filepath.Join(router.DefaultStateDir(), wslIntegrationDirname)
	logPath := filepath.Join(integrationDir, "edge.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	return wsledge.Serve(ctx, wsledge.Config{
		StateDir:        integrationDir,
		CompanionBinary: companionBinary,
		HTTPS:           true,
		LogOutput:       logFile,
	})
}

func edgeLogTail(path string, limit int64) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	start := info.Size() - limit
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

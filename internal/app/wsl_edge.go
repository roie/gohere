package app

import (
	"context"
	"crypto/sha256"
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
	probeWSLRouterIdentityFunc       = wsledge.ProbeRouterIdentity
	probeWSLDirectRouterIdentityFunc = wsledge.ProbeRouterIdentity
	startWSLEdgeFunc                 = wsledge.StartDetached
	inspectWSLEdgeFunc               = wsledge.Inspect
	ensureWSLPublicTransportFunc     = ensureWSLPublicTransport
	wslEdgeReadyTimeout              = routerStartTimeout
)

func ensureWSLPublicTransport(ctx context.Context, info companion.Info, companionBinary string) (wslPublicTransport, error) {
	integrationDir := filepath.Join(router.DefaultStateDir(), wslIntegrationDirname)
	logPath := filepath.Join(integrationDir, "edge.log")
	running, edgeRunning, inspectErr := inspectWSLEdgeFunc(integrationDir)
	if edgeRunning && running.CompanionBinary == "" {
		running.CompanionBinary = companionBinary
	}
	direct, observed, directErr := probeWSLDirectRouterIdentityFunc(ctx, "http://127.0.0.1:39399", info.RouterInstanceID)
	if direct {
		if inspectErr != nil {
			return "", fmt.Errorf("could not verify the running WSL edge before switching to direct transport: %w", inspectErr)
		}
		if edgeRunning {
			if err := wslStopEdgeFunc(integrationDir); err != nil {
				return "", fmt.Errorf("could not stop the verified WSL edge before switching to direct transport: %w", err)
			}
		}
		matched, validationObserved, validationErr := probeWSLRouterIdentityFunc(ctx, "http://127.0.0.1", info.RouterInstanceID)
		if matched {
			return wslTransportDirect, nil
		}
		transitionErr := fmt.Errorf("direct transport validation failed: %w", identityProbeError(validationErr, validationObserved, info.RouterInstanceID))
		if edgeRunning {
			if rollbackErr := restoreWSLEdge(ctx, running, logPath, info.RouterInstanceID); rollbackErr != nil {
				return "", errors.Join(transitionErr, fmt.Errorf("WSL edge rollback failed: %w", rollbackErr))
			}
		}
		return "", transitionErr
	}
	if inspectErr != nil {
		return "", fmt.Errorf("could not verify the running WSL edge: %w", inspectErr)
	}
	if strings.TrimSpace(companionBinary) == "" {
		return "", errors.New("Windows companion path is unavailable for the WSL loopback edge")
	}
	metadata, err := currentWSLMetadataFunc()
	if err != nil {
		return "", fmt.Errorf("WSL loopback edge metadata is unavailable; run gohere setup: %w", err)
	}
	if err := verifyWSLEdgeCandidate(metadata); err != nil {
		return "", err
	}
	if edgeRunning && running.EdgeBinary == metadata.EdgeBinary && running.EdgeSHA256 == metadata.EdgeSHA256 && running.CompanionBinary == companionBinary {
		matched, _, probeErr := probeWSLRouterIdentityFunc(ctx, "http://127.0.0.1", info.RouterInstanceID)
		if matched {
			return wslTransportEdge, nil
		}
		directErr = errors.Join(directErr, probeErr)
	}
	if edgeRunning {
		if err := wslStopEdgeFunc(integrationDir); err != nil {
			return "", fmt.Errorf("could not stop the verified WSL edge for replacement: %w", err)
		}
	}
	_, startErr := startWSLEdgeFunc(ctx, metadata.EdgeBinary, companionBinary, logPath)
	replacementCause := startErr
	if startErr == nil {
		if readyErr := waitForWSLEdgeIdentity(ctx, info.RouterInstanceID); readyErr == nil {
			return wslTransportEdge, nil
		} else {
			replacementCause = readyErr
		}
	}
	directIdentityErr := directErr
	if directIdentityErr == nil {
		directIdentityErr = identityProbeError(nil, observed, info.RouterInstanceID)
	}
	replacementErr := fmt.Errorf("WSL edge replacement failed: %w", errors.Join(replacementCause, directIdentityErr))
	if startErr == nil {
		if cleanupErr := wslStopEdgeFunc(integrationDir); cleanupErr != nil {
			return "", errors.Join(replacementErr, fmt.Errorf("failed replacement edge could not be stopped safely: %w", cleanupErr))
		}
	}
	if edgeRunning {
		if rollbackErr := restoreWSLEdge(ctx, running, logPath, info.RouterInstanceID); rollbackErr != nil {
			return "", errors.Join(replacementErr, fmt.Errorf("WSL edge rollback failed: %w", rollbackErr))
		}
	}
	if detail := edgeLogTail(logPath, 4096); detail != "" {
		replacementErr = errors.Join(replacementErr, errors.New(detail))
	}
	return "", replacementErr
}

func verifyWSLEdgeCandidate(metadata wslIntegrationMetadata) error {
	if !filepath.IsAbs(metadata.EdgeBinary) || len(metadata.EdgeSHA256) != sha256.Size*2 {
		return errors.New("WSL loopback edge metadata is incomplete; run gohere setup")
	}
	hash, err := fileSHA256Hex(metadata.EdgeBinary)
	if err != nil {
		return fmt.Errorf("WSL loopback edge candidate is unavailable; run gohere setup: %w", err)
	}
	if hash != metadata.EdgeSHA256 {
		return errors.New("WSL loopback edge candidate hash mismatch; run gohere setup")
	}
	return nil
}

func waitForWSLEdgeIdentity(ctx context.Context, expectedInstanceID string) error {
	deadline := time.Now().Add(wslEdgeReadyTimeout)
	var lastErr error
	for {
		matched, observed, err := probeWSLRouterIdentityFunc(ctx, "http://127.0.0.1", expectedInstanceID)
		if matched {
			return nil
		}
		lastErr = identityProbeError(err, observed, expectedInstanceID)
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return lastErr
		}
		delay := 100 * time.Millisecond
		if remaining < delay {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func restoreWSLEdge(ctx context.Context, previous wsledge.RunningInfo, logPath, expectedInstanceID string) error {
	if _, err := startWSLEdgeFunc(ctx, previous.EdgeBinary, previous.CompanionBinary, logPath); err != nil {
		return err
	}
	return waitForWSLEdgeIdentity(ctx, expectedInstanceID)
}

func identityProbeError(err error, observed, expected string) error {
	if err != nil {
		return err
	}
	if observed != "" {
		return fmt.Errorf("observed router instance %s, expected %s", observed, expected)
	}
	return errors.New("router identity was not returned")
}

func inspectWSLPublicTransport(ctx context.Context, info companion.Info) (wslPublicTransport, string) {
	matched, observed, err := probeWSLDirectRouterIdentityFunc(ctx, "http://127.0.0.1:39399", info.RouterInstanceID)
	if matched {
		return wslTransportDirect, ""
	}
	integrationDir := filepath.Join(router.DefaultStateDir(), wslIntegrationDirname)
	if _, running, inspectErr := inspectWSLEdgeFunc(integrationDir); inspectErr == nil && running {
		matched, observed, err = probeWSLRouterIdentityFunc(ctx, "http://127.0.0.1", info.RouterInstanceID)
		if matched {
			return wslTransportEdge, ""
		}
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

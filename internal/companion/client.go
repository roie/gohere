package companion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/roie/gohere/internal/router"
)

type ProcessRunner interface {
	Run(context.Context, string, []string, []byte) ([]byte, []byte, error)
}

type ExecRunner struct{}

const (
	wslInteropMaxAttempts = 3
	wslInteropRetryDelay  = 100 * time.Millisecond
)

func (ExecRunner) Run(ctx context.Context, binary string, args []string, stdin []byte) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	configureProcess(cmd)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

type Client struct {
	Binary      string
	Runner      ProcessRunner
	Diagnostics io.Writer
}

func (c Client) Executable() string { return c.Binary }

type RemoteError struct {
	Code    string
	Message string
}

func (e *RemoteError) Error() string {
	if e == nil {
		return "companion failed"
	}
	if e.Code == "" {
		return e.Message
	}
	return fmt.Sprintf("companion %s: %s", e.Code, e.Message)
}

func (c Client) Call(ctx context.Context, request Request) (Response, error) {
	if strings.TrimSpace(c.Binary) == "" {
		return Response{}, errors.New("Windows companion path is empty")
	}
	if c.Runner == nil {
		c.Runner = ExecRunner{}
	}
	request.Magic = ProtocolMagic
	request.ProtocolVersion = ProtocolVersion
	var input bytes.Buffer
	if err := json.NewEncoder(&input).Encode(request); err != nil {
		return Response{}, err
	}

	stdout, stderr, runErr := c.run(ctx, input.Bytes())
	if c.Diagnostics != nil && len(stderr) > 0 {
		_, _ = c.Diagnostics.Write(stderr)
	}
	response, decodeErr := decodeResponse(stdout)
	if decodeErr != nil {
		if runErr != nil {
			return Response{}, processError(runErr, stderr, decodeErr)
		}
		return Response{}, fmt.Errorf("invalid Windows companion response: %w", decodeErr)
	}
	if response.Magic != ProtocolMagic {
		return Response{}, fmt.Errorf("invalid Windows companion protocol magic %q", response.Magic)
	}
	if response.ProtocolVersion != ProtocolVersion {
		return Response{}, fmt.Errorf(
			"incompatible Windows companion protocol: WSL requires %d, Windows returned %d; update from this WSL shell with: npm install -g gohere@latest",
			ProtocolVersion,
			response.ProtocolVersion,
		)
	}
	if !response.OK {
		if response.Error == nil {
			return Response{}, errors.New("Windows companion returned an unspecified error")
		}
		return Response{}, &RemoteError{Code: response.Error.Code, Message: response.Error.Message}
	}
	if runErr != nil {
		return Response{}, processError(runErr, stderr, nil)
	}
	return response, nil
}

func (c Client) run(ctx context.Context, stdin []byte) ([]byte, []byte, error) {
	var stdout, stderr []byte
	var runErr error
	for attempt := 1; attempt <= wslInteropMaxAttempts; attempt++ {
		stdout, stderr, runErr = c.Runner.Run(ctx, c.Binary, []string{InternalCommand}, stdin)
		if !transientWSLInteropLaunchFailure(stdout, stderr, runErr) || attempt == wslInteropMaxAttempts {
			return stdout, stderr, runErr
		}
		timer := time.NewTimer(wslInteropRetryDelay * time.Duration(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil, ctx.Err()
		case <-timer.C:
		}
	}
	return stdout, stderr, runErr
}

func transientWSLInteropLaunchFailure(stdout, stderr []byte, runErr error) bool {
	if runErr == nil || len(bytes.TrimSpace(stdout)) != 0 {
		return false
	}
	detail := string(stderr)
	return strings.Contains(detail, "UtilAcceptVsock") &&
		strings.Contains(detail, "accept4 failed 110")
}

func (c Client) Info(ctx context.Context) (Info, error) {
	return c.info(ctx, OperationInfo)
}

func (c Client) ReadyInfo(ctx context.Context) (Info, error) {
	return c.info(ctx, OperationReadyInfo)
}

func (c Client) info(ctx context.Context, operation Operation) (Info, error) {
	response, err := c.Call(ctx, Request{Operation: operation})
	if err != nil {
		return Info{}, err
	}
	if response.Info == nil {
		return Info{}, errors.New("Windows companion info response is missing info")
	}
	return *response.Info, nil
}

func (c Client) Bootstrap(ctx context.Context, enableHTTPS bool) (string, error) {
	response, err := c.Call(ctx, Request{Operation: OperationBootstrap, EnableHTTPS: enableHTTPS})
	return response.Output, err
}

func (c Client) CACertificate(ctx context.Context) (string, error) {
	response, err := c.Call(ctx, Request{Operation: OperationCACertificate})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(response.CACertificate) == "" {
		return "", errors.New("Windows companion CA response is missing the certificate")
	}
	return response.CACertificate, nil
}

func (c Client) EnsureRouter(ctx context.Context) error {
	_, err := c.Call(ctx, Request{Operation: OperationEnsureRouter})
	return err
}

func (c Client) Health(ctx context.Context) error {
	_, err := c.Call(ctx, Request{Operation: OperationHealth})
	return err
}

func (c Client) Routes(ctx context.Context) ([]router.Route, error) {
	response, err := c.Call(ctx, Request{Operation: OperationRoutes})
	return response.Routes, err
}

func (c Client) RouteStatuses(ctx context.Context) ([]router.RouteStatus, error) {
	response, err := c.Call(ctx, Request{Operation: OperationRouteStatuses})
	return response.RouteStatuses, err
}

func (c Client) Doctor(ctx context.Context) (string, error) {
	response, err := c.Call(ctx, Request{Operation: OperationDoctor})
	return response.Output, err
}

func (c Client) Uninstall(ctx context.Context, removeState bool) (string, error) {
	response, err := c.Call(ctx, Request{Operation: OperationUninstall, RemoveState: removeState})
	return response.Output, err
}

func (c Client) StopRouter(ctx context.Context) (string, error) {
	response, err := c.Call(ctx, Request{Operation: OperationStopRouter})
	return response.Output, err
}

func (c Client) UpsertRoute(ctx context.Context, route router.Route) error {
	_, err := c.Call(ctx, Request{Operation: OperationUpsertRoute, Route: &route})
	return err
}

func (c Client) DeleteRoute(ctx context.Context, host string) error {
	_, err := c.Call(ctx, Request{Operation: OperationDeleteRoute, Host: host})
	return err
}

func (c Client) ProbeTarget(ctx context.Context, target string) (bool, error) {
	response, err := c.Call(ctx, Request{Operation: OperationProbeTarget, Target: target})
	if err != nil {
		return false, err
	}
	if response.Reachable == nil {
		return false, errors.New("Windows companion probe response is missing reachable")
	}
	return *response.Reachable, nil
}

func decodeResponse(data []byte) (Response, error) {
	if len(data) > maxMessageBytes {
		return Response{}, fmt.Errorf("companion response exceeds %d bytes", maxMessageBytes)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return Response{}, errors.New("companion response is empty")
	}
	var response Response
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&response); err != nil {
		return Response{}, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Response{}, errors.New("companion response must contain one JSON object")
	}
	return response, nil
}

func processError(runErr error, stderr []byte, decodeErr error) error {
	detail := strings.TrimSpace(string(stderr))
	message := fmt.Sprintf("Windows companion process failed: %v", runErr)
	if detail != "" {
		message += ": " + detail
	}
	if decodeErr != nil {
		message += fmt.Sprintf(" (response error: %v)", decodeErr)
	}
	return errors.New(message)
}

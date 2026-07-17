package companion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/roie/gohere/internal/router"
)

func TestClientInfoUsesOneShotInternalCommand(t *testing.T) {
	runner := &recordingProcessRunner{stdout: responseJSON(t, Response{
		ProtocolVersion: ProtocolVersion,
		OK:              true,
		Info:            &Info{CompanionVersion: "2.0.0", Platform: "windows"},
	})}
	client := Client{Binary: "/mnt/c/Temp/gohere.exe", Runner: runner}

	info, err := client.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if info.Platform != "windows" || info.CompanionVersion != "2.0.0" {
		t.Fatalf("info = %#v", info)
	}
	if runner.binary != client.Binary || !reflect.DeepEqual(runner.args, []string{InternalCommand}) {
		t.Fatalf("command = %q %#v", runner.binary, runner.args)
	}
	var request Request
	if err := json.Unmarshal(runner.stdin, &request); err != nil {
		t.Fatal(err)
	}
	if request.ProtocolVersion != ProtocolVersion || request.Operation != OperationInfo {
		t.Fatalf("request = %#v", request)
	}
	if request.Magic != ProtocolMagic {
		t.Fatalf("request magic = %q", request.Magic)
	}
}

func TestClientReadyInfoUsesOneShotInternalCommand(t *testing.T) {
	runner := &recordingProcessRunner{stdout: responseJSON(t, Response{
		ProtocolVersion: ProtocolVersion,
		OK:              true,
		Info:            &Info{CompanionVersion: "2.0.0", Platform: "windows", RouterReady: true},
	})}
	client := Client{Binary: "/mnt/c/Temp/gohere.exe", Runner: runner}

	info, err := client.ReadyInfo(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !info.RouterReady {
		t.Fatalf("info = %#v", info)
	}
	var request Request
	if err := json.Unmarshal(runner.stdin, &request); err != nil {
		t.Fatal(err)
	}
	if request.Operation != OperationReadyInfo {
		t.Fatalf("operation = %q", request.Operation)
	}
}

func TestClientRouteLifecycleOperations(t *testing.T) {
	reservation := router.ReservationResult{RunID: "run-a", Routes: []router.ReservedRoute{{Route: router.Route{ID: "route-1", Generation: 1, State: router.RouteStatePending, PreferredScheme: "http"}}}}
	runner := &sequenceProcessRunner{results: []processResult{
		{stdout: responseJSON(t, Response{ProtocolVersion: ProtocolVersion, OK: true, Info: &Info{Capabilities: []string{CapabilityReserveRoutes}}})},
		{stdout: responseJSON(t, Response{ProtocolVersion: ProtocolVersion, OK: true, Reservation: &reservation})},
	}}
	client := Client{Binary: "gohere.exe", Runner: runner}
	result, err := client.ReserveRoutes(t.Context(), router.ReservationRequest{RunID: "run-a", Routes: []router.RouteReservation{{DesiredHost: "web.localhost", PreferredScheme: "http", Target: "http://127.0.0.1:49001", CWD: "/work/web"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, reservation) {
		t.Fatalf("result = %#v", result)
	}
	var request Request
	if err := json.Unmarshal(runner.stdins[1], &request); err != nil {
		t.Fatal(err)
	}
	if request.Operation != OperationReserveRoutes || request.Reservation == nil || request.Reservation.RunID != "run-a" {
		t.Fatalf("request = %#v", request)
	}
}

func TestClientRouteLifecycleUnsupportedIsActionable(t *testing.T) {
	runner := &sequenceProcessRunner{results: []processResult{
		{stdout: responseJSON(t, Response{ProtocolVersion: ProtocolVersion, OK: true, Info: &Info{}})},
	}}
	client := Client{Binary: "gohere.exe", Runner: runner}
	_, err := client.ReserveRoutes(t.Context(), router.ReservationRequest{RunID: "run-a"})
	if err == nil || !strings.Contains(err.Error(), "npm install -g gohere@latest") {
		t.Fatalf("error = %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("calls = %d, old companion received lifecycle payload", runner.calls)
	}
	var request Request
	if err := json.Unmarshal(runner.stdins[0], &request); err != nil {
		t.Fatal(err)
	}
	if request.Operation != OperationInfo {
		t.Fatalf("operation = %q, want capability preflight", request.Operation)
	}
}

func TestClientReturnsStructuredRemoteError(t *testing.T) {
	runner := &recordingProcessRunner{stdout: responseJSON(t, Response{
		ProtocolVersion: ProtocolVersion,
		OK:              false,
		Error:           &ProtocolError{Code: "authority_error", Message: "router stopped"},
	})}
	client := Client{Binary: "gohere.exe", Runner: runner}

	err := client.Health(t.Context())
	var remote *RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("error = %v, want RemoteError", err)
	}
	if remote.Code != "authority_error" || remote.Message != "router stopped" {
		t.Fatalf("remote error = %#v", remote)
	}
}

func TestClientRejectsProtocolMismatch(t *testing.T) {
	runner := &recordingProcessRunner{stdout: responseJSON(t, Response{ProtocolVersion: 7, OK: true})}
	client := Client{Binary: "gohere.exe", Runner: runner}

	err := client.Health(t.Context())
	if err == nil || !strings.Contains(err.Error(), "WSL requires 1, Windows returned 7") {
		t.Fatalf("error = %v", err)
	}
}

func TestClientReportsProcessAndStderrWhenResponseIsInvalid(t *testing.T) {
	runner := &recordingProcessRunner{
		stdout: []byte("not json"),
		stderr: []byte("Windows loader failed"),
		err:    errors.New("exit status 1"),
	}
	client := Client{Binary: "gohere.exe", Runner: runner}

	err := client.Health(t.Context())
	if err == nil || !strings.Contains(err.Error(), "exit status 1") ||
		!strings.Contains(err.Error(), "Windows loader failed") ||
		!strings.Contains(err.Error(), "response error") {
		t.Fatalf("error = %v", err)
	}
}

func TestClientProbeRequiresReachableResult(t *testing.T) {
	runner := &recordingProcessRunner{stdout: responseJSON(t, Response{ProtocolVersion: ProtocolVersion, OK: true})}
	client := Client{Binary: "gohere.exe", Runner: runner}

	_, err := client.ProbeTarget(t.Context(), "http://172.20.0.2:5173")
	if err == nil || !strings.Contains(err.Error(), "missing reachable") {
		t.Fatalf("error = %v", err)
	}
}

func TestClientKeepsStderrSeparateFromProtocolFrames(t *testing.T) {
	runner := &recordingProcessRunner{
		stdout: responseJSON(t, Response{ProtocolVersion: ProtocolVersion, OK: true}),
		stderr: []byte("Windows diagnostic\n"),
	}
	var diagnostics bytes.Buffer
	client := Client{Binary: "gohere.exe", Runner: runner, Diagnostics: &diagnostics}

	if err := client.Health(t.Context()); err != nil {
		t.Fatal(err)
	}
	if diagnostics.String() != "Windows diagnostic\n" {
		t.Fatalf("diagnostics = %q", diagnostics.String())
	}
}

func TestClientRetriesWSLInteropLaunchTimeout(t *testing.T) {
	runner := &sequenceProcessRunner{results: []processResult{
		{
			stderr: []byte("<3>WSL ERROR: UtilAcceptVsock:273: accept4 failed 110"),
			err:    errors.New("exit status 1"),
		},
		{
			stdout: responseJSON(t, Response{ProtocolVersion: ProtocolVersion, OK: true}),
		},
	}}
	var diagnostics bytes.Buffer
	client := Client{Binary: "gohere.exe", Runner: runner, Diagnostics: &diagnostics}

	if err := client.Health(t.Context()); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 2 {
		t.Fatalf("calls = %d, want 2", runner.calls)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("diagnostics = %q", diagnostics.String())
	}
}

func TestClientDoesNotRetryAmbiguousProcessFailure(t *testing.T) {
	runner := &sequenceProcessRunner{results: []processResult{{
		stderr: []byte("Windows loader failed"),
		err:    errors.New("exit status 1"),
	}}}
	client := Client{Binary: "gohere.exe", Runner: runner}

	err := client.Health(t.Context())
	if err == nil || !strings.Contains(err.Error(), "Windows loader failed") {
		t.Fatalf("error = %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("calls = %d, want 1", runner.calls)
	}
}

func TestClientDoesNotRetryWSLInteropErrorAfterProtocolOutput(t *testing.T) {
	runner := &sequenceProcessRunner{results: []processResult{{
		stdout: []byte("partial response"),
		stderr: []byte("<3>WSL ERROR: UtilAcceptVsock:273: accept4 failed 110"),
		err:    errors.New("exit status 1"),
	}}}
	client := Client{Binary: "gohere.exe", Runner: runner}

	if err := client.Health(t.Context()); err == nil {
		t.Fatal("expected protocol error")
	}
	if runner.calls != 1 {
		t.Fatalf("calls = %d, want 1", runner.calls)
	}
}

func responseJSON(t *testing.T, response Response) []byte {
	t.Helper()
	if response.Magic == "" {
		response.Magic = ProtocolMagic
	}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type recordingProcessRunner struct {
	stdout []byte
	stderr []byte
	err    error
	binary string
	args   []string
	stdin  []byte
}

type processResult struct {
	stdout []byte
	stderr []byte
	err    error
}

type sequenceProcessRunner struct {
	results []processResult
	calls   int
	stdins  [][]byte
}

func (r *sequenceProcessRunner) Run(_ context.Context, _ string, _ []string, stdin []byte) ([]byte, []byte, error) {
	index := r.calls
	r.calls++
	r.stdins = append(r.stdins, append([]byte(nil), stdin...))
	if index >= len(r.results) {
		index = len(r.results) - 1
	}
	result := r.results[index]
	return result.stdout, result.stderr, result.err
}

func (r *recordingProcessRunner) Run(_ context.Context, binary string, args []string, stdin []byte) ([]byte, []byte, error) {
	r.binary = binary
	r.args = append([]string(nil), args...)
	r.stdin = append([]byte(nil), stdin...)
	return r.stdout, r.stderr, r.err
}

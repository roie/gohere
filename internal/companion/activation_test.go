package companion

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/roie/gohere/internal/router"
)

func TestClientActivationCarriesProcessIdentity(t *testing.T) {
	refs := []router.RouteRef{{ID: "route", Generation: 4}}
	processes := []router.RouteProcess{{Ref: refs[0], PID: 123, ProcessIdentity: "linux-root"}}
	runner := &sequenceProcessRunner{results: []processResult{
		{stdout: responseJSON(t, Response{OK: true, ProtocolVersion: ProtocolVersion, Info: &Info{Capabilities: []string{CapabilityActivateRoutes}}})},
		{stdout: responseJSON(t, Response{OK: true, ProtocolVersion: ProtocolVersion, Routes: []router.Route{{ID: "route", Generation: 4, PID: 123, ProcessIdentity: "linux-root"}}})},
	}}
	client := Client{Binary: "gohere.exe", Runner: runner}
	routes, err := client.ActivateRoutes(t.Context(), "run", refs, processes)
	if err != nil {
		t.Fatal(err)
	}
	var request Request
	if err := json.Unmarshal(runner.stdins[1], &request); err != nil {
		t.Fatal(err)
	}
	if request.Operation != OperationActivateRoutes || !reflect.DeepEqual(request.Processes, processes) {
		t.Fatalf("request = %+v", request)
	}
	if len(routes) != 1 || routes[0].PID != 123 || routes[0].ProcessIdentity != "linux-root" {
		t.Fatalf("response lost identity: %+v", routes)
	}
}

func TestClientActivationRejectsOlderCompanion(t *testing.T) {
	runner := &sequenceProcessRunner{results: []processResult{{stdout: responseJSON(t, Response{OK: true, ProtocolVersion: ProtocolVersion, Info: &Info{Capabilities: []string{"control.activate-routes-v2"}}})}}}
	client := Client{Binary: "gohere.exe", Runner: runner}
	_, err := client.ActivateRoutes(t.Context(), "run", []router.RouteRef{{ID: "route", Generation: 1}}, nil)
	if err == nil || runner.calls != 1 {
		t.Fatalf("older companion received activation: err=%v calls=%d", err, runner.calls)
	}
}

func TestServeRejectsOlderActivationOperation(t *testing.T) {
	authority := &testAuthority{}
	response := serveProtocolRequest(t, Request{Operation: "activate-routes-v2", RunID: "run", Refs: []router.RouteRef{{ID: "route", Generation: 1}}}, authority)
	if response.OK || response.Error == nil || response.Error.Code != "unsupported_operation" || authority.runID != "" {
		t.Fatalf("older activation was not rejected: %+v", response)
	}
}

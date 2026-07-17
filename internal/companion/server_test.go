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

func TestServeReturnsAuthorityInfo(t *testing.T) {
	authority := &testAuthority{info: Info{
		CompanionVersion: "1.2.3",
		Platform:         "windows",
		Capabilities:     []string{"control.info"},
	}}

	response := serveRequest(t, `{"magic":"gohere-companion","protocolVersion":1,"operation":"info"}`, authority)
	if !response.OK || response.Info == nil {
		t.Fatalf("response = %#v, want successful info", response)
	}
	if response.Info.Platform != "windows" || response.Info.CompanionVersion != "1.2.3" {
		t.Fatalf("info = %#v", response.Info)
	}
}

func TestServeReturnsReadyAuthorityInfo(t *testing.T) {
	authority := &testAuthority{info: Info{Platform: "windows", RouterReady: true}}
	response := serveRequest(t, `{"magic":"gohere-companion","protocolVersion":1,"operation":"ready-info"}`, authority)
	if !response.OK || response.Info == nil || !response.Info.RouterReady || authority.readyInfoCalls != 1 {
		t.Fatalf("response = %#v, ready info calls = %d", response, authority.readyInfoCalls)
	}
}

func TestServeRejectsInvalidOrUnboundedRequests(t *testing.T) {
	tests := []struct {
		name string
		body string
		code string
	}{
		{name: "empty", body: "", code: "invalid_request"},
		{name: "missing magic", body: `{"protocolVersion":1,"operation":"info"}`, code: "invalid_magic"},
		{name: "wrong magic", body: `{"magic":"not-gohere","protocolVersion":1,"operation":"info"}`, code: "invalid_magic"},
		{name: "unknown field", body: `{"magic":"gohere-companion","protocolVersion":1,"operation":"info","token":"must-not-cross"}`, code: "invalid_request"},
		{name: "multiple objects", body: `{"magic":"gohere-companion","protocolVersion":1,"operation":"info"} {}`, code: "invalid_request"},
		{name: "protocol mismatch", body: `{"magic":"gohere-companion","protocolVersion":99,"operation":"info"}`, code: "incompatible_protocol"},
		{name: "unsupported operation", body: `{"magic":"gohere-companion","protocolVersion":1,"operation":"shell"}`, code: "unsupported_operation"},
		{name: "missing route", body: `{"magic":"gohere-companion","protocolVersion":1,"operation":"upsert-route"}`, code: "invalid_request"},
		{name: "missing host", body: `{"magic":"gohere-companion","protocolVersion":1,"operation":"delete-route"}`, code: "invalid_request"},
		{name: "missing target", body: `{"magic":"gohere-companion","protocolVersion":1,"operation":"probe-target"}`, code: "invalid_request"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := serveRequest(t, test.body, &testAuthority{})
			if response.OK || response.Error == nil || response.Error.Code != test.code {
				t.Fatalf("response = %#v, want error code %q", response, test.code)
			}
		})
	}
}

func TestServeDispatchesRouteLifecycleOperations(t *testing.T) {
	authority := &testAuthority{}
	reservation := router.ReservationRequest{RunID: "run-a", Routes: []router.RouteReservation{{DesiredHost: "web.localhost", Target: "http://127.0.0.1:49001", CWD: "/work/web"}}}
	response := serveProtocolRequest(t, Request{Operation: OperationReserveRoutes, Reservation: &reservation}, authority)
	if !response.OK || response.Reservation == nil || authority.reservation.RunID != "run-a" {
		t.Fatalf("reserve response/authority = %#v/%#v", response, authority)
	}
	refs := []router.RouteRef{{ID: "route-1", Generation: 1}}
	for _, request := range []Request{
		{Operation: OperationActivateRoutes, RunID: "run-a", Refs: refs},
		{Operation: OperationRenewRoutes, RunID: "run-a", Refs: refs},
		{Operation: OperationReleaseRoutes, RunID: "run-a", Refs: refs},
		{Operation: OperationDeleteRouteRef, Ref: &refs[0]},
	} {
		response = serveProtocolRequest(t, request, authority)
		if !response.OK {
			t.Fatalf("%s response = %#v", request.Operation, response)
		}
	}
	if authority.runID != "run-a" || !reflect.DeepEqual(authority.refs, refs) || authority.deletedRef != refs[0] {
		t.Fatalf("authority lifecycle state = %#v", authority)
	}
}

func TestServeRejectsMissingRouteLifecyclePayloads(t *testing.T) {
	for _, operation := range []Operation{OperationReserveRoutes, OperationActivateRoutes, OperationReleaseRoutes, OperationRenewRoutes, OperationDeleteRouteRef} {
		response := serveProtocolRequest(t, Request{Operation: operation}, &testAuthority{})
		if response.OK || response.Error == nil || response.Error.Code != "invalid_request" {
			t.Fatalf("%s response = %#v", operation, response)
		}
	}
}

func TestServeRejectsOversizedRequest(t *testing.T) {
	body := strings.Repeat("x", maxMessageBytes+1)
	response := serveRequest(t, body, &testAuthority{})
	if response.OK || response.Error == nil || response.Error.Code != "invalid_request" {
		t.Fatalf("response = %#v", response)
	}
	if !strings.Contains(response.Error.Message, "exceeds") {
		t.Fatalf("error = %q, want size detail", response.Error.Message)
	}
}

func TestServeDispatchesMutationsAndProbe(t *testing.T) {
	authority := &testAuthority{reachable: true}
	route := router.Route{Host: "web.localhost", PreferredScheme: "http", Target: "http://172.20.0.2:5173", OwnerEnv: "wsl"}
	routeJSON, err := json.Marshal(route)
	if err != nil {
		t.Fatal(err)
	}

	response := serveRequest(t, `{"magic":"gohere-companion","protocolVersion":1,"operation":"upsert-route","route":`+string(routeJSON)+`}`, authority)
	if !response.OK || authority.upserted.Host != route.Host {
		t.Fatalf("response = %#v, upserted = %#v", response, authority.upserted)
	}

	response = serveRequest(t, `{"magic":"gohere-companion","protocolVersion":1,"operation":"delete-route","host":"web.localhost"}`, authority)
	if !response.OK || authority.deleted != route.Host {
		t.Fatalf("response = %#v, deleted = %q", response, authority.deleted)
	}

	response = serveRequest(t, `{"magic":"gohere-companion","protocolVersion":1,"operation":"probe-target","target":"http://172.20.0.2:5173"}`, authority)
	if !response.OK || response.Reachable == nil || !*response.Reachable {
		t.Fatalf("response = %#v", response)
	}
}

func TestServeEncodesAuthorityFailure(t *testing.T) {
	authority := &testAuthority{err: errors.New("router unavailable")}
	response := serveRequest(t, `{"magic":"gohere-companion","protocolVersion":1,"operation":"health"}`, authority)
	if response.OK || response.Error == nil || response.Error.Code != "authority_error" {
		t.Fatalf("response = %#v", response)
	}
	if response.Error.Message != "router unavailable" {
		t.Fatalf("message = %q", response.Error.Message)
	}
}

func FuzzServeCompanionRequestIsBounded(f *testing.F) {
	f.Add([]byte(`{"magic":"gohere-companion","protocolVersion":1,"operation":"info"}`))
	f.Add([]byte(`{"magic":"gohere-companion","protocolVersion":1,"operation":"shell"}`))
	f.Add([]byte("not-json"))
	f.Fuzz(func(t *testing.T, body []byte) {
		var output bytes.Buffer
		if err := Serve(t.Context(), bytes.NewReader(body), &output, &testAuthority{}); err != nil {
			t.Fatal(err)
		}
		if output.Len() > 64*1024 {
			t.Fatalf("response grew to %d bytes", output.Len())
		}
		var response Response
		if err := json.Unmarshal(output.Bytes(), &response); err != nil {
			t.Fatalf("response is not JSON: %v", err)
		}
		if response.Magic != ProtocolMagic || response.ProtocolVersion != ProtocolVersion {
			t.Fatalf("response header = %#v", response)
		}
	})
}

func serveProtocolRequest(t *testing.T, request Request, authority Authority) Response {
	t.Helper()
	request.Magic = ProtocolMagic
	request.ProtocolVersion = ProtocolVersion
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return serveRequest(t, string(body), authority)
}

func serveRequest(t *testing.T, body string, authority Authority) Response {
	t.Helper()
	var output bytes.Buffer
	if err := Serve(t.Context(), strings.NewReader(body), &output, authority); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatalf("decode %q: %v", output.String(), err)
	}
	return response
}

type testAuthority struct {
	info           Info
	routes         []router.Route
	statuses       []router.RouteStatus
	doctor         string
	reachable      bool
	err            error
	upserted       router.Route
	deleted        string
	readyInfoCalls int
	reservation    router.ReservationRequest
	runID          string
	refs           []router.RouteRef
	deletedRef     router.RouteRef
}

func (a *testAuthority) Info(context.Context) (Info, error) {
	return a.info, a.err
}

func (a *testAuthority) ReadyInfo(context.Context) (Info, error) {
	a.readyInfoCalls++
	return a.info, a.err
}

func (a *testAuthority) Bootstrap(context.Context, bool) (string, error) {
	return a.doctor, a.err
}

func (a *testAuthority) CACertificate(context.Context) (string, error) {
	return "test certificate", a.err
}

func (a *testAuthority) EnsureRouter(context.Context) error { return a.err }
func (a *testAuthority) Health(context.Context) error       { return a.err }

func (a *testAuthority) Routes(context.Context) ([]router.Route, error) {
	return append([]router.Route(nil), a.routes...), a.err
}

func (a *testAuthority) RouteStatuses(context.Context) ([]router.RouteStatus, error) {
	return append([]router.RouteStatus(nil), a.statuses...), a.err
}

func (a *testAuthority) Doctor(context.Context) (string, error) {
	return a.doctor, a.err
}

func (a *testAuthority) Uninstall(context.Context, bool) (string, error) {
	return a.doctor, a.err
}

func (a *testAuthority) StopRouter(context.Context) (string, error) {
	return a.doctor, a.err
}

func (a *testAuthority) UpsertRoute(_ context.Context, route router.Route) error {
	a.upserted = route
	return a.err
}

func (a *testAuthority) DeleteRoute(_ context.Context, host string) error {
	a.deleted = host
	return a.err
}

func (a *testAuthority) ProbeTarget(context.Context, string) (bool, error) {
	return a.reachable, a.err
}

func (a *testAuthority) ReserveRoutes(_ context.Context, request router.ReservationRequest) (router.ReservationResult, error) {
	a.reservation = request
	return router.ReservationResult{RunID: request.RunID}, a.err
}

func (a *testAuthority) ActivateRoutes(_ context.Context, runID string, refs []router.RouteRef) ([]router.Route, error) {
	a.runID, a.refs = runID, append([]router.RouteRef(nil), refs...)
	return append([]router.Route(nil), a.routes...), a.err
}

func (a *testAuthority) ReleaseRoutes(_ context.Context, runID string, refs []router.RouteRef) error {
	a.runID, a.refs = runID, append([]router.RouteRef(nil), refs...)
	return a.err
}

func (a *testAuthority) RenewRoutes(_ context.Context, runID string, refs []router.RouteRef) error {
	a.runID, a.refs = runID, append([]router.RouteRef(nil), refs...)
	return a.err
}

func (a *testAuthority) DeleteRouteRef(_ context.Context, ref router.RouteRef) error {
	a.deletedRef = ref
	return a.err
}

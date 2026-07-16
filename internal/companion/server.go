package companion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/roie/gohere/internal/router"
)

type Authority interface {
	Info(context.Context) (Info, error)
	ReadyInfo(context.Context) (Info, error)
	Bootstrap(context.Context, bool) (string, error)
	CACertificate(context.Context) (string, error)
	EnsureRouter(context.Context) error
	Health(context.Context) error
	Routes(context.Context) ([]router.Route, error)
	RouteStatuses(context.Context) ([]router.RouteStatus, error)
	Doctor(context.Context) (string, error)
	Uninstall(context.Context, bool) (string, error)
	StopRouter(context.Context) (string, error)
	UpsertRoute(context.Context, router.Route) error
	DeleteRoute(context.Context, string) error
	ProbeTarget(context.Context, string) (bool, error)
	ReserveRoutes(context.Context, router.ReservationRequest) (router.ReservationResult, error)
	ActivateRoutes(context.Context, string, []router.RouteRef) ([]router.Route, error)
	ReleaseRoutes(context.Context, string, []router.RouteRef) error
	RenewRoutes(context.Context, string, []router.RouteRef) error
	DeleteRouteRef(context.Context, router.RouteRef) error
}

func Serve(ctx context.Context, in io.Reader, out io.Writer, authority Authority) error {
	request, err := decodeRequest(in)
	if err != nil {
		return encodeResponse(out, errorResponse("invalid_request", err))
	}
	if request.Magic != ProtocolMagic {
		return encodeResponse(out, errorResponse(
			"invalid_magic",
			fmt.Errorf("companion protocol magic %q is required", ProtocolMagic),
		))
	}
	if request.ProtocolVersion != ProtocolVersion {
		return encodeResponse(out, errorResponse(
			"incompatible_protocol",
			fmt.Errorf("companion protocol version %d is required; received %d", ProtocolVersion, request.ProtocolVersion),
		))
	}
	if authority == nil {
		return encodeResponse(out, errorResponse("authority_unavailable", errors.New("Windows authority is unavailable")))
	}

	response := dispatch(ctx, authority, request)
	response.ProtocolVersion = ProtocolVersion
	return encodeResponse(out, response)
}

func dispatch(ctx context.Context, authority Authority, request Request) Response {
	if err := ctx.Err(); err != nil {
		return errorResponse("canceled", err)
	}

	switch request.Operation {
	case OperationInfo:
		info, err := authority.Info(ctx)
		if err != nil {
			return authorityError(err)
		}
		return Response{OK: true, Info: &info}
	case OperationReadyInfo:
		info, err := authority.ReadyInfo(ctx)
		if err != nil {
			return authorityError(err)
		}
		return Response{OK: true, Info: &info}
	case OperationBootstrap:
		output, err := authority.Bootstrap(ctx, request.EnableHTTPS)
		if err != nil {
			return authorityError(err)
		}
		return Response{OK: true, Output: output}
	case OperationCACertificate:
		certificate, err := authority.CACertificate(ctx)
		if err != nil {
			return authorityError(err)
		}
		return Response{OK: true, CACertificate: certificate}
	case OperationEnsureRouter:
		if err := authority.EnsureRouter(ctx); err != nil {
			return authorityError(err)
		}
		return Response{OK: true}
	case OperationHealth:
		if err := authority.Health(ctx); err != nil {
			return authorityError(err)
		}
		return Response{OK: true}
	case OperationRoutes:
		routes, err := authority.Routes(ctx)
		if err != nil {
			return authorityError(err)
		}
		return Response{OK: true, Routes: routes}
	case OperationRouteStatuses:
		statuses, err := authority.RouteStatuses(ctx)
		if err != nil {
			return authorityError(err)
		}
		return Response{OK: true, RouteStatuses: statuses}
	case OperationDoctor:
		output, err := authority.Doctor(ctx)
		if err != nil {
			return authorityError(err)
		}
		return Response{OK: true, Output: output}
	case OperationUninstall:
		output, err := authority.Uninstall(ctx, request.RemoveState)
		if err != nil {
			return authorityError(err)
		}
		return Response{OK: true, Output: output}
	case OperationStopRouter:
		output, err := authority.StopRouter(ctx)
		if err != nil {
			return authorityError(err)
		}
		return Response{OK: true, Output: output}
	case OperationUpsertRoute:
		if request.Route == nil {
			return errorResponse("invalid_request", errors.New("upsert-route requires route"))
		}
		if err := authority.UpsertRoute(ctx, *request.Route); err != nil {
			return authorityError(err)
		}
		return Response{OK: true}
	case OperationDeleteRoute:
		if strings.TrimSpace(request.Host) == "" {
			return errorResponse("invalid_request", errors.New("delete-route requires host"))
		}
		if err := authority.DeleteRoute(ctx, request.Host); err != nil {
			return authorityError(err)
		}
		return Response{OK: true}
	case OperationProbeTarget:
		if strings.TrimSpace(request.Target) == "" {
			return errorResponse("invalid_request", errors.New("probe-target requires target"))
		}
		reachable, err := authority.ProbeTarget(ctx, request.Target)
		if err != nil {
			return authorityError(err)
		}
		return Response{OK: true, Reachable: &reachable}
	case OperationReserveRoutes:
		if request.Reservation == nil {
			return errorResponse("invalid_request", errors.New("reserve-routes-v2 requires reservation"))
		}
		result, err := authority.ReserveRoutes(ctx, *request.Reservation)
		if err != nil {
			return authorityError(err)
		}
		return Response{OK: true, Reservation: &result}
	case OperationActivateRoutes:
		if strings.TrimSpace(request.RunID) == "" || len(request.Refs) == 0 {
			return errorResponse("invalid_request", errors.New("activate-routes-v2 requires run ID and refs"))
		}
		routes, err := authority.ActivateRoutes(ctx, request.RunID, request.Refs)
		if err != nil {
			return authorityError(err)
		}
		return Response{OK: true, Routes: routes}
	case OperationReleaseRoutes:
		if strings.TrimSpace(request.RunID) == "" || len(request.Refs) == 0 {
			return errorResponse("invalid_request", errors.New("release-routes-v2 requires run ID and refs"))
		}
		if err := authority.ReleaseRoutes(ctx, request.RunID, request.Refs); err != nil {
			return authorityError(err)
		}
		return Response{OK: true}
	case OperationRenewRoutes:
		if strings.TrimSpace(request.RunID) == "" || len(request.Refs) == 0 {
			return errorResponse("invalid_request", errors.New("renew-routes-v2 requires run ID and refs"))
		}
		if err := authority.RenewRoutes(ctx, request.RunID, request.Refs); err != nil {
			return authorityError(err)
		}
		return Response{OK: true}
	case OperationDeleteRouteRef:
		if request.Ref == nil {
			return errorResponse("invalid_request", errors.New("delete-route-ref-v2 requires ref"))
		}
		if err := authority.DeleteRouteRef(ctx, *request.Ref); err != nil {
			return authorityError(err)
		}
		return Response{OK: true}
	default:
		return errorResponse("unsupported_operation", fmt.Errorf("unsupported companion operation %q", request.Operation))
	}
}

func decodeRequest(in io.Reader) (Request, error) {
	if in == nil {
		return Request{}, errors.New("companion request is missing")
	}
	limited := &io.LimitedReader{R: in, N: maxMessageBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return Request{}, err
	}
	if len(data) > maxMessageBytes {
		return Request{}, fmt.Errorf("companion request exceeds %d bytes", maxMessageBytes)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return Request{}, errors.New("companion request is empty")
	}
	var request Request
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return Request{}, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Request{}, errors.New("companion request must contain one JSON object")
	}
	if request.Operation == "" {
		return Request{}, errors.New("companion operation is required")
	}
	return request, nil
}

func encodeResponse(out io.Writer, response Response) error {
	if out == nil {
		return errors.New("companion response writer is missing")
	}
	response.Magic = ProtocolMagic
	response.ProtocolVersion = ProtocolVersion
	encoder := json.NewEncoder(out)
	return encoder.Encode(response)
}

func authorityError(err error) Response {
	return errorResponse("authority_error", err)
}

func errorResponse(code string, err error) Response {
	message := "unknown companion error"
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		message = err.Error()
	}
	return Response{
		Magic:           ProtocolMagic,
		ProtocolVersion: ProtocolVersion,
		OK:              false,
		Error:           &ProtocolError{Code: code, Message: message},
	}
}

package companion

import (
	"context"
	"errors"
	"fmt"

	"github.com/roie/gohere/internal/router"
)

func (c Client) ReserveRoutes(ctx context.Context, request router.ReservationRequest) (router.ReservationResult, error) {
	if err := c.requireLifecycleCapability(ctx, CapabilityReserveRoutes); err != nil {
		return router.ReservationResult{}, err
	}
	response, err := c.Call(ctx, Request{Operation: OperationReserveRoutes, Reservation: &request})
	if err != nil {
		return router.ReservationResult{}, lifecycleOperationError(err)
	}
	if response.Reservation == nil {
		return router.ReservationResult{}, errors.New("Windows companion omitted route reservation")
	}
	return *response.Reservation, nil
}

func (c Client) ActivateRoutes(ctx context.Context, runID string, refs []router.RouteRef) ([]router.Route, error) {
	if err := c.requireLifecycleCapability(ctx, CapabilityActivateRoutes); err != nil {
		return nil, err
	}
	response, err := c.Call(ctx, Request{Operation: OperationActivateRoutes, RunID: runID, Refs: refs})
	if err != nil {
		return nil, lifecycleOperationError(err)
	}
	return response.Routes, nil
}

func (c Client) ReleaseRoutes(ctx context.Context, runID string, refs []router.RouteRef) error {
	if err := c.requireLifecycleCapability(ctx, CapabilityReleaseRoutes); err != nil {
		return err
	}
	_, err := c.Call(ctx, Request{Operation: OperationReleaseRoutes, RunID: runID, Refs: refs})
	return lifecycleOperationError(err)
}

func (c Client) RenewRoutes(ctx context.Context, runID string, refs []router.RouteRef) error {
	if err := c.requireLifecycleCapability(ctx, CapabilityRenewRoutes); err != nil {
		return err
	}
	_, err := c.Call(ctx, Request{Operation: OperationRenewRoutes, RunID: runID, Refs: refs})
	return lifecycleOperationError(err)
}

func (c Client) DeleteRouteRef(ctx context.Context, ref router.RouteRef) error {
	if err := c.requireLifecycleCapability(ctx, CapabilityDeleteRouteRef); err != nil {
		return err
	}
	_, err := c.Call(ctx, Request{Operation: OperationDeleteRouteRef, Ref: &ref})
	return lifecycleOperationError(err)
}

func (c Client) requireLifecycleCapability(ctx context.Context, capability string) error {
	info, err := c.Info(ctx)
	if err != nil {
		return lifecycleOperationError(err)
	}
	for _, available := range info.Capabilities {
		if available == capability {
			return nil
		}
	}
	return fmt.Errorf("Windows companion does not support %s; update from this WSL shell with: npm install -g gohere@latest", capability)
}

func lifecycleOperationError(err error) error {
	if err == nil {
		return nil
	}
	var remote *RemoteError
	if errors.As(err, &remote) && remote.Code == "unsupported_operation" {
		return fmt.Errorf("Windows companion does not support atomic route lifecycle; update from this WSL shell with: npm install -g gohere@latest: %w", err)
	}
	return err
}

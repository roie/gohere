package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/roie/gohere/internal/router"
)

func (c *Client) ReserveRoutes(ctx context.Context, request router.ReservationRequest) (router.ReservationResult, error) {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(request); err != nil {
		return router.ReservationResult{}, err
	}
	const path = "/v2/route-reservations"
	resp, err := c.doLifecycleRequest(ctx, http.MethodPost, path, &body)
	if err != nil {
		return router.ReservationResult{}, err
	}
	defer resp.Body.Close()
	var result router.ReservationResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return router.ReservationResult{}, err
	}
	return result, nil
}

func (c *Client) ActivateRoutes(ctx context.Context, runID string, refs []router.RouteRef) ([]router.Route, error) {
	path := reservationPath(runID) + "/activate"
	resp, err := c.doRefsRequest(ctx, http.MethodPost, path, refs)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var routes []router.Route
	if err := json.NewDecoder(resp.Body).Decode(&routes); err != nil {
		return nil, err
	}
	return routes, nil
}

func (c *Client) ReleaseRoutes(ctx context.Context, runID string, refs []router.RouteRef) error {
	resp, err := c.doRefsRequest(ctx, http.MethodDelete, reservationPath(runID), refs)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (c *Client) RenewRoutes(ctx context.Context, runID string, refs []router.RouteRef) error {
	resp, err := c.doRefsRequest(ctx, http.MethodPost, reservationPath(runID)+"/renew", refs)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (c *Client) DeleteRouteRef(ctx context.Context, ref router.RouteRef) error {
	path := "/v2/routes/" + url.PathEscape(ref.ID) + "/" + strconv.FormatUint(ref.Generation, 10)
	resp, err := c.doLifecycleRequest(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (c *Client) doRefsRequest(ctx context.Context, method, path string, refs []router.RouteRef) (*http.Response, error) {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(router.RouteRefsRequest{Refs: refs}); err != nil {
		return nil, err
	}
	return c.doLifecycleRequest(ctx, method, path, &body)
}

func (c *Client) doLifecycleRequest(ctx context.Context, method, path string, body *bytes.Buffer) (*http.Response, error) {
	req, err := c.authedRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, ErrUnauthorized
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		err := responseStatusError(method, path, resp)
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

func reservationPath(runID string) string {
	return "/v2/route-reservations/" + url.PathEscape(runID)
}

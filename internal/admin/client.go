package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/roie/gohere/internal/router"
)

var ErrUnauthorized = errors.New("gohere admin API unauthorized")
var ErrRouteStatusesUnsupported = errors.New("gohere admin API route statuses unsupported")

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("router health returned %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(body)) != "gohere-router" {
		return fmt.Errorf("router health returned unexpected body")
	}
	return nil
}

func (c *Client) Routes(ctx context.Context) ([]router.Route, error) {
	req, err := c.authedRequest(ctx, http.MethodGet, "/routes", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return nil, responseStatusError("GET", "/routes", resp)
	}
	var routes []router.Route
	if err := json.NewDecoder(resp.Body).Decode(&routes); err != nil {
		return nil, err
	}
	return routes, nil
}

func (c *Client) RouteStatuses(ctx context.Context) ([]router.RouteStatus, error) {
	req, err := c.authedRequest(ctx, http.MethodGet, "/route-statuses", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil, ErrRouteStatusesUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		return nil, responseStatusError("GET", "/route-statuses", resp)
	}
	var statuses []router.RouteStatus
	if err := json.NewDecoder(resp.Body).Decode(&statuses); err != nil {
		return nil, err
	}
	return statuses, nil
}

func (c *Client) CreateLANShare(ctx context.Context, ref router.RouteRef) (router.LANShareResult, error) {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(ref); err != nil {
		return router.LANShareResult{}, err
	}
	request, err := c.authedRequest(ctx, http.MethodPost, "/v2/lan-shares", &body)
	if err != nil {
		return router.LANShareResult{}, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return router.LANShareResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return router.LANShareResult{}, ErrUnauthorized
	}
	if response.StatusCode != http.StatusOK {
		return router.LANShareResult{}, responseStatusError("POST", "/v2/lan-shares", response)
	}
	var result router.LANShareResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return router.LANShareResult{}, err
	}
	return result, nil
}

func (c *Client) DeleteLANShare(ctx context.Context, ref router.RouteRef) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(ref); err != nil {
		return err
	}
	request, err := c.authedRequest(ctx, http.MethodDelete, "/v2/lan-shares", &body)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if response.StatusCode != http.StatusNoContent {
		return responseStatusError("DELETE", "/v2/lan-shares", response)
	}
	return nil
}

func (c *Client) UpsertRoute(ctx context.Context, route router.Route) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(route); err != nil {
		return err
	}
	req, err := c.authedRequest(ctx, http.MethodPost, "/routes", &body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode != http.StatusNoContent {
		return responseStatusError("POST", "/routes", resp)
	}
	return nil
}

func (c *Client) DeleteRoute(ctx context.Context, host string) error {
	path := "/routes/" + url.PathEscape(host)
	req, err := c.authedRequest(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode != http.StatusNoContent {
		return responseStatusError("DELETE", path, resp)
	}
	return nil
}

func (c *Client) Shutdown(ctx context.Context) error {
	req, err := c.authedRequest(ctx, http.MethodPost, "/shutdown", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("POST /shutdown returned %s", resp.Status)
	}
	return nil
}

func (c *Client) ProbeTarget(ctx context.Context, target string) (bool, error) {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(struct {
		Target string `json:"target"`
	}{Target: target}); err != nil {
		return false, err
	}
	req, err := c.authedRequest(ctx, http.MethodPost, "/probe-target", &body)
	if err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return false, ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("POST /probe-target returned %s", resp.Status)
	}
	var result struct {
		Reachable bool `json:"reachable"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}
	return result.Reachable, nil
}

func (c *Client) authedRequest(ctx context.Context, method, path string, body *bytes.Buffer) (*http.Request, error) {
	var reader interface{ Read([]byte) (int, error) }
	if body != nil {
		reader = body
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Gohere-Token", c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func responseStatusError(method, path string, resp *http.Response) error {
	message := fmt.Sprintf("%s %s returned %s", method, path, resp.Status)
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err == nil {
		detail := strings.TrimSpace(string(body))
		if detail != "" {
			message += ": " + detail
		}
	}
	return errors.New(message)
}

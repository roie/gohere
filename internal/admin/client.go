package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/roie/gohere/internal/router"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 2 * time.Second},
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
	if strings.TrimSpace(string(body)) != "ok" {
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
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /routes returned %s", resp.Status)
	}
	var routes []router.Route
	if err := json.NewDecoder(resp.Body).Decode(&routes); err != nil {
		return nil, err
	}
	return routes, nil
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
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("POST /routes returned %s", resp.Status)
	}
	return nil
}

func (c *Client) DeleteRoute(ctx context.Context, host string) error {
	req, err := c.authedRequest(ctx, http.MethodDelete, "/routes/"+host, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("DELETE /routes/%s returned %s", host, resp.Status)
	}
	return nil
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

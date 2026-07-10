package wsledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/roie/gohere/internal/router"
)

const identityProbeTimeout = 1500 * time.Millisecond

func ProbeRouterIdentity(ctx context.Context, baseURL, expectedInstanceID string) (bool, string, error) {
	expectedInstanceID = strings.TrimSpace(expectedInstanceID)
	if expectedInstanceID == "" {
		return false, "", errors.New("expected router instance ID is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, identityProbeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+router.RouterIdentityPath, nil)
	if err != nil {
		return false, "", err
	}
	request.Host = "gohere-identity.localhost"
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, Timeout: identityProbeTimeout}
	response, err := client.Do(request)
	if err != nil {
		return false, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return false, "", fmt.Errorf("router identity probe returned %s", response.Status)
	}
	var result struct {
		RouterInstanceID string `json:"routerInstanceId"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4096))
	if err := decoder.Decode(&result); err != nil {
		return false, "", err
	}
	observed := strings.TrimSpace(result.RouterInstanceID)
	return observed == expectedInstanceID, observed, nil
}

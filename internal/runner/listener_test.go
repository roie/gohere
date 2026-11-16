package runner

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestListenerAncestry(t *testing.T) {
	for _, test := range []struct {
		name    string
		owners  []int
		parents map[int]int
		owned   bool
		unknown bool
	}{
		{"direct", []int{24}, nil, true, false},
		{"wrapper-descendant", []int{42}, map[int]int{42: 43, 43: 24}, true, false},
		{"mixed-owners", []int{24, 99}, map[int]int{99: 0}, false, false},
		{"outside-tree", []int{os.Getpid()}, nil, false, false},
		{"missing-ancestor", []int{42}, nil, false, true},
		{"cyclic-ancestry", []int{42}, map[int]int{42: 43, 43: 42}, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			owned, err := listenerDescendsFrom(test.owners, test.parents, 24)
			if owned != test.owned || (err != nil) != test.unknown {
				t.Fatalf("owned=%v error=%v", owned, err)
			}
		})
	}
}

func TestListenerOwnershipCancellationIsUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	owned, err := listenerOwned(ctx, "127.0.0.1", 3000, os.Getpid())
	if owned || !errors.Is(err, context.Canceled) {
		t.Fatalf("owned=%v error=%v", owned, err)
	}
}

func TestReadinessRejectsUnrelatedListener(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	for _, mode := range []string{"sleep", "advertise"} {
		t.Run(mode, func(t *testing.T) {
			result, err := Start(t.Context(), Config{
				Command: []string{os.Args[0], "-test.run=^TestHelperProcess$", "--", mode, strconv.Itoa(port)},
				Env:     []string{"GOHERE_HELPER_PROCESS=1"}, ChosenPort: port,
				StartupTimeout: 50 * time.Millisecond,
			})
			if result != nil {
				result.Stop()
			}
			if err == nil || !strings.Contains(err.Error(), "belongs to another process") {
				t.Fatalf("unrelated listener must be rejected explicitly, got %v", err)
			}
		})
	}
}

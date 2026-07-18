package router

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultReservationTTL = 30 * time.Second
	DefaultRouteLeaseTTL  = 90 * time.Second
)

type RouteRefsRequest struct {
	Refs []RouteRef `json:"refs"`
}

func (s *Server) handleRouteReservations(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Path != "/v2/route-reservations" || r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request ReservationRequest
	if err := decodeAdminJSON(r, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	request.TTL = DefaultReservationTTL
	result, err := ReserveRoutes(s.store, request, time.Now().UTC())
	if err != nil {
		writeReservationError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(result)
}

func (s *Server) handleRouteReservation(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, "/v2/route-reservations/")
	parts := strings.Split(suffix, "/")
	if len(parts) < 1 || len(parts) > 2 || parts[0] == "" {
		http.Error(w, "invalid reservation path", http.StatusBadRequest)
		return
	}
	runID, err := url.PathUnescape(parts[0])
	if err != nil || runID == "" {
		http.Error(w, "invalid run ID", http.StatusBadRequest)
		return
	}
	var request RouteRefsRequest
	if err := decodeAdminJSON(r, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if len(parts) == 1 && r.Method == http.MethodDelete {
		if err := ReleaseRoutes(s.store, runID, request.Refs); err != nil {
			writeReservationError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(parts) != 2 || r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch parts[1] {
	case "activate":
		routes, err := ActivateRoutes(s.store, runID, request.Refs, time.Now().UTC(), DefaultRouteLeaseTTL)
		if err != nil {
			writeReservationError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(routes)
	case "renew":
		if err := RenewRoutes(s.store, runID, request.Refs, time.Now().UTC(), DefaultRouteLeaseTTL); err != nil {
			writeReservationError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unknown reservation action", http.StatusNotFound)
	}
}

func (s *Server) handleRouteRef(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/routes/"), "/")
	if len(parts) != 2 {
		http.Error(w, "route ID and generation are required", http.StatusBadRequest)
		return
	}
	id, err := url.PathUnescape(parts[0])
	if err != nil || id == "" {
		http.Error(w, "invalid route ID", http.StatusBadRequest)
		return
	}
	generation, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil || generation == 0 {
		http.Error(w, "invalid route generation", http.StatusBadRequest)
		return
	}
	ref := RouteRef{ID: id, Generation: generation}
	if s.lanManager != nil {
		if err := s.lanManager.Remove(r.Context(), ref); err != nil {
			writeReservationError(w, err)
			return
		}
	}
	if err := DeleteRouteRef(s.store, ref); err != nil {
		writeReservationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeAdminJSON(r *http.Request, destination any) error {
	return json.NewDecoder(io.LimitReader(r.Body, maxAdminBodyBytes)).Decode(destination)
}

func writeReservationError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	var schemeConflict *routeSchemeConflictError
	if errors.Is(err, ErrRouteRefMismatch) ||
		errors.Is(err, ErrReservationConflict) ||
		errors.As(err, &schemeConflict) {
		status = http.StatusConflict
	}
	http.Error(w, err.Error(), status)
}

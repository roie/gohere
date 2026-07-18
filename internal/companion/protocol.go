package companion

import "github.com/roie/gohere/internal/router"

const ProtocolMagic = "gohere-companion"
const ProtocolVersion = 1
const InternalCommand = "__companion"

const maxMessageBytes = 1024 * 1024

const (
	CapabilityReserveRoutes  = "control.reserve-routes-v2"
	CapabilityActivateRoutes = "control.activate-routes-v2"
	CapabilityReleaseRoutes  = "control.release-routes-v2"
	CapabilityRenewRoutes    = "control.renew-routes-v2"
	CapabilityDeleteRouteRef = "control.delete-route-ref-v2"
	CapabilityCreateLANShare = "control.create-lan-share-v1"
	CapabilityDeleteLANShare = "control.delete-lan-share-v1"
)

type Operation string

const (
	OperationInfo           Operation = "info"
	OperationReadyInfo      Operation = "ready-info"
	OperationBootstrap      Operation = "bootstrap"
	OperationCACertificate  Operation = "ca-certificate"
	OperationEnsureRouter   Operation = "ensure-router"
	OperationHealth         Operation = "health"
	OperationRoutes         Operation = "routes"
	OperationRouteStatuses  Operation = "route-statuses"
	OperationDoctor         Operation = "doctor"
	OperationUninstall      Operation = "uninstall"
	OperationStopRouter     Operation = "stop-router"
	OperationUpsertRoute    Operation = "upsert-route"
	OperationDeleteRoute    Operation = "delete-route"
	OperationProbeTarget    Operation = "probe-target"
	OperationReserveRoutes  Operation = "reserve-routes-v2"
	OperationActivateRoutes Operation = "activate-routes-v2"
	OperationReleaseRoutes  Operation = "release-routes-v2"
	OperationRenewRoutes    Operation = "renew-routes-v2"
	OperationDeleteRouteRef Operation = "delete-route-ref-v2"
	OperationCreateLANShare Operation = "create-lan-share-v1"
	OperationDeleteLANShare Operation = "delete-lan-share-v1"
)

type Request struct {
	Magic           string                     `json:"magic"`
	ProtocolVersion int                        `json:"protocolVersion"`
	Operation       Operation                  `json:"operation"`
	Route           *router.Route              `json:"route,omitempty"`
	Host            string                     `json:"host,omitempty"`
	Target          string                     `json:"target,omitempty"`
	EnableHTTPS     bool                       `json:"enableHttps,omitempty"`
	RemoveState     bool                       `json:"removeState,omitempty"`
	Reservation     *router.ReservationRequest `json:"reservation,omitempty"`
	RunID           string                     `json:"runId,omitempty"`
	Refs            []router.RouteRef          `json:"refs,omitempty"`
	Ref             *router.RouteRef           `json:"ref,omitempty"`
}

type Response struct {
	Magic           string                    `json:"magic"`
	ProtocolVersion int                       `json:"protocolVersion"`
	OK              bool                      `json:"ok"`
	Error           *ProtocolError            `json:"error,omitempty"`
	Info            *Info                     `json:"info,omitempty"`
	Routes          []router.Route            `json:"routes,omitempty"`
	RouteStatuses   []router.RouteStatus      `json:"routeStatuses,omitempty"`
	Output          string                    `json:"output,omitempty"`
	CACertificate   string                    `json:"caCertificate,omitempty"`
	Reachable       *bool                     `json:"reachable,omitempty"`
	Reservation     *router.ReservationResult `json:"reservation,omitempty"`
	LANShare        *router.LANShareResult    `json:"lanShare,omitempty"`
}

type ProtocolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Info struct {
	CompanionVersion string     `json:"companionVersion"`
	Platform         string     `json:"platform"`
	Architecture     string     `json:"architecture"`
	User             string     `json:"user,omitempty"`
	UserProfile      string     `json:"userProfile"`
	StateDir         string     `json:"stateDir"`
	RouterReady      bool       `json:"routerReady"`
	RouterInstalled  bool       `json:"routerInstalled"`
	RouterInstanceID string     `json:"routerInstanceId,omitempty"`
	CAFingerprint    string     `json:"caFingerprint,omitempty"`
	Capabilities     []string   `json:"capabilities"`
	Listeners        []Listener `json:"listeners,omitempty"`
}

type Listener struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

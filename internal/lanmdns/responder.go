package lanmdns

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	commandQueueCapacity     = 32
	inboundQueueCapacity     = 64
	outboundQueueCapacity    = 64
	coordinatorQueueCapacity = 16
)

type transportFactory func(Interface) (transport, error)

type responderConfig struct {
	transport        transport
	transportFactory transportFactory
	clock            actorClock
	probeDelay       func(time.Duration) time.Duration
	bypassProbing    bool
}

type Option func(*responderConfig)

func withTransport(value transport) Option {
	return func(cfg *responderConfig) { cfg.transport = value }
}

func withTransportFactory(factory transportFactory) Option {
	return func(cfg *responderConfig) { cfg.transportFactory = factory }
}

func withClock(value actorClock) Option {
	return func(cfg *responderConfig) { cfg.clock = value }
}

func withProbeDelay(delay func(time.Duration) time.Duration) Option {
	return func(cfg *responderConfig) { cfg.probeDelay = delay }
}

func withImmediateRegistration() Option {
	return func(cfg *responderConfig) { cfg.bypassProbing = true }
}

type Responder struct {
	ctx                 context.Context
	cancel              context.CancelFunc
	transport           transport
	coordinator         Coordinator
	iface               Interface
	clock               actorClock
	probeDelay          func(time.Duration) time.Duration
	bypassProbing       bool
	commands            chan any
	inbound             chan packet
	outbound            chan writeRequest
	writeResults        chan writeResult
	coordinatorRequests chan coordinatorRequest
	coordinatorResults  chan coordinatorResult
	workers             sync.WaitGroup
	closeOnce           sync.Once
	closeErr            error
}

func New(parent context.Context, iface Interface, coordinator Coordinator, options ...Option) (*Responder, error) {
	if parent == nil {
		return nil, fmt.Errorf("LAN mDNS parent context is required")
	}
	if err := validateInterface(iface); err != nil {
		return nil, err
	}
	if coordinator == nil {
		return nil, fmt.Errorf("LAN mDNS coordinator is required")
	}
	cfg := responderConfig{
		transportFactory: newPlatformTransport,
		clock:            realClock{},
		probeDelay: func(max time.Duration) time.Duration {
			if max <= 0 {
				return 0
			}
			var value [8]byte
			if _, err := rand.Read(value[:]); err != nil {
				return 0
			}
			var number uint64
			for _, b := range value {
				number = number<<8 | uint64(b)
			}
			return time.Duration(number % uint64(max+1))
		},
	}
	for _, option := range options {
		option(&cfg)
	}
	selectedTransport := cfg.transport
	if selectedTransport == nil {
		var err error
		selectedTransport, err = cfg.transportFactory(iface)
		if err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithCancel(parent)
	responder := &Responder{
		ctx:                 ctx,
		cancel:              cancel,
		transport:           selectedTransport,
		coordinator:         coordinator,
		iface:               iface,
		clock:               cfg.clock,
		probeDelay:          cfg.probeDelay,
		bypassProbing:       cfg.bypassProbing,
		commands:            make(chan any, commandQueueCapacity),
		inbound:             make(chan packet, inboundQueueCapacity),
		outbound:            make(chan writeRequest, outboundQueueCapacity),
		writeResults:        make(chan writeResult, outboundQueueCapacity),
		coordinatorRequests: make(chan coordinatorRequest, coordinatorQueueCapacity),
		coordinatorResults:  make(chan coordinatorResult, coordinatorQueueCapacity),
	}
	responder.workers.Add(4)
	go responder.runActor()
	go responder.runReadPump()
	go responder.runWritePump()
	go responder.runCoordinatorPump()
	return responder, nil
}

func (r *Responder) Register(ctx context.Context, hostname string) (Registration, error) {
	reply := make(chan registerResult, 1)
	command := registerCommand{hostname: hostname, reply: reply}
	select {
	case r.commands <- command:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.ctx.Done():
		return nil, context.Canceled
	}
	select {
	case result := <-reply:
		return result.registration, result.err
	case <-ctx.Done():
		r.cancelRegistration(reply)
		return nil, ctx.Err()
	case <-r.ctx.Done():
		return nil, context.Canceled
	}
}

func (r *Responder) cancelRegistration(reply chan registerResult) {
	select {
	case r.commands <- cancelRegisterCommand{reply: reply}:
	case <-r.ctx.Done():
	}
}

func (r *Responder) release(ctx context.Context, id RegistrationID) error {
	reply := make(chan error, 1)
	command := releaseCommand{id: id, reply: reply}
	select {
	case r.commands <- command:
	case <-ctx.Done():
		return ctx.Err()
	case <-r.ctx.Done():
		return context.Canceled
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-r.ctx.Done():
		return context.Canceled
	}
}

func (r *Responder) enqueueWrite(packet outboundPacket) bool {
	return r.enqueueWriteRequest(writeRequest{packet: packet})
}

func (r *Responder) enqueueShutdownWrite(packet outboundPacket) bool {
	return r.enqueueWriteRequest(writeRequest{packet: packet, shutdown: true})
}

func (r *Responder) enqueueWriteRequest(request writeRequest) bool {
	select {
	case r.outbound <- request:
		return true
	default:
		return false
	}
}

func (r *Responder) fail(error) {
	r.cancel()
}

func (r *Responder) Close() error {
	r.closeOnce.Do(func() {
		if r.ctx.Err() == nil {
			reply := make(chan error, 1)
			select {
			case r.commands <- shutdownCommand{reply: reply}:
				timer := time.NewTimer(time.Second)
				select {
				case err := <-reply:
					r.closeErr = err
				case <-timer.C:
					r.closeErr = fmt.Errorf("LAN mDNS shutdown goodbye budget expired")
				case <-r.ctx.Done():
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-r.ctx.Done():
			}
		}
		r.cancel()
		r.closeErr = errors.Join(r.closeErr, r.transport.Close())
		r.workers.Wait()
	})
	return r.closeErr
}

func newRegistrationID() (RegistrationID, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate LAN mDNS registration ID: %w", err)
	}
	return RegistrationID(hex.EncodeToString(value[:])), nil
}

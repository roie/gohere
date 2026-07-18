package lanmdns

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
)

const (
	commandQueueCapacity = 32
	inboundQueueCapacity = 64
)

type transportFactory func(Interface) (transport, error)

type responderConfig struct {
	transport        transport
	transportFactory transportFactory
}

type Option func(*responderConfig)

func withTransport(value transport) Option {
	return func(cfg *responderConfig) { cfg.transport = value }
}

func withTransportFactory(factory transportFactory) Option {
	return func(cfg *responderConfig) { cfg.transportFactory = factory }
}

type Responder struct {
	ctx         context.Context
	cancel      context.CancelFunc
	transport   transport
	coordinator Coordinator
	commands    chan any
	inbound     chan packet
	workers     sync.WaitGroup
	closeOnce   sync.Once
	closeErr    error
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
	cfg := responderConfig{transportFactory: newPlatformTransport}
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
		ctx:         ctx,
		cancel:      cancel,
		transport:   selectedTransport,
		coordinator: coordinator,
		commands:    make(chan any, commandQueueCapacity),
		inbound:     make(chan packet, inboundQueueCapacity),
	}
	responder.workers.Add(2)
	go responder.runActor()
	go responder.runReadPump()
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
		return nil, ctx.Err()
	case <-r.ctx.Done():
		return nil, context.Canceled
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

func (r *Responder) Close() error {
	r.closeOnce.Do(func() {
		r.cancel()
		r.closeErr = r.transport.Close()
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

package lanmdns

import (
	"context"
	"sync/atomic"
)

type RegistrationID string

type Change struct {
	Registration RegistrationID
	Previous     string
	Proposed     string
}

type Coordinator interface {
	Prepare(context.Context, Change) error
}

type Registration interface {
	ID() RegistrationID
	RequestedHostname() string
	CurrentHostname() string
	Close(context.Context) error
}

type registration struct {
	id        RegistrationID
	requested string
	current   atomic.Value
	owner     *Responder
	closed    atomic.Bool
}

func (r *registration) ID() RegistrationID        { return r.id }
func (r *registration) RequestedHostname() string { return r.requested }
func (r *registration) CurrentHostname() string {
	value, _ := r.current.Load().(string)
	return value
}

func (r *registration) Close(ctx context.Context) error {
	if !r.closed.CompareAndSwap(false, true) {
		return nil
	}
	return r.owner.release(ctx, r.id)
}

type claim struct {
	current string
	refs    map[RegistrationID]*registration
}

type registerCommand struct {
	hostname string
	reply    chan registerResult
}

type registerResult struct {
	registration Registration
	err          error
}

type releaseCommand struct {
	id    RegistrationID
	reply chan error
}

func (r *Responder) runActor() {
	defer r.workers.Done()
	claims := make(map[string]*claim)
	registrations := make(map[RegistrationID]*registration)
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-r.inbound:
			// Protocol packet handling is added after the actor boundary is established.
		case command := <-r.commands:
			switch command := command.(type) {
			case registerCommand:
				hostname, err := canonicalHostname(command.hostname)
				if err != nil {
					command.reply <- registerResult{err: err}
					continue
				}
				id, err := newRegistrationID()
				if err != nil {
					command.reply <- registerResult{err: err}
					continue
				}
				handle := &registration{id: id, requested: hostname, owner: r}
				handle.current.Store(hostname)
				entry := claims[hostname]
				if entry == nil {
					entry = &claim{current: hostname, refs: make(map[RegistrationID]*registration)}
					claims[hostname] = entry
				}
				entry.refs[id] = handle
				registrations[id] = handle
				command.reply <- registerResult{registration: handle}
			case releaseCommand:
				registration := registrations[command.id]
				if registration != nil {
					delete(registrations, command.id)
					entry := claims[registration.requested]
					if entry != nil {
						delete(entry.refs, command.id)
						if len(entry.refs) == 0 {
							delete(claims, registration.requested)
						}
					}
				}
				command.reply <- nil
			}
		}
	}
}

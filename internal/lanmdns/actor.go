package lanmdns

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
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

type claimPhase uint8

const (
	phaseProbeWait claimPhase = iota
	phaseProbing
	phaseObserving
	phaseAwaitingReadiness
	phaseAnnouncing
	phaseActive
)

type pendingRegistration struct {
	handle *registration
	reply  chan registerResult
}

type claim struct {
	requested     string
	candidate     string
	current       string
	phase         claimPhase
	probeCount    int
	deadline      time.Time
	startedAt     time.Time
	lastMulticast time.Time
	conflicts     []time.Time
	generation    uint64
	refs          map[RegistrationID]*registration
	pending       []pendingRegistration
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

type shutdownCommand struct{ reply chan error }

type coordinatorRequest struct {
	requested  string
	generation uint64
	change     Change
}

type coordinatorResult struct {
	requested  string
	generation uint64
	err        error
}

type writeRequest struct {
	packet   outboundPacket
	shutdown bool
}

type writeResult struct {
	err      error
	shutdown bool
}

type scheduledWrite struct {
	requested string
	packet    outboundPacket
	due       time.Time
	multicast bool
}

func (r *Responder) runActor() {
	defer r.workers.Done()
	claims := make(map[string]*claim)
	registrations := make(map[RegistrationID]*registration)
	timer := r.clock.NewTimer(time.Hour)
	if !timer.Stop() {
		select {
		case <-timer.C():
		default:
		}
	}
	var timerC <-chan time.Time
	var scheduled []scheduledWrite
	var shutdownReply chan error
	shutdownWrites := 0

	reschedule := func() {
		var nearest time.Time
		for _, entry := range claims {
			if entry.deadline.IsZero() {
				continue
			}
			if nearest.IsZero() || entry.deadline.Before(nearest) {
				nearest = entry.deadline
			}
		}
		for _, write := range scheduled {
			if nearest.IsZero() || write.due.Before(nearest) {
				nearest = write.due
			}
		}
		if nearest.IsZero() {
			timer.Stop()
			timerC = nil
			return
		}
		delay := nearest.Sub(r.clock.Now())
		if delay < 0 {
			delay = 0
		}
		timer.Reset(delay)
		timerC = timer.C()
	}

	completeClaim := func(entry *claim, err error) {
		for _, pending := range entry.pending {
			var handle Registration
			if err == nil {
				handle = pending.handle
			}
			pending.reply <- registerResult{registration: handle, err: err}
		}
		entry.pending = nil
	}

	for {
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return
		case inbound := <-r.inbound:
			message, err := decodePacket(inbound.Payload)
			if err != nil {
				continue
			}
			now := r.clock.Now()
			for requested, entry := range claims {
				if (entry.phase == phaseAnnouncing || entry.phase == phaseActive) && message.Response {
					if conflictingAResponse(message, entry.current, r.iface.Prefix.Addr()) {
						entry.candidate = entry.current
						entry.phase = phaseProbeWait
						entry.probeCount = 0
						entry.startedAt = now
						entry.conflicts = nil
						entry.generation++
						entry.deadline = now.Add(r.probeDelay(250 * time.Millisecond))
					}
					continue
				}
				if (entry.phase == phaseAnnouncing || entry.phase == phaseActive) && !message.Response {
					if len(message.Question) == 0 || knownAnswerSuppresses(message, entry.current, r.iface.Prefix.Addr()) {
						continue
					}
					var question dns.Question
					matched := false
					for _, candidate := range message.Question {
						if strings.EqualFold(candidate.Name, entry.current) {
							question = candidate
							matched = true
							break
						}
					}
					if !matched {
						continue
					}
					destination, ok := responseDestination(inbound, question, entry.lastMulticast, now, r.iface.Prefix)
					if !ok {
						continue
					}
					legacy := inbound.Source.Port() != 5353
					payload, responseErr := responseMessage(message, entry.current, r.iface.Prefix.Addr(), legacy)
					if responseErr != nil {
						continue
					}
					due := now.Add(queryResponseDelay(message, r.probeDelay))
					multicast := destination == mdnsIPv4AddrPort
					if multicast {
						_, defense := probeOpponentIPv4(message, entry.current)
						rateDue := multicastDue(now, entry.lastMulticast, defense)
						if rateDue.After(due) {
							due = rateDue
						}
					}
					write := scheduledWrite{requested: requested, packet: outboundPacket{Payload: payload, Destination: destination}, due: due, multicast: multicast}
					if !due.After(now) {
						r.enqueueWrite(write.packet)
						if multicast {
							entry.lastMulticast = now
						}
					} else {
						scheduled = append(scheduled, write)
					}
					continue
				}
				if entry.phase != phaseProbing && entry.phase != phaseObserving && entry.phase != phaseAwaitingReadiness {
					continue
				}
				if entry.probeCount == 0 {
					continue
				}
				lost := responseClaimsName(message, entry.candidate)
				if opponent, ok := probeOpponentIPv4(message, entry.candidate); ok {
					lost = compareProbeIPv4(r.iface.Prefix.Addr(), opponent) < 0
				}
				if !lost {
					continue
				}
				entry.conflicts = append(entry.conflicts, now)
				if now.Sub(entry.startedAt) >= probingFailureLimit {
					completeClaim(entry, fmt.Errorf("could not claim LAN hostname after one minute"))
					delete(claims, requested)
					continue
				}
				next, suffixErr := nextConflictHostname(entry.candidate)
				if suffixErr != nil {
					completeClaim(entry, suffixErr)
					delete(claims, requested)
					continue
				}
				entry.candidate = next
				entry.phase = phaseProbeWait
				entry.probeCount = 0
				entry.generation++
				delay := r.probeDelay(250 * time.Millisecond)
				if backoff := conflictBackoff(now, entry.conflicts); backoff > delay {
					delay = backoff
				}
				entry.deadline = now.Add(delay)
			}
			reschedule()
		case result := <-r.writeResults:
			if result.shutdown && shutdownReply != nil {
				shutdownWrites--
				if result.err != nil {
					shutdownReply <- fmt.Errorf("write LAN mDNS goodbye: %w", result.err)
					shutdownReply = nil
				} else if shutdownWrites == 0 {
					shutdownReply <- nil
					shutdownReply = nil
				}
				continue
			}
			if result.err != nil {
				r.fail(fmt.Errorf("write LAN mDNS packet: %w", result.err))
			}
		case result := <-r.coordinatorResults:
			entry := claims[result.requested]
			if entry == nil || entry.generation != result.generation || entry.phase != phaseAwaitingReadiness {
				continue
			}
			if result.err != nil {
				completeClaim(entry, result.err)
				delete(claims, entry.requested)
				continue
			}
			payload, err := announcementMessage(entry.candidate, r.iface.Prefix.Addr(), recordTTL)
			if err != nil || !r.enqueueWrite(outboundPacket{Payload: payload, Destination: mdnsIPv4AddrPort}) {
				if err == nil {
					err = fmt.Errorf("LAN mDNS write queue is full")
				}
				completeClaim(entry, err)
				delete(claims, entry.requested)
				continue
			}
			entry.current = entry.candidate
			entry.lastMulticast = r.clock.Now()
			for _, handle := range entry.refs {
				handle.current.Store(entry.current)
			}
			entry.phase = phaseAnnouncing
			entry.deadline = r.clock.Now().Add(time.Second)
			reschedule()
		case now := <-timerC:
			remaining := scheduled[:0]
			for _, write := range scheduled {
				if write.due.After(now) {
					remaining = append(remaining, write)
					continue
				}
				entry := claims[write.requested]
				if entry == nil || (entry.phase != phaseAnnouncing && entry.phase != phaseActive) {
					continue
				}
				r.enqueueWrite(write.packet)
				if write.multicast {
					entry.lastMulticast = now
				}
			}
			scheduled = remaining
			for _, entry := range claims {
				if entry.deadline.IsZero() || entry.deadline.After(now) {
					continue
				}
				switch entry.phase {
				case phaseProbeWait, phaseProbing:
					payload, err := probeMessage(entry.candidate, r.iface.Prefix.Addr())
					if err != nil || !r.enqueueWrite(outboundPacket{Payload: payload, Destination: mdnsIPv4AddrPort}) {
						if err == nil {
							err = fmt.Errorf("LAN mDNS write queue is full")
						}
						completeClaim(entry, err)
						delete(claims, entry.requested)
						continue
					}
					entry.probeCount++
					entry.deadline = now.Add(250 * time.Millisecond)
					if entry.probeCount == 3 {
						entry.phase = phaseObserving
					} else {
						entry.phase = phaseProbing
					}
				case phaseObserving:
					var registrationID RegistrationID
					for id := range entry.refs {
						registrationID = id
						break
					}
					entry.phase = phaseAwaitingReadiness
					entry.deadline = time.Time{}
					request := coordinatorRequest{
						requested:  entry.requested,
						generation: entry.generation,
						change:     Change{Registration: registrationID, Previous: entry.current, Proposed: entry.candidate},
					}
					select {
					case r.coordinatorRequests <- request:
					default:
						completeClaim(entry, fmt.Errorf("LAN mDNS coordinator queue is full"))
						delete(claims, entry.requested)
					}
				case phaseAnnouncing:
					payload, err := announcementMessage(entry.candidate, r.iface.Prefix.Addr(), recordTTL)
					if err != nil || !r.enqueueWrite(outboundPacket{Payload: payload, Destination: mdnsIPv4AddrPort}) {
						if err == nil {
							err = fmt.Errorf("LAN mDNS write queue is full")
						}
						completeClaim(entry, err)
						delete(claims, entry.requested)
						continue
					}
					entry.lastMulticast = now
					entry.phase = phaseActive
					entry.deadline = time.Time{}
					completeClaim(entry, nil)
				}
			}
			reschedule()
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
				handle.current.Store("")
				entry := claims[hostname]
				if entry == nil {
					now := r.clock.Now()
					entry = &claim{
						requested:  hostname,
						candidate:  hostname,
						phase:      phaseProbeWait,
						startedAt:  now,
						generation: 1,
						refs:       make(map[RegistrationID]*registration),
					}
					entry.deadline = now.Add(r.probeDelay(250 * time.Millisecond))
					claims[hostname] = entry
				}
				entry.refs[id] = handle
				registrations[id] = handle
				if r.bypassProbing || entry.phase == phaseActive {
					if entry.current == "" {
						entry.current = hostname
					}
					handle.current.Store(entry.current)
					command.reply <- registerResult{registration: handle}
				} else {
					entry.pending = append(entry.pending, pendingRegistration{handle: handle, reply: command.reply})
				}
				reschedule()
			case releaseCommand:
				handle := registrations[command.id]
				if handle != nil {
					delete(registrations, command.id)
					entry := claims[handle.requested]
					if entry != nil {
						delete(entry.refs, command.id)
						if len(entry.refs) == 0 {
							if entry.phase == phaseActive || entry.phase == phaseAnnouncing {
								if payload, err := goodbyeMessage(entry.current, r.iface.Prefix.Addr()); err == nil {
									r.enqueueWrite(outboundPacket{Payload: payload, Destination: mdnsIPv4AddrPort})
								}
							}
							delete(claims, handle.requested)
						}
					}
				}
				command.reply <- nil
				reschedule()
			case shutdownCommand:
				shutdownReply = command.reply
				shutdownWrites = 0
				for _, entry := range claims {
					if entry.phase != phaseActive && entry.phase != phaseAnnouncing {
						continue
					}
					payload, err := goodbyeMessage(entry.current, r.iface.Prefix.Addr())
					if err != nil {
						command.reply <- fmt.Errorf("build LAN mDNS goodbye: %w", err)
						shutdownReply = nil
						break
					}
					if !r.enqueueShutdownWrite(outboundPacket{Payload: payload, Destination: mdnsIPv4AddrPort}) {
						command.reply <- fmt.Errorf("LAN mDNS goodbye queue is full")
						shutdownReply = nil
						break
					}
					shutdownWrites++
				}
				if shutdownReply != nil && shutdownWrites == 0 {
					shutdownReply <- nil
					shutdownReply = nil
				}
			}
		}
	}
}

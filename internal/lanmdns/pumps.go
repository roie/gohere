package lanmdns

func (r *Responder) runReadPump() {
	defer r.workers.Done()
	for {
		packet, err := r.transport.ReadPacket(r.ctx)
		if err != nil {
			return
		}
		select {
		case r.inbound <- packet:
		case <-r.ctx.Done():
			return
		}
	}
}

func (r *Responder) runWritePump() {
	defer r.workers.Done()
	for {
		select {
		case <-r.ctx.Done():
			return
		case request := <-r.outbound:
			err := r.transport.WritePacket(r.ctx, request.packet)
			select {
			case r.writeResults <- writeResult{err: err, shutdown: request.shutdown}:
			case <-r.ctx.Done():
				return
			}
		}
	}
}

func (r *Responder) runCoordinatorPump() {
	defer r.workers.Done()
	for {
		select {
		case <-r.ctx.Done():
			return
		case request := <-r.coordinatorRequests:
			err := r.coordinator.Prepare(r.ctx, request.change)
			result := coordinatorResult{requested: request.requested, generation: request.generation, err: err}
			select {
			case r.coordinatorResults <- result:
			case <-r.ctx.Done():
				return
			}
		}
	}
}

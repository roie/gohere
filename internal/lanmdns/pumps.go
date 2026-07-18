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

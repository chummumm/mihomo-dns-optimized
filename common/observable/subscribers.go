package observable

// HasSubscribers is a demand check, not a delivery guarantee. It never changes
// subscription state or blocks while sending to a subscriber.
func (o *Observable[T]) HasSubscribers() bool {
	o.mux.Lock()
	defer o.mux.Unlock()
	return len(o.listener) != 0
}

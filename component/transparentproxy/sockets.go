package transparentproxy

// socketProtection pins socket identities until capture commits. Rollback uses
// duplicated descriptors, so a concurrently closed/reused descriptor can never
// cause a mark to be restored on an unrelated new socket.
type socketProtection struct {
	marks  []uint32
	finish func(bool) error
}

func (p *socketProtection) commit() {
	if p.finish != nil {
		_ = p.finish(true)
		p.finish = nil
	}
}
func (p *socketProtection) rollback() error {
	if p.finish == nil {
		return nil
	}
	err := p.finish(false)
	p.finish = nil
	return err
}

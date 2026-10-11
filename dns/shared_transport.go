package dns

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/component/dnsmessage"
	D "github.com/miekg/dns"
)

const (
	sharedDNSQueryTimeout = 5 * time.Second
	sharedDNSIdleTimeout  = 30 * time.Second
	sharedDNSMaxPending   = 256
	sharedDNSMaxSessions  = 2 // one accepting queries, one draining its old IDs/route
	sharedDNSIDLimit      = 1 << 15
)

// sharedDNSState belongs to one protocol of one configured upstream. Native
// route clients remain independently bounded by nativeTransportCapacity.
type sharedDNSState struct {
	mu     sync.Mutex
	pool   *sharedDNSPool
	closed bool
}

type sharedDNSDial func(context.Context) (net.Conn, error)

type sharedDNSPool struct {
	mu          sync.Mutex
	current     *sharedDNSSession
	sessions    map[*sharedDNSSession]struct{}
	changed     chan struct{}
	lifetime    context.Context
	cancel      context.CancelFunc
	closed      bool
	dialing     bool
	completed   uint64
	dialErr     error
	waiters     int
	udp         bool
	maxPending  int
	idLimit     int
	idleTimeout time.Duration
	ioTimeout   time.Duration
}

type sharedDNSSession struct {
	deliveryMu sync.Mutex // complete already-read frames before a late cancel
	pool       *sharedDNSPool
	key        string
	conn       net.Conn
	packet     net.PacketConn
	pending    map[uint16]*sharedDNSRequest
	used       [1024]uint64 // never clear a bit while this socket remains alive
	usedCount  int
	retired    bool
	writes     chan *sharedDNSRequest
	done       chan struct{}
	ready      chan struct{}
	started    bool
	idle       *time.Timer
}

type sharedDNSRequest struct {
	ctx      context.Context
	id       uint16
	original uint16
	question D.Question
	opcode   int
	wire     []byte
	done     chan struct{}
	response *D.Msg
	err      error
	finished bool // all completion/cancellation decisions use pool.mu
}

type sharedDNSConnectionError struct{ error }

func (e *sharedDNSConnectionError) Unwrap() error { return e.error }

var errSharedDNSBusy = errors.New("shared DNS transport query limit reached")

func newSharedDNSPool(udp bool) *sharedDNSPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &sharedDNSPool{sessions: make(map[*sharedDNSSession]struct{}), changed: make(chan struct{}),
		lifetime: ctx, cancel: cancel, udp: udp, maxPending: sharedDNSMaxPending, idLimit: sharedDNSIDLimit,
		idleTimeout: sharedDNSIdleTimeout, ioTimeout: sharedDNSQueryTimeout}
}

func (s *sharedDNSState) exchange(ctx context.Context, m *D.Msg, key string, udp bool, dial sharedDNSDial) (*D.Msg, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, net.ErrClosed
	}
	if s.pool == nil {
		s.pool = newSharedDNSPool(udp)
	}
	p := s.pool
	s.mu.Unlock()
	// Keep the historical five-second query bound, including queueing, dialing
	// and a possible reconnect. A request deadline never becomes a socket's
	// read/write deadline: its siblings own independent requests on that socket.
	ctx, cancel := context.WithTimeout(ctx, sharedDNSQueryTimeout)
	defer cancel()
	for attempt := 0; ; attempt++ {
		answer, err := p.exchange(ctx, m, key, dial)
		var failure *sharedDNSConnectionError
		if attempt >= 1 || !errors.As(err, &failure) || ctx.Err() != nil {
			return answer, err
		}
	}
}

func (s *sharedDNSState) reset(close bool) {
	s.mu.Lock()
	s.closed = s.closed || close
	p := s.pool
	s.pool = nil
	s.mu.Unlock()
	if p != nil {
		p.close()
	}
}

func (p *sharedDNSPool) signalLocked() { close(p.changed); p.changed = make(chan struct{}) }

func (p *sharedDNSPool) exchange(ctx context.Context, m *D.Msg, key string, dial sharedDNSDial) (*D.Msg, error) {
	s, request, err := p.enqueue(ctx, m, key, dial)
	if err != nil {
		return nil, err
	}
	select {
	case <-request.done:
	case <-ctx.Done():
		// The reader holds deliveryMu only while validating and publishing
		// already-read bytes/errors, never while waiting on network I/O.
		// Preserve a completed response even if pool bookkeeping is busy.
		s.deliveryMu.Lock()
		p.mu.Lock()
		if !request.finished {
			// Removing this waiter never closes the shared transport. The ID
			// remains burned, so a late/duplicate packet cannot satisfy a later
			// request even if it asks exactly the same question.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				s.retired = true
				if p.current == s {
					p.current = nil
				}
			}
			p.finishLocked(s, request, nil, ctx.Err())
		}
		p.mu.Unlock()
		s.deliveryMu.Unlock()
	}
	<-request.done
	return request.response, request.err
}

func (p *sharedDNSPool) enqueue(ctx context.Context, m *D.Msg, key string, dial sharedDNSDial) (*sharedDNSSession, *sharedDNSRequest, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, nil, net.ErrClosed
	}
	if p.waiters >= p.maxPending {
		p.mu.Unlock()
		return nil, nil, errSharedDNSBusy
	}
	p.waiters++
	observed := p.completed
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.waiters--
		p.signalLocked()
		p.mu.Unlock()
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, nil, net.ErrClosed
		}
		if p.completed != observed && p.dialErr != nil {
			err := p.dialErr
			p.mu.Unlock()
			return nil, nil, &sharedDNSConnectionError{err}
		}
		if s := p.current; s != nil && (s.key != key || s.usedCount >= p.idLimit) {
			s.retired = true
			p.current = nil
			if len(s.pending) == 0 {
				p.removeLocked(s, net.ErrClosed)
				p.mu.Unlock()
				abortDNSConn(s.conn)
				continue
			}
		}
		if s := p.current; s != nil && len(s.pending) < p.maxPending && len(s.writes) < cap(s.writes) {
			id, err := s.nextIDLocked()
			if err != nil {
				p.mu.Unlock()
				return nil, nil, err
			}
			message := m.Copy()
			message.Id = id
			wire, err := message.Pack()
			if err != nil {
				p.mu.Unlock()
				return nil, nil, err
			}
			if len(wire) > dnsmessage.MaxSize {
				p.mu.Unlock()
				return nil, nil, errors.New("shared DNS query exceeds the maximum message size")
			}
			request := &sharedDNSRequest{ctx: ctx, id: id, original: m.Id, question: m.Question[0], opcode: m.Opcode,
				wire: wire, done: make(chan struct{})}
			s.pending[id] = request
			if s.idle != nil {
				s.idle.Stop()
				s.idle = nil
			}
			s.writes <- request // capacity was checked under the same mutex
			if !s.started {
				s.started = true
				close(s.ready)
			}
			p.mu.Unlock()
			return s, request, nil
		}
		if p.current == nil && !p.dialing && len(p.sessions) < sharedDNSMaxSessions {
			p.dialing = true
			// The first query may be canceled while another query is waiting
			// for this dial. Construction belongs to the pool's lifetime.
			go p.dial(key, dial, context.WithoutCancel(ctx))
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-changed:
		}
	}
}

func (s *sharedDNSSession) nextIDLocked() (uint16, error) {
	// A shared UDP source port loses per-query port entropy. Choose every ID
	// cryptographically, with no sequential fallback. Retire at half of the
	// 16-bit space to keep rejection sampling bounded in expectation.
	var bytes [2]byte
	for {
		if _, err := rand.Read(bytes[:]); err != nil {
			return 0, err
		}
		id := binary.BigEndian.Uint16(bytes[:])
		word, bit := id/64, uint64(1)<<(id%64)
		if s.used[word]&bit == 0 {
			s.used[word] |= bit
			s.usedCount++
			return id, nil
		}
	}
}

func (p *sharedDNSPool) dial(key string, dial sharedDNSDial, values context.Context) {
	ctx, cancel := context.WithTimeout(values, p.ioTimeout)
	stop := context.AfterFunc(p.lifetime, cancel)
	defer stop()
	if p.lifetime.Err() != nil {
		cancel()
	}
	conn, err := dial(ctx)
	if err == nil {
		err = ctx.Err()
	}
	cancel()
	var packet net.PacketConn
	if err == nil && p.udp {
		packet, _ = conn.(net.PacketConn)
		if packet == nil || conn.RemoteAddr() == nil {
			err = errors.New("shared UDP DNS requires a source-aware packet connection")
		}
	}
	p.mu.Lock()
	p.dialing = false
	p.completed++
	p.dialErr = err
	if p.closed || err != nil {
		p.signalLocked()
		p.mu.Unlock()
		if conn != nil {
			abortDNSConn(conn)
		}
		return
	}
	s := &sharedDNSSession{pool: p, key: key, conn: conn, packet: packet,
		pending: make(map[uint16]*sharedDNSRequest), writes: make(chan *sharedDNSRequest, p.maxPending), done: make(chan struct{}), ready: make(chan struct{})}
	p.sessions[s] = struct{}{}
	p.current = s
	p.idleLocked(s)
	p.signalLocked()
	p.mu.Unlock()
	go s.readLoop()
	go s.writeLoop()
}

func (p *sharedDNSPool) finishLocked(s *sharedDNSSession, r *sharedDNSRequest, answer *D.Msg, err error) {
	if r.finished {
		return
	}
	delete(s.pending, r.id)
	r.finished, r.response, r.err = true, answer, err
	close(r.done)
	p.signalLocked()
	if len(s.pending) == 0 {
		p.idleLocked(s)
	}
}

func (p *sharedDNSPool) idleLocked(s *sharedDNSSession) {
	if s.idle != nil {
		s.idle.Stop()
	}
	delay := p.idleTimeout
	if s.retired {
		delay = 0
	}
	s.idle = time.AfterFunc(delay, func() {
		p.mu.Lock()
		_, active := p.sessions[s]
		if !active || len(s.pending) != 0 {
			p.mu.Unlock()
			return
		}
		p.removeLocked(s, net.ErrClosed)
		p.mu.Unlock()
		abortDNSConn(s.conn)
	})
}

func (p *sharedDNSPool) removeLocked(s *sharedDNSSession, err error) bool {
	if _, ok := p.sessions[s]; !ok {
		return false
	}
	delete(p.sessions, s)
	if p.current == s {
		p.current = nil
	}
	s.retired = true
	close(s.done)
	for _, r := range s.pending {
		p.finishLocked(s, r, nil, err)
	}
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
	p.signalLocked()
	return true
}

func (s *sharedDNSSession) fail(err error) {
	s.deliveryMu.Lock()
	s.failDeliveryLocked(err)
}

func (s *sharedDNSSession) failDeliveryLocked(err error) {
	p := s.pool
	p.mu.Lock()
	removed := p.removeLocked(s, &sharedDNSConnectionError{err})
	p.mu.Unlock()
	s.deliveryMu.Unlock()
	if removed {
		abortDNSConn(s.conn)
	}
}

func (p *sharedDNSPool) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.cancel()
	var connections []net.Conn
	for s := range p.sessions {
		p.removeLocked(s, net.ErrClosed)
		connections = append(connections, s.conn)
	}
	p.signalLocked()
	p.mu.Unlock()
	for _, conn := range connections {
		abortDNSConn(conn)
	}
}

func (s *sharedDNSSession) writeLoop() {
	for {
		select {
		case <-s.done:
			return
		case request := <-s.writes:
			s.pool.mu.Lock()
			_, active := s.pool.sessions[s]
			if active && !request.finished && request.ctx.Err() != nil {
				s.pool.finishLocked(s, request, nil, request.ctx.Err())
			}
			write := active && !request.finished
			s.pool.signalLocked()
			s.pool.mu.Unlock()
			if !active {
				return
			}
			if !write {
				continue
			}
			// Only this writer touches write deadlines. Even a canceled query
			// finishes its entire frame or fails the connection; it must not
			// leave a partial length/payload in a sibling's TCP/TLS stream.
			if err := s.conn.SetWriteDeadline(time.Now().Add(s.pool.ioTimeout)); err != nil {
				s.fail(err)
				return
			}
			wire := request.wire
			if !s.pool.udp {
				frame := make([]byte, len(wire)+2)
				binary.BigEndian.PutUint16(frame, uint16(len(wire)))
				copy(frame[2:], wire)
				wire = frame
			}
			var err error
			if s.pool.udp {
				var n int
				n, err = s.conn.Write(wire)
				if err == nil && n != len(wire) {
					err = io.ErrShortWrite
				}
			} else {
				for len(wire) != 0 && err == nil {
					var n int
					n, err = s.conn.Write(wire)
					if n == 0 && err == nil {
						err = io.ErrShortWrite
					}
					wire = wire[n:]
				}
			}
			if err != nil {
				s.fail(err)
				return
			}
		}
	}
}

func (s *sharedDNSSession) readLoop() {
	// Publish the first pending waiter before reading a new socket. Otherwise
	// an immediately-closing peer could destroy each fresh session before its
	// caller enqueues, causing unbounded reconnects inside a single acquisition.
	select {
	case <-s.ready:
	case <-s.done:
		return
	}
	buffer := make([]byte, dnsmessage.MaxSize)
	for {
		var n int
		var err error
		if s.pool.udp {
			var source net.Addr
			n, source, err = s.packet.ReadFrom(buffer)
			if err == nil && !sameSharedDNSSource(source, s.conn.RemoteAddr()) {
				continue
			}
		} else {
			var length [2]byte
			// An idle persistent connection may wait for its first byte. Once
			// a frame starts, its remaining prefix and payload have one fixed
			// transport budget. New queries cannot keep a partial frame alive.
			if err = s.conn.SetReadDeadline(time.Time{}); err == nil {
				_, err = io.ReadFull(s.conn, length[:1])
			}
			if err == nil {
				err = s.conn.SetReadDeadline(time.Now().Add(s.pool.ioTimeout))
			}
			if err == nil {
				_, err = io.ReadFull(s.conn, length[1:])
			}
			if err == nil {
				n = int(binary.BigEndian.Uint16(length[:]))
				_, err = io.ReadFull(s.conn, buffer[:n])
			}
			// Clear the frame deadline at the next iteration, after publishing
			// this complete reply. A peer can close immediately after replying;
			// a failed cleanup SetReadDeadline must not replace successful I/O.
		}
		s.deliveryMu.Lock()
		if err != nil {
			s.failDeliveryLocked(err)
			return
		}
		answer, err := dnsmessage.Unpack(buffer[:n])
		if err != nil {
			if s.pool.udp {
				s.deliveryMu.Unlock()
				continue
			}
			s.failDeliveryLocked(err)
			return
		}
		s.pool.mu.Lock()
		request := s.pending[answer.Id]
		if request != nil && sharedDNSResponseMatches(answer, request) {
			answer.Id = request.original
			s.pool.finishLocked(s, request, answer, nil)
		}
		s.pool.mu.Unlock()
		s.deliveryMu.Unlock()
	}
}

func sameSharedDNSSource(left, right net.Addr) bool {
	if left == nil || right == nil {
		return false
	}
	parse := func(addr net.Addr) (netip.AddrPort, error) {
		if udp, ok := addr.(*net.UDPAddr); ok {
			value := udp.AddrPort()
			return netip.AddrPortFrom(value.Addr().Unmap(), value.Port()), nil
		}
		value, err := netip.ParseAddrPort(addr.String())
		if err != nil {
			return netip.AddrPort{}, err
		}
		return netip.AddrPortFrom(value.Addr().Unmap(), value.Port()), nil
	}
	l, le := parse(left)
	r, re := parse(right)
	return le == nil && re == nil && l.IsValid() && l == r
}

func sharedDNSResponseMatches(answer *D.Msg, request *sharedDNSRequest) bool {
	if !answer.Response || answer.Opcode != request.opcode {
		return false
	}
	if len(answer.Question) == 0 && answer.Rcode != D.RcodeSuccess && len(answer.Answer)+len(answer.Ns)+len(answer.Extra) == 0 {
		return true // preserve valid header-only error replies
	}
	return len(answer.Question) == 1 && strings.EqualFold(answer.Question[0].Name, request.question.Name) &&
		answer.Question[0].Qtype == request.question.Qtype && answer.Question[0].Qclass == request.question.Qclass
}

func sharedDNSQuery(message *D.Msg) bool {
	if message == nil || message.Response || message.Opcode != D.OpcodeQuery || message.Rcode != D.RcodeSuccess ||
		message.Truncated || len(message.Question) != 1 || len(message.Answer)+len(message.Ns) != 0 || message.IsTsig() != nil {
		return false
	}
	q := message.Question[0]
	if q.Qtype == D.TypeAXFR || q.Qtype == D.TypeIXFR {
		return false
	}
	for _, rr := range message.Extra {
		if rr.Header().Rrtype == D.TypeTSIG || rr.Header().Rrtype == D.TypeSIG {
			return false // rewriting an authenticated message ID changes its MAC
		}
	}
	_, valid := D.IsDomainName(q.Name)
	return valid
}

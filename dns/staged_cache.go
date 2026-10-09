package dns

import (
	D "github.com/miekg/dns"
	"time"
)

// Prepare the existing copy/TTL/OPT policy outside the generation mutex. Only
// applying the resulting single mutation needs the control+cache lock. This
// reuses the existing policy instead of introducing a second TTL algorithm.
type stagedDNSWrite struct {
	key     string
	message *D.Msg
	expires time.Time
	remove  bool
}

func (s *stagedDNSWrite) GetWithExpire(string) (*D.Msg, time.Time, bool) {
	return nil, time.Time{}, false
}
func (s *stagedDNSWrite) SetWithExpire(key string, m *D.Msg, expires time.Time) {
	s.key = key
	s.message = m
	s.expires = expires
}
func (s *stagedDNSWrite) Delete(key string) { s.key = key; s.remove = true }
func (s *stagedDNSWrite) Clear()            {}
func (s *stagedDNSWrite) apply(cache dnsCache) {
	if s.remove {
		cache.Delete(s.key)
	} else if s.message != nil {
		cache.SetWithExpire(s.key, s.message, s.expires)
	}
}

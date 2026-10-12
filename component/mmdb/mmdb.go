package mmdb

import (
	"fmt"
	"sync"

	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"

	"github.com/oschwald/maxminddb-golang"
)

type databaseType = uint8

const (
	typeMaxmind databaseType = iota
	typeSing
	typeMetaV0
)

var (
	ipDatabase  database
	asnDatabase database
)

// The lock covers a complete lookup, not only returning the reader pointer.
// A previously returned IPReader/ASNReader therefore remains safe when a live
// update closes and replaces the mmap before that caller starts its lookup.
type database struct {
	mu     sync.RWMutex
	reader *maxminddb.Reader
}

func (d *database) acquire(path string) (*maxminddb.Reader, func()) {
	for {
		d.mu.RLock()
		if d.reader != nil {
			return d.reader, d.mu.RUnlock
		}
		d.mu.RUnlock()
		d.mu.Lock()
		if d.reader == nil {
			reader, err := maxminddb.Open(path)
			if err != nil {
				d.mu.Unlock()
				log.Fatalln("Can't load database: %s", err.Error())
				return nil, func() {}
			}
			d.reader = reader
		}
		d.mu.Unlock()
	}
}

func (d *database) reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reader != nil {
		_ = d.reader.Close()
		d.reader = nil
	}
}

func ipDatabaseType(reader *maxminddb.Reader) databaseType {
	switch reader.Metadata.DatabaseType {
	case "sing-geoip":
		return typeSing
	case "Meta-geoip0":
		return typeMetaV0
	default:
		return typeMaxmind
	}
}

func LoadFromBytes(buffer []byte) {
	ipDatabase.mu.Lock()
	defer ipDatabase.mu.Unlock()
	if ipDatabase.reader == nil {
		reader, err := maxminddb.FromBytes(buffer)
		if err != nil {
			log.Fatalln("Can't load mmdb: %s", err.Error())
			return
		}
		ipDatabase.reader = reader
	}
}

func Verify(path string) bool {
	instance, err := maxminddb.Open(path)
	if err == nil {
		instance.Close()
	}
	return err == nil
}

func IPInstance() IPReader {
	reader, release := ipDatabase.acquire(C.Path.MMDB())
	defer release()
	return IPReader{Reader: reader, databaseType: ipDatabaseType(reader), managed: &ipDatabase}
}

func ASNInstance() ASNReader {
	reader, release := asnDatabase.acquire(C.Path.ASN())
	defer release()
	return ASNReader{Reader: reader, managed: &asnDatabase}
}

func ReloadIP() {
	ipDatabase.reset()
}

func ReloadASN() {
	asnDatabase.reset()
}

// replace keeps lookups behind the same lock while closing the old mmap and
// publishing a validated replacement. If the atomic file write fails, the old
// bytes keep the live database usable even on Windows, where its mmap had to be
// closed before renaming over the destination.
func (d *database) replace(path string, data, oldData []byte, write func() error) error {
	next, err := maxminddb.FromBytes(data)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var previous *maxminddb.Reader
	if d.reader != nil && oldData != nil {
		previous, err = maxminddb.FromBytes(oldData)
		if err != nil {
			_ = next.Close()
			return fmt.Errorf("cannot preserve current database: %w", err)
		}
		_ = d.reader.Close()
		d.reader = previous
	}
	if err := write(); err != nil {
		_ = next.Close()
		return err
	}
	if d.reader != nil {
		_ = d.reader.Close()
	}
	// Re-open as mmap when available. The already validated in-memory reader is
	// a safe fallback if opening the successfully written file happens to fail.
	if mapped, err := maxminddb.Open(path); err == nil {
		_ = next.Close()
		next = mapped
	}
	d.reader = next
	return nil
}

func ReplaceIP(path string, data, oldData []byte, write func() error) error {
	return ipDatabase.replace(path, data, oldData, write)
}

func ReplaceASN(path string, data, oldData []byte, write func() error) error {
	return asnDatabase.replace(path, data, oldData, write)
}

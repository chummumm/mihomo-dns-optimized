package mmdb

import (
	"encoding/binary"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/oschwald/maxminddb-golang"
)

// A tiny valid database maps the whole IPv4 space to one record. Building the
// fixture locally avoids downloading large vendor databases for race tests.
func testDatabaseBytes(databaseType string, record map[string]any) []byte {
	var encode func(any) []byte
	header := func(typ byte, size int) []byte {
		if size < 29 {
			return []byte{typ<<5 | byte(size)}
		}
		return []byte{typ<<5 | 29, byte(size - 29)}
	}
	encode = func(value any) []byte {
		switch value := value.(type) {
		case string:
			return append(header(2, len(value)), []byte(value)...)
		case uint32:
			data := []byte{6<<5 | 4, 0, 0, 0, 0}
			binary.BigEndian.PutUint32(data[1:], value)
			return data
		case map[string]any:
			data := header(7, len(value))
			keys := make([]string, 0, len(value))
			for key := range value {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				data = append(data, encode(key)...)
				data = append(data, encode(value[key])...)
			}
			return data
		default:
			panic("unsupported test metadata")
		}
	}
	data := []byte{0, 0, 17, 0, 0, 17}
	data = append(data, make([]byte, 16)...)
	data = append(data, encode(record)...)
	data = append(data, []byte("\xab\xcd\xefMaxMind.com")...)
	return append(data, encode(map[string]any{
		"database_type": databaseType, "binary_format_major_version": uint32(2),
		"binary_format_minor_version": uint32(0), "build_epoch": uint32(1),
		"ip_version": uint32(4), "node_count": uint32(1), "record_size": uint32(24),
	})...)
}

func testIPDatabaseBytes(country string) []byte {
	return testDatabaseBytes("GeoIP2-Country", map[string]any{"country": map[string]any{"iso_code": country}})
}

func TestDatabaseUpdateKeepsPreviouslyReturnedReaderUsable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "geoip.mmdb")
	old, next := testIPDatabaseBytes("CN"), testIPDatabaseBytes("US")
	if err := os.WriteFile(path, old, 0o644); err != nil {
		t.Fatal(err)
	}
	reader, err := maxminddb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	state := &database{reader: reader}
	defer state.reset()
	retained := IPReader{Reader: reader, databaseType: typeMaxmind, managed: state}
	if err := state.replace(path, next, old, func() error { return os.WriteFile(path, next, 0o644) }); err != nil {
		t.Fatal(err)
	}
	got := retained.LookupCode(net.ParseIP("1.2.3.4"))
	if len(got) != 1 || got[0] != "us" {
		t.Fatalf("retained handle read an old/closed database: %v", got)
	}
	failed := errors.New("cannot replace destination")
	if err := state.replace(path, old, next, func() error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("write failure = %v", err)
	}
	got = retained.LookupCode(net.ParseIP("1.2.3.4"))
	if len(got) != 1 || got[0] != "us" {
		t.Fatalf("failed update left the live database unusable: %v", got)
	}
}

func TestDatabaseUpdateDoesNotCloseReaderDuringLookup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "geoip.mmdb")
	old, next := testIPDatabaseBytes("CN"), testIPDatabaseBytes("US")
	if err := os.WriteFile(path, old, 0o644); err != nil {
		t.Fatal(err)
	}
	reader, err := maxminddb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	state := &database{reader: reader}
	defer state.reset()
	retained := IPReader{Reader: reader, databaseType: typeMaxmind, managed: state}
	stop := make(chan struct{})
	var queries atomic.Int32
	lookup := func() bool {
		got := retained.LookupCode(net.ParseIP("1.2.3.4"))
		if len(got) != 1 || (got[0] != "cn" && got[0] != "us") {
			t.Errorf("lookup observed closed or partial data: %v", got)
			return false
		}
		queries.Add(1)
		return true
	}
	var readers, started sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		started.Add(1)
		go func() {
			defer readers.Done()
			ok := lookup()
			started.Done()
			if !ok {
				return
			}
			for {
				select {
				case <-stop:
					return
				default:
					if !lookup() {
						return
					}
				}
			}
		}()
	}
	// A fast writer may otherwise finish before the reader goroutines are
	// scheduled, especially on Windows. Start only after every reader has
	// performed its first real lookup; no sleep or scheduler guess is needed.
	started.Wait()
	current := old
	for i := 0; i < 20; i++ {
		data := next
		if i%2 != 0 {
			data = old
		}
		if err := state.replace(path, data, current, func() error { return os.WriteFile(path, data, 0o644) }); err != nil {
			t.Error(err)
			break
		}
		current = data
	}
	close(stop)
	readers.Wait()
	if queries.Load() == 0 {
		t.Fatal("lookup/update race fixture did not perform any lookup")
	}
}

func TestASNUpdateKeepsRetainedHandleCurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asn.mmdb")
	makeASN := func(number uint32) []byte {
		return testDatabaseBytes("GeoLite2-ASN", map[string]any{"autonomous_system_number": number, "autonomous_system_organization": "Example"})
	}
	old, next := makeASN(64512), makeASN(64513)
	if err := os.WriteFile(path, old, 0o644); err != nil {
		t.Fatal(err)
	}
	reader, err := maxminddb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	state := &database{reader: reader}
	defer state.reset()
	retained := ASNReader{Reader: reader, managed: state}
	if err := state.replace(path, next, old, func() error { return os.WriteFile(path, next, 0o644) }); err != nil {
		t.Fatal(err)
	}
	asn, name := retained.LookupASN(net.ParseIP("1.2.3.4"))
	if asn != "64513" || name != "Example" {
		t.Fatalf("ASN update was not active through retained handle: %s %s", asn, name)
	}
}

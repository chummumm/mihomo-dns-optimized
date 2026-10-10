package statistic

import (
	"encoding/json"
	"net/netip"
	"reflect"
	"sync"
	"testing"

	C "github.com/metacubex/mihomo/constant"
)

type dnsTrackerTestTCP struct{ C.Conn }

func (dnsTrackerTestTCP) Chains() C.Chain           { return C.Chain{"leaf", "inner-group", "outer-group"} }
func (dnsTrackerTestTCP) ProviderChains() C.Chain   { return C.Chain{"provider", "", ""} }
func (dnsTrackerTestTCP) RemoteDestination() string { return "198.51.100.1:853" }

type dnsTrackerTestUDP struct{ C.PacketConn }

func (dnsTrackerTestUDP) Chains() C.Chain           { return C.Chain{"leaf", "inner-group", "outer-group"} }
func (dnsTrackerTestUDP) ProviderChains() C.Chain   { return C.Chain{"provider", "", ""} }
func (dnsTrackerTestUDP) RemoteDestination() string { return "198.51.100.1:853" }

type dnsTrackerTestRule struct{ C.Rule }

func (dnsTrackerTestRule) RuleType() C.RuleType { return C.Domain }
func (dnsTrackerTestRule) Payload() string      { return "example.test" }

func TestDNSMarkerPreservesConnectionMetadataAndRawRoute(t *testing.T) {
	for _, dns := range []bool{false, true} {
		for _, network := range []C.NetWork{C.TCP, C.UDP} {
			for _, port := range []uint16{53, 853} {
				manager := &Manager{}
				metadata := &C.Metadata{
					Type: C.SOCKS5, NetWork: network, InName: "DNS", Host: "example.test",
					SrcIP: netip.MustParseAddr("192.0.2.1"), SrcPort: 50000,
					DstIP: netip.MustParseAddr("198.51.100.1"), DstPort: port,
				}
				original := metadata.Clone()
				var tracker Tracker
				if network == C.TCP {
					if dns {
						tracker = NewDNSTCPTracker(dnsTrackerTestTCP{}, manager, metadata, dnsTrackerTestRule{}, 11, 17, true)
					} else {
						tracker = NewTCPTracker(dnsTrackerTestTCP{}, manager, metadata, dnsTrackerTestRule{}, 11, 17, true)
					}
				} else if dns {
					tracker = NewDNSUDPTracker(dnsTrackerTestUDP{}, manager, metadata, dnsTrackerTestRule{}, 11, 17, true)
				} else {
					tracker = NewUDPTracker(dnsTrackerTestUDP{}, manager, metadata, dnsTrackerTestRule{}, 11, 17, true)
				}
				info := manager.Get(tracker.ID()).Info()
				if info.DNS != dns {
					t.Fatalf("DNS marker = %v, want %v (network=%v port=%d)", info.DNS, dns, network, port)
				}
				original.RemoteDst = "198.51.100.1:853"
				if !reflect.DeepEqual(info.Metadata, original) {
					t.Fatalf("DNS presentation changed inbound/routing metadata: %#v", info.Metadata)
				}
				if !reflect.DeepEqual(info.Chain, C.Chain{"leaf", "inner-group", "outer-group"}) || info.Rule != "Domain" || info.RulePayload != "example.test" {
					t.Fatalf("raw route was changed: %#v", info)
				}
				if up, down := manager.Total(); up != 11 || down != 17 {
					t.Fatalf("wire-byte accounting changed: %d/%d", up, down)
				}
				wire, err := json.Marshal(info)
				if err != nil {
					t.Fatal(err)
				}
				var value map[string]any
				if err := json.Unmarshal(wire, &value); err != nil {
					t.Fatal(err)
				}
				flag, exists := value["dns"]
				if exists != dns || dns && flag != true {
					t.Fatalf("JSON marker = %v (present=%v), want DNS=%v", flag, exists, dns)
				}
			}
		}
	}
}

func TestDNSMarkerPublishedBeforeConcurrentSnapshots(t *testing.T) {
	manager := &Manager{}
	var readers sync.WaitGroup
	stop := make(chan struct{})
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				manager.Range(func(tracker Tracker) bool {
					if !tracker.Info().DNS {
						t.Error("DNS tracker was published before its marker")
					}
					if _, err := json.Marshal(tracker.Info()); err != nil {
						t.Error(err)
					}
					return true
				})
			}
		}
	}()
	for i := 0; i < 256; i++ {
		tcp := NewDNSTCPTracker(dnsTrackerTestTCP{}, manager, &C.Metadata{}, nil, 0, 0, false)
		udp := NewDNSUDPTracker(dnsTrackerTestUDP{}, manager, &C.Metadata{}, nil, 0, 0, false)
		manager.Leave(tcp)
		manager.Leave(udp)
	}
	close(stop)
	readers.Wait()
}

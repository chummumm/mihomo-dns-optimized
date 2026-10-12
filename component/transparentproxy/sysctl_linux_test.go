//go:build linux

package transparentproxy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/exp/slices"
)

func sysctlTestSystem(t *testing.T) (*linuxRouteSystem, *routeTestKernel, networkState, Config, Runtime, map[string]string, *int) {
	t.Helper()
	s, kernel, writes := routeTestSystem(t)
	kernel.links = []networkInterface{{Index: 1, Name: "lo", Up: true, Loopback: true}, {Index: 2, Name: "eth0", Up: true}, {Index: 3, Name: "lan0", Up: true}, {Index: 4, Name: "spare0", Up: true}, {Index: 10, Name: "utun", Up: true}}
	state := networkState{Interfaces: slices.Clone(kernel.links), DefaultIPv4: "eth0", DefaultIPv6: "eth0", IPv6Available: true}
	cfg := DefaultConfig()
	cfg.Enable, cfg.Gateway.Enable = true, true
	cfg.Local.Interfaces = []string{"eth0"}
	cfg.Gateway.Interfaces = []string{"lan0"}
	runtime := Runtime{IPv6: true, TunName: "utun", TunIndex: 10, TunReady: true}
	originals := map[string]string{
		"net/ipv4/ip_forward": "0", "net/ipv4/conf/all/accept_redirects": "1",
		"net/ipv4/conf/all/rp_filter": "1", "net/ipv4/conf/default/rp_filter": "0",
		"net/ipv6/conf/all/disable_ipv6": "0", "net/ipv6/conf/all/forwarding": "0",
		"net/ipv6/conf/default/forwarding": "0", "net/ipv6/conf/default/accept_ra": "1",
	}
	for _, link := range state.Interfaces {
		originals["net/ipv4/conf/"+link.Name+"/rp_filter"] = "0"
		originals["net/ipv4/conf/"+link.Name+"/forwarding"] = "0"
		originals["net/ipv6/conf/"+link.Name+"/forwarding"] = "0"
		originals["net/ipv6/conf/"+link.Name+"/accept_ra"] = "1"
		originals["net/ipv6/conf/"+link.Name+"/force_forwarding"] = "0"
	}
	originals["net/ipv4/conf/spare0/rp_filter"] = "2"
	originals["net/ipv6/conf/eth0/forwarding"] = "1"
	originals["net/ipv6/conf/eth0/force_forwarding"] = "1"
	originals["net/ipv6/conf/lan0/accept_ra"] = "0"
	for path, value := range originals {
		writeTestSysctl(t, s, path, value)
	}
	return s, kernel, state, cfg, runtime, originals, writes
}

func assertTestSysctl(t *testing.T, s *linuxRouteSystem, path, expected string) {
	t.Helper()
	actual, err := s.readSysctl(path)
	if err != nil || actual != expected {
		t.Fatalf("%s=%q, want %q (err %v)", path, actual, expected, err)
	}
}

func TestSysctlsScopeIdempotenceAndRestore(t *testing.T) {
	s, _, state, cfg, runtime, originals, writes := sysctlTestSystem(t)
	if err := s.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
		t.Fatal(err)
	}
	for path, expected := range map[string]string{
		"net/ipv4/ip_forward": "0", "net/ipv4/conf/all/accept_redirects": "1",
		"net/ipv4/conf/lan0/forwarding": "1", "net/ipv4/conf/utun/forwarding": "1",
		"net/ipv4/conf/eth0/forwarding": "0", "net/ipv4/conf/spare0/forwarding": "0",
		"net/ipv4/conf/all/rp_filter": "0", "net/ipv4/conf/lo/rp_filter": "0",
		"net/ipv4/conf/eth0/rp_filter": "1", "net/ipv4/conf/spare0/rp_filter": "2",
		"net/ipv6/conf/all/forwarding": "1", "net/ipv6/conf/eth0/accept_ra": "2",
		"net/ipv6/conf/lan0/accept_ra": "0", "net/ipv6/conf/eth0/force_forwarding": "1",
	} {
		assertTestSysctl(t, s, path, expected)
	}
	for _, owned := range s.manifest.Sysctls {
		if owned.Original != originals[owned.Path] {
			t.Fatalf("journal lost original before a bulk update: %+v want %s", owned, originals[owned.Path])
		}
		if strings.HasSuffix(owned.Path, "disable_ipv6") {
			t.Fatal("administrator IPv6 switch became owned")
		}
	}
	before := *writes
	if err := s.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
		t.Fatal(err)
	}
	if *writes != before {
		t.Fatalf("unchanged event rewrote sysctl journal: %d -> %d", before, *writes)
	}
	if err := s.RestoreSysctls(context.Background()); err != nil {
		t.Fatal(err)
	}
	for path, expected := range originals {
		assertTestSysctl(t, s, path, expected)
	}
	if len(s.manifest.Sysctls) != 0 {
		t.Fatalf("cleanup left owned settings: %+v", s.manifest.Sysctls)
	}
}

func TestSysctlsLeaveExternalChangesAndReusedInterfaceAlone(t *testing.T) {
	s, kernel, state, cfg, runtime, _, _ := sysctlTestSystem(t)
	runtime.IPv6 = false
	if err := s.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
		t.Fatal(err)
	}
	writeTestSysctl(t, s, "net/ipv4/conf/lan0/rp_filter", "2")
	for i := range kernel.links {
		if kernel.links[i].Name == "utun" {
			kernel.links[i].Index = 20
		}
	}
	// A replacement device is configured independently by another owner.
	writeTestSysctl(t, s, "net/ipv4/conf/utun/forwarding", "1")
	writeTestSysctl(t, s, "net/ipv4/conf/utun/rp_filter", "2")
	if err := s.RestoreSysctls(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertTestSysctl(t, s, "net/ipv4/conf/lan0/rp_filter", "2")
	assertTestSysctl(t, s, "net/ipv4/conf/utun/forwarding", "1")
	assertTestSysctl(t, s, "net/ipv4/conf/utun/rp_filter", "2")
	if len(s.manifest.Sysctls) != 0 {
		t.Fatal("external changes left a stale ownership claim")
	}
}

func TestSysctlWriteRequiresDurableOriginal(t *testing.T) {
	s, _, _ := routeTestSystem(t)
	path := "net/ipv4/conf/utun/forwarding"
	writeTestSysctl(t, s, path, "0")
	s.writeState = func() error { return os.ErrPermission }
	if err := s.ownSysctl(desiredSysctl{Path: path, Value: "1", InterfaceIndex: 10}); !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	assertTestSysctl(t, s, path, "0")
	if len(s.manifest.Sysctls) != 0 {
		t.Fatal("failed journal retained ownership")
	}
	if err := s.writeSysctl("net/ipv4/conf/missing/forwarding", "1"); !os.IsNotExist(err) {
		t.Fatalf("missing proc setting: %v", err)
	}
}

func TestSysctlsRetiredGatewayScopeRestoresSelectedInterface(t *testing.T) {
	s, _, state, cfg, runtime, _, _ := sysctlTestSystem(t)
	runtime.IPv6 = false
	// Use a nonzero interface original to distinguish restoration from simply
	// continuing to leave the formerly captured interface unrestricted.
	writeTestSysctl(t, s, "net/ipv4/conf/lan0/rp_filter", "2")
	if err := s.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
		t.Fatal(err)
	}
	assertTestSysctl(t, s, "net/ipv4/conf/lan0/rp_filter", "0")
	cfg.Gateway.Interfaces = []string{"eth0"}
	if err := s.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
		t.Fatal(err)
	}
	assertTestSysctl(t, s, "net/ipv4/conf/lan0/rp_filter", "2")
	assertTestSysctl(t, s, "net/ipv4/conf/lan0/forwarding", "0")
	assertTestSysctl(t, s, "net/ipv4/conf/eth0/forwarding", "1")
}

func TestSysctlJournalSurvivesProcessCloseAndReexec(t *testing.T) {
	s, kernel, state, cfg, runtime, originals, _ := sysctlTestSystem(t)
	s.statePath, s.writeState = filepath.Join(t.TempDir(), "state.json"), nil
	if err := s.loadManifest(); err != nil {
		t.Fatal(err)
	}
	owner := s.manifest.Owner
	if err := s.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Closing the transport/lease for planned reexec does not restore sysctls.
	assertTestSysctl(t, s, "net/ipv6/conf/all/forwarding", "1")
	after, err := os.ReadFile(s.statePath)
	if err != nil || string(before) != string(after) {
		t.Fatalf("transport close changed journal: %v", err)
	}
	next := &linuxRouteSystem{request: testWireConnection(kernel.handle), statePath: s.statePath, procRoot: s.procRoot, lease: -1}
	defer next.Close()
	if err := next.loadManifest(); err != nil {
		t.Fatal(err)
	}
	if next.manifest.Owner != owner {
		t.Fatal("same-namespace reexec changed durable owner")
	}
	if err := next.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
		t.Fatal(err)
	}
	for _, setting := range next.manifest.Sysctls {
		if setting.Original != originals[setting.Path] {
			t.Fatalf("reexec replaced the pre-policy original: %+v", setting)
		}
	}
	if err := next.RestoreSysctls(context.Background()); err != nil {
		t.Fatal(err)
	}
	for path, expected := range originals {
		assertTestSysctl(t, next, path, expected)
	}
	info, err := os.Stat(s.statePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ownership file mode: %v %v", info, err)
	}
}

func TestSysctlBulkRestorationRecoversAfterInterruptedCleanup(t *testing.T) {
	s, kernel, state, cfg, runtime, originals, _ := sysctlTestSystem(t)
	if err := s.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
		t.Fatal(err)
	}
	// Include an unobserved replacement device. Restoring all.forwarding also
	// affects it, but it must retain its own current values, not the old TUN's.
	for i := range kernel.links {
		if kernel.links[i].Name == "utun" {
			kernel.links[i].Index = 20
		}
	}
	writeTestSysctl(t, s, "net/ipv6/conf/utun/forwarding", "1")
	writeTestSysctl(t, s, "net/ipv6/conf/utun/force_forwarding", "1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var durable []byte
	s.writeState = func() error {
		var err error
		durable, err = json.Marshal(s.manifest)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(s.manifest.Sysctls, func(setting ownedSysctl) bool { return setting.BeforeBulk != "" }) {
			return nil
		}
		// Model a crash immediately after the IPv6 all.forwarding restoration:
		// Linux changed default/per-interface values, while individual inverse
		// writes have not yet run. This touches only the isolated fake proc tree.
		writeTestSysctl(t, s, "net/ipv6/conf/all/forwarding", "0")
		writeTestSysctl(t, s, "net/ipv6/conf/default/forwarding", "0")
		for _, link := range kernel.links {
			writeTestSysctl(t, s, "net/ipv6/conf/"+link.Name+"/forwarding", "0")
			writeTestSysctl(t, s, "net/ipv6/conf/"+link.Name+"/force_forwarding", "0")
		}
		cancel()
		return nil
	}
	if err := s.RestoreSysctls(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted cleanup: %v", err)
	}
	next := &linuxRouteSystem{request: testWireConnection(kernel.handle), procRoot: s.procRoot, lease: -1, writeState: func() error { return nil }}
	defer next.Close()
	if err := json.Unmarshal(durable, &next.manifest); err != nil {
		t.Fatal(err)
	}
	if err := next.RestoreSysctls(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertTestSysctl(t, next, "net/ipv6/conf/eth0/forwarding", originals["net/ipv6/conf/eth0/forwarding"])
	assertTestSysctl(t, next, "net/ipv6/conf/eth0/force_forwarding", "1")
	assertTestSysctl(t, next, "net/ipv6/conf/utun/forwarding", "1")
	assertTestSysctl(t, next, "net/ipv6/conf/utun/force_forwarding", "1")
	if len(next.manifest.Sysctls) != 0 {
		t.Fatal("recovered cleanup retained stale ownership")
	}
}

func TestSysctlManifestRejectsArbitraryPathsAndValues(t *testing.T) {
	for _, setting := range []ownedSysctl{
		{Path: "net/ipv6/conf/all/disable_ipv6", Original: "1", Written: "0"},
		{Path: "net/ipv4/conf/../forwarding", Original: "0", Written: "1"},
		{Path: "net/ipv4/conf/utun/forwarding", Original: "0", Written: "1"},
		{Path: "net/ipv6/conf/all/forwarding", Original: "0", Written: "1", InterfaceIndex: 7},
		{Path: "net/ipv4/conf/utun/rp_filter", Original: "3", Written: "0", InterfaceIndex: 10},
		{Path: "net/ipv4/conf/utun/rp_filter", Original: "1", Written: "0", BeforeBulk: "$(command)", InterfaceIndex: 10},
	} {
		if err := validateOwnedSysctl(setting); err == nil {
			t.Fatalf("accepted unsafe journal entry: %+v", setting)
		}
	}
}

func TestSysctlRestoresRAInheritedByLaterInterface(t *testing.T) {
	s, kernel, state, cfg, runtime, _, _ := sysctlTestSystem(t)
	// This pre-existing explicit value must not be confused with a device
	// which later inherited the module's temporary default.accept_ra=2.
	writeTestSysctl(t, s, "net/ipv6/conf/spare0/accept_ra", "2")
	if err := s.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
		t.Fatal(err)
	}
	added := networkInterface{Index: 20, Name: "later0", Up: true}
	kernel.links = append(kernel.links, added)
	state.Interfaces = append(state.Interfaces, added)
	for suffix, value := range map[string]string{
		"net/ipv4/conf/later0/rp_filter": "1", "net/ipv4/conf/later0/forwarding": "0",
		"net/ipv6/conf/later0/forwarding": "1", "net/ipv6/conf/later0/accept_ra": "2",
		"net/ipv6/conf/later0/force_forwarding": "0",
	} {
		writeTestSysctl(t, s, suffix, value)
	}
	if err := s.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreSysctls(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertTestSysctl(t, s, "net/ipv6/conf/spare0/accept_ra", "2")
	assertTestSysctl(t, s, "net/ipv6/conf/later0/accept_ra", "1")
}

func TestSysctlRAInheritanceHandlesReplacedInterfaceBeforeNextEvent(t *testing.T) {
	s, kernel, state, cfg, runtime, _, _ := sysctlTestSystem(t)
	writeTestSysctl(t, s, "net/ipv6/conf/spare0/accept_ra", "2")
	writeTestSysctl(t, s, "net/ipv6/conf/lo/accept_ra", "2")
	if err := s.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
		t.Fatal(err)
	}
	for i := range kernel.links {
		if kernel.links[i].Name == "spare0" {
			kernel.links[i].Index = 21
		}
	}
	// New spare0 inherited the temporary default. Do not re-apply the old
	// spare0's explicit 2, even when disable wins the race with its link event.
	writeTestSysctl(t, s, "net/ipv6/conf/spare0/accept_ra", "2")
	if err := s.RestoreSysctls(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertTestSysctl(t, s, "net/ipv6/conf/spare0/accept_ra", "1")
	assertTestSysctl(t, s, "net/ipv6/conf/lo/accept_ra", "2")
}

func TestSysctlRAExternalChangesRemainAuthoritative(t *testing.T) {
	s, _, state, cfg, runtime, _, _ := sysctlTestSystem(t)
	if err := s.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
		t.Fatal(err)
	}
	writeTestSysctl(t, s, "net/ipv6/conf/eth0/accept_ra", "0")
	writeTestSysctl(t, s, "net/ipv6/conf/lan0/accept_ra", "2")
	if err := s.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreSysctls(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertTestSysctl(t, s, "net/ipv6/conf/eth0/accept_ra", "0")
	assertTestSysctl(t, s, "net/ipv6/conf/lan0/accept_ra", "2")
}

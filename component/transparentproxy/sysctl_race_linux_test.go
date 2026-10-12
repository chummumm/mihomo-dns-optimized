//go:build linux

package transparentproxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/exp/slices"
	"golang.org/x/sys/unix"
)

func removeSysctlTestInterface(t *testing.T, s *linuxRouteSystem, kernel *routeTestKernel, name string) {
	t.Helper()
	for _, family := range []string{"ipv4", "ipv6"} {
		if err := os.RemoveAll(filepath.Join(s.procRoot, "net", family, "conf", name)); err != nil {
			t.Fatal(err)
		}
	}
	kernel.links = slices.DeleteFunc(kernel.links, func(link networkInterface) bool { return link.Name == name })
}

func TestMissingInterfaceSysctlRequiresOnlyENOENT(t *testing.T) {
	path := "net/ipv4/conf/utun/forwarding"
	missing := &os.PathError{Op: "open", Path: path, Err: unix.ENOENT}
	for _, test := range []struct {
		name    string
		err     error
		missing bool
	}{
		{"nil", nil, false},
		{"path", missing, true},
		{"wrapped", fmt.Errorf("configure %s: %w", path, missing), true},
		{"joined", errors.Join(missing, fmt.Errorf("close: %w", os.ErrNotExist)), true},
		{"permission", os.ErrPermission, false},
		{"readonly", unix.EROFS, false},
		{"io", unix.EIO, false},
		{"mixed-permission", errors.Join(missing, os.ErrPermission), false},
		{"mixed-readonly", fmt.Errorf("configure: %w", errors.Join(missing, unix.EROFS)), false},
		{"mixed-io", errors.Join(fmt.Errorf("write: %w", missing), unix.EIO), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := isMissingInterfaceSysctl(path, 10, test.err); got != test.missing {
				t.Fatalf("missing=%v, want %v for %v", got, test.missing, test.err)
			}
		})
	}
	for _, path := range []string{"net/ipv4/ip_forward", "net/ipv4/conf/all/rp_filter", "net/ipv6/conf/default/forwarding"} {
		for _, index := range []int{0, 10} {
			if isMissingInterfaceSysctl(path, index, missing) {
				t.Fatalf("required sysctl %s treated as an absent interface", path)
			}
		}
	}
	if isMissingInterfaceSysctl(path, 0, missing) {
		t.Fatal("missing interface identity was accepted")
	}
}

func TestSysctlsInterfaceDisappearsAfterSnapshotAndReturnsWithNewIndex(t *testing.T) {
	for _, ipv6 := range []bool{false, true} {
		t.Run(fmt.Sprintf("ipv6-%t", ipv6), func(t *testing.T) {
			s, kernel, state, cfg, runtime, originals, _ := sysctlTestSystem(t)
			runtime.IPv6 = ipv6
			removeSysctlTestInterface(t, s, kernel, "utun")
			// The already completed snapshot still contains the removed device.
			if err := s.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
				t.Fatalf("stale interface snapshot interrupted configuration: %v", err)
			}
			assertTestSysctl(t, s, "net/ipv4/conf/lan0/forwarding", "1")
			state.Interfaces = slices.Clone(kernel.links)
			runtime.TunReady = false
			if err := s.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
				t.Fatalf("fresh snapshot without TUN failed: %v", err)
			}
			// A later event observes a replacement with its own administrator baseline.
			kernel.links = append(kernel.links, networkInterface{Index: 20, Name: "utun", Up: true})
			state.Interfaces = slices.Clone(kernel.links)
			for path, value := range originals {
				if strings.Contains(path, "/conf/utun/") {
					writeTestSysctl(t, s, path, value)
				}
			}
			path := "net/ipv4/conf/utun/rp_filter"
			writeTestSysctl(t, s, path, "2")
			runtime.TunIndex, runtime.TunReady = 20, true
			if err := s.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
				t.Fatalf("replacement interface did not converge: %v", err)
			}
			assertTestSysctl(t, s, "net/ipv4/conf/utun/forwarding", "1")
			assertTestSysctl(t, s, path, "0")
			if !slices.ContainsFunc(s.state.Sysctls, func(setting ownedSysctl) bool {
				return setting.Path == path && setting.InterfaceIndex == 20 && setting.Original == "2"
			}) {
				t.Fatal("replacement interface inherited the old device's baseline")
			}
			if err := s.RestoreSysctls(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertTestSysctl(t, s, path, "2")
			if len(s.state.Sysctls) != 0 {
				t.Fatal("successful restoration retained interface ownership")
			}
		})
	}
}

func TestSysctlsInterfaceDisappearsDuringApply(t *testing.T) {
	for _, test := range []struct{ path, device string }{
		{"net/ipv4/conf/utun/forwarding", "utun"},
		{"net/ipv6/conf/eth0/accept_ra", "eth0"},
		{"net/ipv6/conf/all/forwarding", "utun"},
		{"net/ipv4/conf/default/rp_filter", "utun"},
	} {
		t.Run(test.path, func(t *testing.T) {
			s, kernel, state, cfg, runtime, _, _ := sysctlTestSystem(t)
			writeTestSysctl(t, s, "net/ipv4/conf/utun/rp_filter", "2")
			removed := false
			s.beforeSysctlWrite = func(path, value string) error {
				if path == test.path && !removed {
					removed = true
					removeSysctlTestInterface(t, s, kernel, test.device)
				}
				return nil
			}
			if err := s.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
				t.Fatalf("interface removal interrupted remaining sysctls: %v", err)
			}
			if !removed {
				t.Fatal("test did not reach the requested read/write window")
			}
			s.beforeSysctlWrite = nil
			if err := s.RestoreSysctls(context.Background()); err != nil {
				t.Fatalf("removed interface prevented cleanup: %v", err)
			}
			if len(s.state.Sysctls) != 0 {
				t.Fatal("removed interface retained ownership after cleanup")
			}
		})
	}
}

func TestSysctlsRememberEffectsSkipsOnlyMissingInterfaces(t *testing.T) {
	s, kernel, _, _, _, _, _ := sysctlTestSystem(t)
	removeSysctlTestInterface(t, s, kernel, "utun")
	settings := []desiredSysctl{
		{Path: "net/ipv4/conf/utun/rp_filter", Value: "1", InterfaceIndex: 10},
		{Path: "net/ipv4/conf/eth0/rp_filter", Value: "1", InterfaceIndex: 2},
	}
	if err := s.rememberSysctlEffects(settings); err != nil {
		t.Fatalf("interface disappeared before the second read: %v", err)
	}
	if len(s.state.Sysctls) != 1 || s.state.Sysctls[0].Path != settings[1].Path {
		t.Fatalf("remaining interface baseline was not preserved: %+v", s.state.Sysctls)
	}
	path := "net/ipv4/conf/default/rp_filter"
	if err := os.Remove(filepath.Join(s.procRoot, path)); err != nil {
		t.Fatal(err)
	}
	if err := s.rememberSysctlEffects([]desiredSysctl{{Path: path, Value: "1"}}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing default setting was ignored: %v", err)
	}
}

func TestSysctlsMissingGlobalSettingsRemainErrors(t *testing.T) {
	for _, operation := range []string{"apply", "restore"} {
		for _, path := range []string{
			"net/ipv4/conf/all/rp_filter", "net/ipv4/conf/default/rp_filter",
			"net/ipv6/conf/all/forwarding", "net/ipv6/conf/default/forwarding",
			"net/ipv6/conf/default/accept_ra",
		} {
			t.Run(operation+"/"+path, func(t *testing.T) {
				s, _, state, cfg, runtime, _, _ := sysctlTestSystem(t)
				if operation == "restore" {
					if err := s.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Remove(filepath.Join(s.procRoot, path)); err != nil {
					t.Fatal(err)
				}
				var err error
				if operation == "apply" {
					err = s.ApplySysctls(context.Background(), state, cfg, runtime)
				} else {
					err = s.RestoreSysctls(context.Background())
				}
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("missing required sysctl was ignored: %v", err)
				}
			})
		}
	}
}

func TestSysctlsRealFailuresRemainErrors(t *testing.T) {
	for _, operation := range []string{"apply", "restore"} {
		for _, failure := range []error{os.ErrPermission, unix.EROFS, unix.EIO} {
			for _, joined := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/joined-%t", operation, failure, joined), func(t *testing.T) {
					s, _, state, cfg, runtime, _, _ := sysctlTestSystem(t)
					if operation == "restore" {
						if err := s.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
							t.Fatal(err)
						}
					}
					path := "net/ipv4/conf/utun/forwarding"
					s.beforeSysctlWrite = func(current, value string) error {
						if current != path {
							return nil
						}
						err := failure
						if joined {
							err = errors.Join(os.ErrNotExist, err)
						}
						return fmt.Errorf("write/close: %w", err)
					}
					var err error
					if operation == "apply" {
						err = s.ApplySysctls(context.Background(), state, cfg, runtime)
					} else {
						err = s.RestoreSysctls(context.Background())
					}
					if !errors.Is(err, failure) || !strings.Contains(err.Error(), path) {
						t.Fatalf("real failure or its path was lost: %v", err)
					}
				})
			}
		}
	}
}

func TestSysctlsInterfaceDisappearsDuringRestore(t *testing.T) {
	s, kernel, state, cfg, runtime, _, _ := sysctlTestSystem(t)
	if err := s.ApplySysctls(context.Background(), state, cfg, runtime); err != nil {
		t.Fatal(err)
	}
	removed := false
	s.beforeSysctlWrite = func(path, value string) error {
		if path == "net/ipv6/conf/all/forwarding" && !removed {
			removed = true
			removeSysctlTestInterface(t, s, kernel, "utun")
		}
		return nil
	}
	if err := s.RestoreSysctls(context.Background()); err != nil {
		t.Fatalf("interface disappeared after restoration snapshot: %v", err)
	}
	if !removed || len(s.state.Sysctls) != 0 {
		t.Fatalf("restoration did not finish: removed=%t remaining=%+v", removed, s.state.Sysctls)
	}
}

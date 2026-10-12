//go:build linux

package transparentproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/exp/slices"
	"golang.org/x/sys/unix"
)

type ownedSysctl struct {
	Path     string
	Original string
	Written  string
	// A partial global/default restoration may leave the old or new value.
	// Both remain known to this process until its per-interface inverse completes.
	BeforeBulk     string
	InterfaceIndex int
}

type desiredSysctl struct {
	Path           string
	Value          string
	InterfaceIndex int
}

func sysctlInterface(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) == 5 && parts[2] == "conf" && parts[3] != "all" && parts[3] != "default" {
		return parts[3]
	}
	return ""
}

// Interface proc entries can disappear after a netlink snapshot or an earlier
// read. Global/default settings are required, and a joined write/close failure
// is harmless only when every cause is a missing entry.
func isMissingInterfaceSysctl(path string, ifIndex int, err error) bool {
	return ifIndex > 0 && sysctlInterface(path) != "" && onlySysctlNotExist(err)
}

func onlySysctlNotExist(err error) bool {
	switch err := err.(type) {
	case interface{ Unwrap() []error }:
		found := false
		for _, cause := range err.Unwrap() {
			if cause == nil {
				continue
			}
			if !onlySysctlNotExist(cause) {
				return false
			}
			found = true
		}
		return found
	case interface{ Unwrap() error }:
		return onlySysctlNotExist(err.Unwrap())
	default:
		return errors.Is(err, os.ErrNotExist)
	}
}

func validateOwnedSysctl(setting ownedSysctl) error {
	maximum := 1
	if setting.Path != "net/ipv4/ip_forward" {
		parts := strings.Split(setting.Path, "/")
		if len(parts) != 5 || parts[0] != "net" || parts[2] != "conf" || parts[3] == "" || parts[3] == "." || parts[3] == ".." || strings.ContainsAny(parts[3], "\x00\n\r") || len(parts[3]) > 15 {
			return errors.New("invalid owned sysctl path")
		}
		if parts[1] == "ipv4" && parts[4] == "rp_filter" || parts[1] == "ipv6" && parts[4] == "accept_ra" {
			maximum = 2
		} else if !((parts[1] == "ipv4" || parts[1] == "ipv6") && parts[4] == "forwarding") && !(parts[1] == "ipv6" && parts[4] == "force_forwarding") {
			return errors.New("unsupported owned sysctl")
		}
	}
	values := []string{setting.Original, setting.Written}
	if setting.BeforeBulk != "" {
		values = append(values, setting.BeforeBulk)
	}
	for _, value := range values {
		number, err := strconv.Atoi(value)
		if err != nil || number < 0 || number > maximum || strconv.Itoa(number) != value {
			return errors.New("invalid owned sysctl value")
		}
	}
	if sysctlInterface(setting.Path) != "" && setting.InterfaceIndex <= 0 {
		return errors.New("owned interface sysctl has no interface identity")
	}
	if sysctlInterface(setting.Path) == "" && setting.InterfaceIndex != 0 {
		return errors.New("global owned sysctl has an interface identity")
	}
	return nil
}

func (s *linuxRouteSystem) readSysctl(path string) (string, error) {
	data, err := os.ReadFile(filepath.Join(s.procRoot, path))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func (s *linuxRouteSystem) writeSysctl(path, value string) error {
	// The path is selected from the fixed allowlist above, never a YAML command.
	if s.beforeSysctlWrite != nil {
		if err := s.beforeSysctlWrite(path, value); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(filepath.Join(s.procRoot, path), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, err = io.WriteString(file, value+"\n")
	return errors.Join(err, file.Close())
}

func (s *linuxRouteSystem) ownSysctl(setting desiredSysctl) error {
	current, err := s.readSysctl(setting.Path)
	if err != nil {
		return err
	}
	index := slices.IndexFunc(s.state.Sysctls, func(old ownedSysctl) bool { return old.Path == setting.Path })
	if index >= 0 && s.state.Sysctls[index].InterfaceIndex != setting.InterfaceIndex {
		// Reusing an interface name does not transfer ownership of the old
		// device's settings to a replacement interface.
		if err := s.updateState(func(state *routeState) {
			state.Sysctls = slices.DeleteFunc(state.Sysctls, func(old ownedSysctl) bool { return old.Path == setting.Path })
		}); err != nil {
			return err
		}
		index = -1
	}
	if index < 0 && current == setting.Value {
		return nil
	}
	owned := ownedSysctl{Path: setting.Path, Original: current, Written: setting.Value, InterfaceIndex: setting.InterfaceIndex}
	if index >= 0 {
		owned.Original = s.state.Sysctls[index].Original
	}
	if err := validateOwnedSysctl(owned); err != nil {
		return err
	}
	if err := s.updateState(func(state *routeState) {
		if index >= 0 {
			state.Sysctls[index] = owned
		} else {
			state.Sysctls = append(state.Sysctls, owned)
		}
	}); err != nil {
		return err
	}
	if current == setting.Value {
		return nil
	}
	return s.writeSysctl(setting.Path, setting.Value)
}

func (s *linuxRouteSystem) ApplySysctls(ctx context.Context, state networkState, config Config, runtime Runtime) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	wanted := make(map[string]bool)
	apply := func(setting desiredSysctl) error {
		wanted[setting.Path] = true
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.ownSysctl(setting); err != nil {
			if isMissingInterfaceSysctl(setting.Path, setting.InterfaceIndex, err) {
				return nil
			}
			return fmt.Errorf("configure %s: %w", setting.Path, err)
		}
		return nil
	}
	active := config.Enable && (config.Uses(ModeTProxy) || config.Uses(ModeTUN) || config.Uses(ModeRedir))
	if active && config.Gateway.Enable {
		// IPv4 forwards according to its ingress interface's FORWARDING flag.
		// Unlike ip_forward/all.forwarding, writing the selected interfaces does
		// not reset all.accept_redirects or every interface's forwarding state.
		for _, link := range state.Interfaces {
			if !gatewayInterface(config, state, link.Name) && !(config.Uses(ModeTUN) && link.Name == runtime.TunName) {
				continue
			}
			if err := apply(desiredSysctl{Path: "net/ipv4/conf/" + link.Name + "/forwarding", Value: "1", InterfaceIndex: link.Index}); err != nil {
				return err
			}
		}
		if runtime.IPv6 && state.IPv6Available {
			forwarding, err := s.readSysctl("net/ipv6/conf/all/forwarding")
			if err != nil {
				return err
			}
			// Preserve RA acceptance across the host->router transition. When
			// forwarding was already enabled, only known uplinks need this.
			wan := map[string]bool{state.DefaultIPv4: true, state.DefaultIPv6: true}
			for _, name := range config.Local.Interfaces {
				if name != "auto" && name != "*" {
					wan[name] = true
				}
			}
			var inheritedRA *ownedSysctl
			for _, old := range s.state.Sysctls {
				if old.Path != "net/ipv6/conf/default/accept_ra" {
					continue
				}
				current, err := s.readSysctl(old.Path)
				if err != nil {
					return err
				}
				if current == old.Written {
					copy := old
					inheritedRA = &copy
				}
			}
			for _, link := range state.Interfaces {
				enableRA := !link.Loopback && link.Name != runtime.TunName
				if forwarding != "0" && inheritedRA == nil && (!enableRA || !wan[link.Name]) {
					continue
				}
				path := "net/ipv6/conf/" + link.Name + "/accept_ra"
				if err := s.ownRouterAdvertisements(path, link.Index, inheritedRA, enableRA); err != nil && !isMissingInterfaceSysctl(path, link.Index, err) {
					return err
				}
				wanted[path] = true
			}
			if forwarding == "0" {
				value, err := s.readSysctl("net/ipv6/conf/default/accept_ra")
				if err != nil {
					return err
				}
				if value != "0" {
					if err := apply(desiredSysctl{Path: "net/ipv6/conf/default/accept_ra", Value: "2"}); err != nil {
						return err
					}
				}
			}
			// all.forwarding changes per-interface and default forwarding too.
			// Remember those side effects before the bulk write, including values
			// already equal to 1 which must survive a later global restoration.
			bulkOwned := slices.ContainsFunc(s.state.Sysctls, func(old ownedSysctl) bool { return old.Path == "net/ipv6/conf/all/forwarding" })
			if forwarding == "0" || bulkOwned {
				settings := []desiredSysctl{{Path: "net/ipv6/conf/default/forwarding", Value: "1"}}
				for _, link := range state.Interfaces {
					settings = append(settings, desiredSysctl{Path: "net/ipv6/conf/" + link.Name + "/forwarding", Value: "1", InterfaceIndex: link.Index})
					// Recent kernels also clear force_forwarding when restoring
					// all.forwarding=0. Preserve that optional per-device setting.
					forcePath := "net/ipv6/conf/" + link.Name + "/force_forwarding"
					force, err := s.readSysctl(forcePath)
					if err == nil {
						settings = append(settings, desiredSysctl{Path: forcePath, Value: force, InterfaceIndex: link.Index})
					} else if !isMissingInterfaceSysctl(forcePath, link.Index, err) {
						return err
					}
				}
				// Read all originals before changing any forwarding value.
				var originals []ownedSysctl
				for _, setting := range settings {
					current, err := s.readSysctl(setting.Path)
					if isMissingInterfaceSysctl(setting.Path, setting.InterfaceIndex, err) {
						continue
					}
					if err != nil {
						return err
					}
					wanted[setting.Path] = true
					oldIndex := slices.IndexFunc(s.state.Sysctls, func(old ownedSysctl) bool {
						return old.Path == setting.Path && old.InterfaceIndex == setting.InterfaceIndex
					})
					if oldIndex >= 0 {
						continue
					}
					original := current
					if bulkOwned && setting.InterfaceIndex != 0 && current == "1" && strings.HasSuffix(setting.Path, "/forwarding") {
						for _, old := range s.state.Sysctls {
							if old.Path == "net/ipv6/conf/default/forwarding" {
								original = old.Original
							}
						}
					}
					entry := ownedSysctl{Path: setting.Path, Original: original, Written: setting.Value, InterfaceIndex: setting.InterfaceIndex}
					if err := validateOwnedSysctl(entry); err != nil {
						return err
					}
					originals = append(originals, entry)
				}
				if err := s.updateState(func(state *routeState) {
					for _, entry := range originals {
						state.Sysctls = slices.DeleteFunc(state.Sysctls, func(old ownedSysctl) bool { return old.Path == entry.Path })
						state.Sysctls = append(state.Sysctls, entry)
					}
				}); err != nil {
					return err
				}
				if err := apply(desiredSysctl{Path: "net/ipv6/conf/all/forwarding", Value: "1"}); err != nil {
					return err
				}
				for _, setting := range settings {
					if wanted[setting.Path] {
						if err := apply(setting); err != nil {
							return err
						}
					}
				}
			}
			// Keep RA adjustments that preserve the original host behavior while
			// this same global forwarding change remains owned.
			if bulkOwned || forwarding == "0" {
				for _, old := range s.state.Sysctls {
					if strings.HasSuffix(old.Path, "/accept_ra") {
						wanted[old.Path] = true
					}
				}
			}
		}
	}
	if active && (config.Local.Enable || config.Gateway.Enable) {
		all, err := s.readSysctl("net/ipv4/conf/all/rp_filter")
		if err != nil {
			return err
		}
		for _, old := range s.state.Sysctls {
			if old.Path == "net/ipv4/conf/all/rp_filter" {
				all = old.Original
			}
		}
		baseline, err := strconv.Atoi(all)
		if err != nil || baseline < 0 || baseline > 2 {
			return errors.New("invalid rp_filter state")
		}
		var settings []desiredSysctl
		for _, link := range state.Interfaces {
			captured := link.Loopback && config.Local.Enable || link.Name == runtime.TunName && config.Uses(ModeTUN)
			captured = captured || config.Gateway.Enable && gatewayInterface(config, state, link.Name)
			path := "net/ipv4/conf/" + link.Name + "/rp_filter"
			value, err := s.readSysctl(path)
			if isMissingInterfaceSysctl(path, link.Index, err) {
				continue
			}
			if err != nil {
				return err
			}
			current, err := strconv.Atoi(value)
			if err != nil || current < 0 || current > 2 {
				return errors.New("invalid interface rp_filter state")
			}
			wantedValue := value
			for _, old := range s.state.Sysctls {
				if old.Path == path && old.InterfaceIndex == link.Index && old.Written == value {
					wantedValue = old.Original
				}
			}
			wantedNumber, _ := strconv.Atoi(wantedValue)
			if captured {
				wantedValue = "0"
			} else if wantedNumber < baseline {
				wantedValue = all
			}
			settings = append(settings, desiredSysctl{Path: path, Value: wantedValue, InterfaceIndex: link.Index})
		}
		value, err := s.readSysctl("net/ipv4/conf/default/rp_filter")
		if err != nil {
			return err
		}
		defaultValue := value
		if number, err := strconv.Atoi(value); err != nil || number < 0 || number > 2 {
			return errors.New("invalid default rp_filter state")
		} else if number < baseline {
			defaultValue = all
		}
		if defaultValue != value {
			// Writing a default IPv4 setting also updates interfaces which have
			// never overridden it. Remember every affected original first, then
			// apply explicit interface values after the default's bulk change.
			if err := s.rememberSysctlEffects(settings); err != nil {
				return err
			}
		}
		if err := apply(desiredSysctl{Path: "net/ipv4/conf/default/rp_filter", Value: defaultValue}); err != nil {
			return err
		}
		for _, setting := range settings {
			if err := apply(setting); err != nil {
				return err
			}
		}
		if err := apply(desiredSysctl{Path: "net/ipv4/conf/all/rp_filter", Value: "0"}); err != nil {
			return err
		}
	}
	return s.restoreSysctlsLocked(ctx, wanted, state.Interfaces)
}

// Record the existing interface census before changing default.accept_ra.
// Otherwise a pre-existing explicit 2 and a later interface which inherited
// our temporary default 2 would be indistinguishable during restoration.
func (s *linuxRouteSystem) ownRouterAdvertisements(path string, ifIndex int, inherited *ownedSysctl, enable bool) error {
	current, err := s.readSysctl(path)
	if err != nil {
		return err
	}
	index := slices.IndexFunc(s.state.Sysctls, func(old ownedSysctl) bool { return old.Path == path })
	known := index >= 0 && s.state.Sysctls[index].InterfaceIndex == ifIndex
	original := current
	if known {
		old := s.state.Sysctls[index]
		if current == old.Written {
			original = old.Original
		}
	} else if inherited != nil && current == inherited.Written {
		original = inherited.Original
	}
	written := current
	if enable && current != "0" {
		written = "2"
	}
	if current == "0" {
		// Explicit RA disable remains authoritative, including changes made
		// while this module is active. Do not restore an earlier enabled value.
		written = "0"
	}
	entry := ownedSysctl{Path: path, Original: original, Written: written, InterfaceIndex: ifIndex}
	if err := validateOwnedSysctl(entry); err != nil {
		return err
	}
	if err := s.updateState(func(state *routeState) {
		if index >= 0 {
			state.Sysctls[index] = entry
		} else {
			state.Sysctls = append(state.Sysctls, entry)
		}
	}); err != nil {
		return err
	}
	if current != written {
		return s.writeSysctl(path, written)
	}
	return nil
}

// rememberSysctlEffects records settings before another sysctl can change them
// indirectly. This includes unchanged values, which a later global restoration
// could otherwise overwrite without an original value to restore.
func (s *linuxRouteSystem) rememberSysctlEffects(settings []desiredSysctl) error {
	var originals []ownedSysctl
	for _, setting := range settings {
		if slices.ContainsFunc(s.state.Sysctls, func(old ownedSysctl) bool {
			return old.Path == setting.Path && old.InterfaceIndex == setting.InterfaceIndex
		}) {
			continue
		}
		current, err := s.readSysctl(setting.Path)
		if isMissingInterfaceSysctl(setting.Path, setting.InterfaceIndex, err) {
			continue
		}
		if err != nil {
			return err
		}
		entry := ownedSysctl{Path: setting.Path, Original: current, Written: setting.Value, InterfaceIndex: setting.InterfaceIndex}
		if err := validateOwnedSysctl(entry); err != nil {
			return err
		}
		originals = append(originals, entry)
	}
	return s.updateState(func(state *routeState) {
		for _, entry := range originals {
			state.Sysctls = slices.DeleteFunc(state.Sysctls, func(old ownedSysctl) bool { return old.Path == entry.Path })
			state.Sysctls = append(state.Sysctls, entry)
		}
	})
}

func gatewayInterface(config Config, state networkState, name string) bool {
	for _, selected := range config.Gateway.Interfaces {
		if selected == "*" || selected == name || selected == "auto" && (name == state.DefaultIPv4 || name == state.DefaultIPv6) {
			return true
		}
	}
	return false
}

func (s *linuxRouteSystem) RestoreSysctls(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	messages, err := s.request.Execute(ctx, unix.RTM_GETLINK, unix.NLM_F_DUMP, make([]byte, 16))
	if err != nil {
		return err
	}
	var links []networkInterface
	for _, message := range messages {
		link, err := decodeNetworkLink(message.Data)
		if err != nil {
			return err
		}
		links = append(links, link)
	}
	return s.restoreSysctlsLocked(ctx, nil, links)
}

func (s *linuxRouteSystem) restoreSysctlsLocked(ctx context.Context, keep map[string]bool, links []networkInterface) error {
	indices := make(map[string]int, len(links))
	for _, link := range links {
		indices[link.Name] = link.Index
	}
	// A link may be created after the last handled event and immediately before
	// explicit disable. Record its inherited RA setting before removing the
	// default that proves the inheritance; cleanup cannot wait for another event.
	for _, baseline := range slices.Clone(s.state.Sysctls) {
		if baseline.Path != "net/ipv6/conf/default/accept_ra" || keep[baseline.Path] {
			continue
		}
		current, err := s.readSysctl(baseline.Path)
		if err != nil {
			return err
		}
		if current != baseline.Written {
			continue
		}
		for _, link := range links {
			path := "net/ipv6/conf/" + link.Name + "/accept_ra"
			if keep[path] || slices.ContainsFunc(s.state.Sysctls, func(old ownedSysctl) bool { return old.Path == path && old.InterfaceIndex == link.Index }) {
				continue
			}
			if err := s.ownRouterAdvertisements(path, link.Index, &baseline, false); err != nil && !isMissingInterfaceSysctl(path, link.Index, err) {
				return err
			}
		}
	}
	var restore []ownedSysctl
	var retire []string
	for _, setting := range s.state.Sysctls {
		if keep[setting.Path] {
			continue
		}
		if err := validateOwnedSysctl(setting); err != nil {
			return err
		}
		retire = append(retire, setting.Path)
		if name := sysctlInterface(setting.Path); name != "" && indices[name] != setting.InterfaceIndex {
			continue
		}
		current, err := s.readSysctl(setting.Path)
		if isMissingInterfaceSysctl(setting.Path, setting.InterfaceIndex, err) {
			continue
		}
		if err != nil {
			return err
		}
		if current == setting.Written || setting.BeforeBulk != "" && current == setting.BeforeBulk {
			restore = append(restore, setting)
		}
	}
	// A global/default restoration can also affect devices created since the
	// last event, or an interface whose name was reused. Preserve their actual
	// current values; the old interface's original values do not apply to them.
	// Remember this inverse operation before the bulk write so this process
	// can retry a partially failed restoration without losing interface values.
	var inverse []ownedSysctl
	for _, bulk := range slices.Clone(restore) {
		var paths []desiredSysctl
		switch bulk.Path {
		case "net/ipv6/conf/all/forwarding":
			paths = append(paths, desiredSysctl{Path: "net/ipv6/conf/default/forwarding", Value: bulk.Original})
			for _, link := range links {
				paths = append(paths, desiredSysctl{Path: "net/ipv6/conf/" + link.Name + "/forwarding", Value: bulk.Original, InterfaceIndex: link.Index})
				if bulk.Original == "0" {
					paths = append(paths, desiredSysctl{Path: "net/ipv6/conf/" + link.Name + "/force_forwarding", Value: "0", InterfaceIndex: link.Index})
				}
			}
		case "net/ipv4/conf/default/rp_filter":
			for _, link := range links {
				paths = append(paths, desiredSysctl{Path: "net/ipv4/conf/" + link.Name + "/rp_filter", Value: bulk.Original, InterfaceIndex: link.Index})
			}
		}
		for _, setting := range paths {
			index := slices.IndexFunc(restore, func(old ownedSysctl) bool { return old.Path == setting.Path })
			value, err := s.readSysctl(setting.Path)
			if isMissingInterfaceSysctl(setting.Path, setting.InterfaceIndex, err) {
				continue
			}
			if err != nil {
				return err
			}
			if index < 0 && value == setting.Value {
				continue
			}
			entry := ownedSysctl{Path: setting.Path, Original: value, Written: setting.Value, InterfaceIndex: setting.InterfaceIndex}
			if index >= 0 {
				entry.Original = restore[index].Original
			}
			if value != setting.Value {
				entry.BeforeBulk = value
			}
			if err := validateOwnedSysctl(entry); err != nil {
				return err
			}
			inverse = append(inverse, entry)
			if index >= 0 {
				restore[index] = entry
			} else {
				restore = append(restore, entry)
				retire = append(retire, setting.Path)
			}
		}
	}
	if err := s.updateState(func(state *routeState) {
		for _, entry := range inverse {
			state.Sysctls = slices.DeleteFunc(state.Sysctls, func(old ownedSysctl) bool { return old.Path == entry.Path })
			state.Sysctls = append(state.Sysctls, entry)
		}
	}); err != nil {
		return err
	}
	// Read ownership before changing all.forwarding, which itself rewrites the
	// per-interface values. Restore global values first, then their owned effects.
	slices.SortStableFunc(restore, func(a, b ownedSysctl) int {
		rank := func(path string) int {
			if path == "net/ipv4/ip_forward" || path == "net/ipv6/conf/all/forwarding" {
				return 0
			}
			return 1
		}
		return rank(a.Path) - rank(b.Path)
	})
	for _, setting := range restore {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.writeSysctl(setting.Path, setting.Original); err != nil && !isMissingInterfaceSysctl(setting.Path, setting.InterfaceIndex, err) {
			return fmt.Errorf("restore %s: %w", setting.Path, err)
		}
	}
	return s.updateState(func(state *routeState) {
		state.Sysctls = slices.DeleteFunc(state.Sysctls, func(old ownedSysctl) bool { return slices.Contains(retire, old.Path) })
	})
}

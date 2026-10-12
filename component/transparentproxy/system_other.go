//go:build !linux

package transparentproxy

import "errors"

func newRouteSystem(string) (routeSystem, error) {
	return nil, errors.New("transparent-proxy is supported only on Linux")
}

func newFirewall(string) (firewall, error) {
	return nil, errors.New("transparent-proxy is supported only on Linux")
}

func protectProcessSockets(Config) (*socketProtection, error) {
	return nil, errors.New("transparent-proxy is supported only on Linux")
}

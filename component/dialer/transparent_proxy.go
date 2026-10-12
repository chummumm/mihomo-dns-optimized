package dialer

import (
	"context"
	"fmt"
	"syscall"

	"github.com/metacubex/mihomo/common/sockopt"
	"github.com/metacubex/mihomo/component/transparentproxy"
)

var (
	managedCaptureEnabled = transparentproxy.IsEnabled
	managedOutboundMark   = transparentproxy.OutboundMark
	setManagedSocketMark  = sockopt.RawConnMark
)

// Attach this control even while capture is disabled: enabling it can race
// with a previously constructed Dialer. The check and mark registration occur
// on the real socket, before connect or the first UDP write. Virtual dialers do
// not own a kernel socket here; their underlying transport uses this same path.
func managedOutboundControl(mark int) controlFn {
	return func(_ context.Context, _, _ string, conn syscall.RawConn) error {
		if !managedCaptureEnabled() {
			return nil
		}
		resolved, err := managedOutboundMark(uint32(mark))
		if err != nil {
			return fmt.Errorf("transparent-proxy outbound mark: %w", err)
		}
		// Write the complete chosen mark, including for loopback addresses.
		// Explicit routing-mark values must not be changed by adding flag bits.
		if err := setManagedSocketMark(conn, int(resolved)); err != nil {
			return fmt.Errorf("transparent-proxy protect outbound socket: %w", err)
		}
		return nil
	}
}

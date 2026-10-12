//go:build linux

package transparentproxy

import (
	"fmt"

	"github.com/metacubex/nftables/binaryutil"
	"github.com/metacubex/nftables/expr"
	"golang.org/x/sys/unix"
)

// REDIRECT is a stateful NAT operation. Classify every packet before DNAT,
// translate only new sessions, and verify the translation after DNAT. This
// keeps an established un-NATed session or an old listener port from becoming
// an accidental direct path when NAT chains do not see the packet again.
func buildNFTRedir(state firewallState, addRule func(string, ...expr.Any)) {
	for _, path := range []string{"pre", "out"} {
		guard := "redir_guard_" + path
		// Replies from accepted sockets are already on the original path.
		addRule(guard, nftCTDirection(1)...)
		for _, family := range []int{4, 6} {
			nfproto := byte(unix.NFPROTO_IPV4)
			if family == 6 {
				nfproto = unix.NFPROTO_IPV6
			}
			match := nftMetaMatch(expr.MetaKeyNFPROTO, []byte{nfproto})
			match = append(match, nftMetaMatch(expr.MetaKeyMARK, binaryutil.NativeEndian.PutUint32(RedirMark))...)
			addRule(guard, append(match, nftGoto(fmt.Sprintf("redir_check%d", family)))...)
		}
		// A previously redirected flow that no longer matches the current
		// classifier must reconnect; a stale NAT binding cannot migrate to
		// another backend, bypass policy, or listener port safely.
		addRule(guard, append(nftCTMark(RedirMark), nftDrop())...)
	}
	for _, family := range []int{4, 6} {
		suffix := fmt.Sprint(family)
		nfproto := byte(unix.NFPROTO_IPV4)
		ready := state.TCP == ModeRedir && state.Redir4
		if family == 6 {
			nfproto = unix.NFPROTO_IPV6
			ready = state.TCP == ModeRedir && state.IPv6 && state.Redir6
		}
		if !ready {
			addRule("redir"+suffix, nftMarkVerdict(RedirMark, nftDrop())...)
			addRule("redir_check"+suffix, nftDrop())
			continue
		}
		addRule("redir"+suffix, nftSetMark(RedirMark)...)
		for _, path := range []string{"pre", "out"} {
			match := nftMetaMatch(expr.MetaKeyNFPROTO, []byte{nfproto})
			match = append(match, nftProtocol(unix.IPPROTO_TCP)...)
			match = append(match, nftMetaMatch(expr.MetaKeyMARK, binaryutil.NativeEndian.PutUint32(RedirMark))...)
			// Never replace a foreign full conntrack mark. Incompatible
			// sessions remain unchanged and fail the terminal guard below.
			match = append(match, nftCTMark(0)...)
			match = append(match, nftDNATStatus(false)...)
			match = append(match,
				&expr.Immediate{Register: 1, Data: binaryutil.NativeEndian.PutUint32(RedirMark)},
				&expr.Ct{Key: expr.CtKeyMARK, SourceRegister: true, Register: 1},
				&expr.Immediate{Register: 1, Data: binaryutil.BigEndian.PutUint16(state.RedirPort)},
				&expr.Redir{RegisterProtoMin: 1, RegisterProtoMax: 1})
			addRule("redir_nat_"+path, match...)
		}
		match := nftProtocol(unix.IPPROTO_TCP)
		match = append(match, nftCTMark(RedirMark)...)
		match = append(match, nftDNATStatus(true)...)
		match = append(match,
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(state.RedirPort)},
			&expr.Fib{Register: 1, FlagDADDR: true, ResultADDRTYPE: true},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(unix.RTN_LOCAL)}, nftAccept())
		addRule("redir_check"+suffix, match...)
		addRule("redir_check"+suffix, nftDrop())
	}
}

func nftCTDirection(direction byte) []expr.Any {
	return []expr.Any{&expr.Ct{Key: expr.CtKeyDIRECTION, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{direction}}, nftReturn()}
}

func nftCTMark(mark uint32) []expr.Any {
	return []expr.Any{&expr.Ct{Key: expr.CtKeyMARK, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(mark)}}
}

func nftDNATStatus(present bool) []expr.Any {
	const destinationNAT = 1 << 5 // IPS_DST_NAT
	value := uint32(0)
	if present {
		value = destinationNAT
	}
	return []expr.Any{
		&expr.Ct{Key: expr.CtKeySTATUS, Register: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: binaryutil.NativeEndian.PutUint32(destinationNAT), Xor: binaryutil.NativeEndian.PutUint32(0)},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(value)},
	}
}

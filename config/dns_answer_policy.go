package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/metacubex/mihomo/dns"
	"go.yaml.in/yaml/v3"
)

// RawDNSQtypes accepts YAML lists as well as SmartDNS's comma/space-separated
// directive syntax. Each list item is one directive, allowing ordered add,
// range, clear-all ("-") and clear-selected ("-,28,65") operations.
type RawDNSQtypes []string

func (q *RawDNSQtypes) UnmarshalYAML(node *yaml.Node) error {
	var nodes []*yaml.Node
	if node.Kind == yaml.SequenceNode {
		nodes = node.Content
	} else {
		nodes = []*yaml.Node{node}
	}
	values := make(RawDNSQtypes, 0, len(nodes))
	for _, item := range nodes {
		if item.Kind != yaml.ScalarNode || (item.Tag != "!!str" && item.Tag != "!!int") {
			return fmt.Errorf("dns.force-qtype-SOA must be a query type number, a directive string, or a list of them")
		}
		values = append(values, item.Value)
	}
	*q = values
	return nil
}

func parseDNSForcedQtypes(values RawDNSQtypes) (*dns.ForcedQtypes, error) {
	if len(values) == 0 {
		return nil, nil
	}
	var types dns.ForcedQtypes
	for _, value := range values {
		parts := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
		if len(parts) == 0 {
			return nil, fmt.Errorf("dns.force-qtype-SOA contains an empty directive")
		}
		clearTypes := parts[0] == "-"
		if clearTypes {
			parts = parts[1:]
			if len(parts) == 0 {
				types = dns.ForcedQtypes{}
				continue
			}
		}
		for _, part := range parts {
			bounds := strings.Split(part, "-")
			if len(bounds) > 2 {
				return nil, fmt.Errorf("dns.force-qtype-SOA has invalid query type range %q", part)
			}
			start, err := strconv.ParseUint(bounds[0], 10, 16)
			if err != nil {
				return nil, fmt.Errorf("dns.force-qtype-SOA query types must be numbers from 0 to 65535: %q", part)
			}
			end := start
			if len(bounds) == 2 {
				end, err = strconv.ParseUint(bounds[1], 10, 16)
				if err != nil || end < start {
					return nil, fmt.Errorf("dns.force-qtype-SOA has invalid query type range %q", part)
				}
			}
			for qtype := start; qtype <= end; qtype++ {
				mask := uint64(1) << (qtype % 64)
				if clearTypes {
					types[qtype/64] &^= mask
				} else {
					types[qtype/64] |= mask
				}
			}
		}
	}
	if types == (dns.ForcedQtypes{}) {
		return nil, nil
	}
	return &types, nil
}

func parseDNSAnswerPolicy(raw RawDNS) (dns.AnswerPolicy, error) {
	var result dns.AnswerPolicy
	if raw.MaxReplyIPNum != nil {
		if *raw.MaxReplyIPNum < 1 || *raw.MaxReplyIPNum > math.MaxInt32 {
			return result, fmt.Errorf("dns.max-reply-ip-num must be between 1 and %d", int64(math.MaxInt32))
		}
		result.ReplyIPLimit = int(*raw.MaxReplyIPNum)
	}
	for _, setting := range []struct {
		name  string
		value int64
	}{{"rr-ttl", raw.RRTTL}, {"rr-ttl-min", raw.RRTTLMin}, {"rr-ttl-max", raw.RRTTLMax}, {"rr-ttl-reply-max", raw.RRTTLReplyMax}} {
		if setting.value < 0 || setting.value > math.MaxUint32 {
			return result, fmt.Errorf("dns.%s must be between 0 and %d seconds", setting.name, uint64(math.MaxUint32))
		}
	}
	if raw.RRTTL == 0 && raw.RRTTLMax != 0 && raw.RRTTLMin > raw.RRTTLMax {
		return result, fmt.Errorf("dns.rr-ttl-min must not exceed dns.rr-ttl-max")
	}
	result.ForceNoCNAME = raw.ForceNoCNAME
	var err error
	result.ForceQtypeSOA, err = parseDNSForcedQtypes(raw.ForceQtypeSOA)
	if err != nil {
		return result, err
	}
	result.ForceAAAASOA = raw.ForceAAAASOA
	result.RRTTL, result.RRTTLReplyMax = uint32(raw.RRTTL), uint32(raw.RRTTLReplyMax)
	result.RRTTLMin, result.RRTTLMax = uint32(raw.RRTTLMin), uint32(raw.RRTTLMax)
	return result, nil
}

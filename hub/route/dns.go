package route

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/metacubex/mihomo/component/dnsstats"
	"github.com/metacubex/mihomo/component/resolver"

	"github.com/metacubex/chi"
	"github.com/metacubex/chi/render"
	"github.com/metacubex/http"
	"github.com/miekg/dns"
	"github.com/samber/lo"
)

func dnsRouter() http.Handler {
	r := chi.NewRouter()
	r.Get("/query", queryDNS)
	r.Get("/observability", dnsObservability)
	r.Get("/observability/", dnsObservability)
	r.Get("/observability/stats", dnsObservabilityStats)
	r.Get("/observability/queries", dnsObservabilityQueries)
	r.Get("/observability/upstreams", dnsObservabilityUpstreams)
	return r
}

func dnsObservability(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	render.JSON(w, r, dnsstats.Default.Status())
}

func dnsObservabilityStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	render.JSON(w, r, dnsstats.Default.Stats())
}

func dnsObservabilityUpstreams(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	render.JSON(w, r, dnsstats.Default.Upstreams())
}

func dnsObservabilityQueries(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	query := r.URL.Query()
	filter := dnsstats.Filter{Limit: 50, QName: query.Get("qname"), Client: query.Get("client"), Outcome: query.Get("outcome"), QType: strings.ToUpper(query.Get("qtype"))}
	bad := func(message string) { render.Status(r, http.StatusBadRequest); render.JSON(w, r, newError(message)) }
	if value := query.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 200 {
			bad("limit must be between 1 and 200")
			return
		}
		filter.Limit = limit
	}
	if value := query.Get("cursor"); value != "" {
		cursor, err := strconv.ParseUint(value, 10, 64)
		if err != nil || cursor == 0 {
			bad("cursor must be a positive query ID")
			return
		}
		filter.Cursor = cursor
	}
	if len(filter.QName) > 254 || len(filter.Client) > 64 || len(filter.QType) > 16 {
		bad("query filter is too long")
		return
	}
	if filter.Outcome != "" {
		if _, valid := dnsstats.ParseOutcome(filter.Outcome); !valid {
			bad("unknown outcome")
			return
		}
	}
	render.JSON(w, r, dnsstats.Default.Queries(filter))
}

func queryDNS(w http.ResponseWriter, r *http.Request) {
	if resolver.DefaultResolver == nil {
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, newError("DNS section is disabled"))
		return
	}

	name := r.URL.Query().Get("name")
	qTypeStr, _ := lo.Coalesce(r.URL.Query().Get("type"), "A")

	qType, exist := dns.StringToType[qTypeStr]
	if !exist {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, newError("invalid query type"))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), resolver.DefaultDNSTimeout)
	defer cancel()

	msg := dns.Msg{}
	msg.SetQuestion(dns.Fqdn(name), qType)
	resp, err := resolver.DefaultResolver.ExchangeContext(ctx, &msg)
	if err != nil {
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, newError(err.Error()))
		return
	}

	responseData := render.M{
		"Status":   resp.Rcode,
		"Question": resp.Question,
		"TC":       resp.Truncated,
		"RD":       resp.RecursionDesired,
		"RA":       resp.RecursionAvailable,
		"AD":       resp.AuthenticatedData,
		"CD":       resp.CheckingDisabled,
	}

	rr2Json := func(rr dns.RR, _ int) render.M {
		header := rr.Header()
		return render.M{
			"name": header.Name,
			"type": header.Rrtype,
			"TTL":  header.Ttl,
			"data": lo.Substring(rr.String(), len(header.String()), math.MaxUint),
		}
	}

	if len(resp.Answer) > 0 {
		responseData["Answer"] = lo.Map(resp.Answer, rr2Json)
	}
	if len(resp.Ns) > 0 {
		responseData["Authority"] = lo.Map(resp.Ns, rr2Json)
	}
	if len(resp.Extra) > 0 {
		responseData["Additional"] = lo.Map(resp.Extra, rr2Json)
	}

	render.JSON(w, r, responseData)
}

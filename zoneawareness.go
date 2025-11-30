// Package zoneawareness is a CoreDNS plugin that lookups the responses and reorders them
// based on the CIDRs defined.

package zoneawareness

import (
	"context"
	"net"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/metrics"

	"github.com/miekg/dns"
)

type Zone struct {
	CIDRs []*net.IPNet
}

type Zoneawareness struct {
	Next                      plugin.Handler
	Zones                     map[string]*Zone
	currentAvailabilityZoneId string
	HasSynced                 bool
}

// ServeDNS implements the plugin.Handler interface. This method gets called when zoneawareness is used
// in a Server.
func (e Zoneawareness) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	pw := NewResponsePrinter(w)

	rcode, err := plugin.NextOrFailure(e.Name(), e.Next, ctx, pw, r)
	if err != nil {
		return rcode, err
	}
	if pw.msg == nil {
		// The next plugin handled the response, so we don't need to do anything.
		return rcode, nil
	}

	if pw.msg.Rcode != dns.RcodeSuccess {
		log.Debugf("Received error response: %d", pw.msg.Rcode)
		return writeFinalResponse(w, pw.msg)
	}

	if len(pw.msg.Answer) <= 1 {
		log.Debugf("No answers in response or just 1 entry, skipping reordering")
		return writeFinalResponse(w, pw.msg)
	}

	var preferredAnswers []dns.RR
	var otherAnswers []dns.RR

	// --- Start of reordering logzic to time ---
	reorderTimeStart := time.Now()

	for _, rr := range pw.msg.Answer {
		ip := extractRRIP(rr)
		if ip != nil && ipMatchesCIDRs(ip, e.Zones[e.currentAvailabilityZoneId].CIDRs) {
			log.Debugf("Matched preferred IP %s in zone %s", ip, e.currentAvailabilityZoneId)
			preferredAnswers = append(preferredAnswers, rr)
		} else {
			otherAnswers = append(otherAnswers, rr)
		}
	}

	// --- End of reordering logic to time ---
	// We only record the latency it took to reorder the answers
	reorderLatency.WithLabelValues(metrics.WithServer(ctx)).Observe(time.Since(reorderTimeStart).Seconds())

	// If no preferred answers are found, return the original message
	if len(preferredAnswers) == 0 {
		log.Debugf("No preferred answers found in zone %s for query %+v (answer: %s)", e.currentAvailabilityZoneId, pw.msg.Question, pw.msg.Answer)
		return writeFinalResponse(w, pw.msg)
	}

	// Overwrite the original message with the reordered answers
	pw.msg = pw.msg.Copy() /* Is this needed ? https://github.com/coredns/coredns/blob/master/plugin.md?#mutating-a-response */
	pw.msg.Answer = append(preferredAnswers, otherAnswers...)

	// Increase counter to indicate a query was reordered
	reorderedQueriesCount.WithLabelValues(metrics.WithServer(ctx)).Inc()

	// Increase reorder count by the number of preferred answers
	reorderCount.WithLabelValues(metrics.WithServer(ctx)).Add(float64(len(preferredAnswers)))

	log.Debugf("Reordered %d answers for query %s", len(preferredAnswers), pw.msg.Question[0].Name)

	return writeFinalResponse(w, pw.msg)
}

// writeFinalResponse writes the final response to the client.
func writeFinalResponse(w dns.ResponseWriter, msg *dns.Msg) (int, error) {
	if err := w.WriteMsg(msg); err != nil {
		log.Errorf("Failed to write response: %v", err)
		return dns.RcodeServerFailure, err
	}
	return dns.RcodeSuccess, nil
}

// extractRRIP extracts the IP address from a DNS resource record.
func extractRRIP(rr dns.RR) net.IP {
	switch rr := rr.(type) {
	case *dns.A:
		return rr.A
	case *dns.AAAA:
		return rr.AAAA
	default:
		return nil
	}
}

// ipMatchesCIDRs checks if the given IP address matches any of the CIDRs.
func ipMatchesCIDRs(ip net.IP, cidrs []*net.IPNet) bool {
	for _, cidr := range cidrs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// Name implements the Handler interface.
func (e Zoneawareness) Name() string { return "zoneawareness" }

// ResponsePrinter wrap a dns.ResponseWriter and will write zoneawareness to standard output when WriteMsg is called.
type ResponsePrinter struct {
	dns.ResponseWriter
	msg *dns.Msg
}

// NewResponsePrinter returns ResponseWriter.
func NewResponsePrinter(w dns.ResponseWriter) *ResponsePrinter {
	return &ResponsePrinter{ResponseWriter: w}
}

// WriteMsg implements the dns.ResponseWriter interface. It is called when a response is written.
// TODO: Should this always return nil?
func (r *ResponsePrinter) WriteMsg(res *dns.Msg) error {
	r.msg = res
	return nil
}

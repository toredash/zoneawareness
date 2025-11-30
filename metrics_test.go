package zoneawareness

import (
	"context"
	"net"
	"testing"

	"github.com/coredns/coredns/plugin/pkg/dnstest"
	"github.com/coredns/coredns/plugin/test"
	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMetrics(t *testing.T) {
	// 1. Setup the plugin with a test zone and CIDR
	_, cidr, _ := net.ParseCIDR("192.168.1.0/24")
	za := Zoneawareness{
		currentAvailabilityZoneId: "test-az-1",
		Zones: map[string]*Zone{
			"test-az-1": {
				CIDRs: []*net.IPNet{cidr},
			},
		},
		// The "Next" plugin is a dummy plugin that returns a pre-canned response.
		Next: test.HandlerFunc(func(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
			m := new(dns.Msg)
			m.SetReply(r)
			m.Answer = []dns.RR{
				test.A("example.org. IN A 10.0.0.1"),      // Other IP
				test.A("example.org. IN A 192.168.1.10"), // Preferred IP
				test.A("example.org. IN A 10.0.0.2"),      // Other IP
			}
			w.WriteMsg(m)
			return dns.RcodeSuccess, nil
		}),
	}

	ctx := context.TODO()
	req := new(dns.Msg)
	req.SetQuestion("example.org.", dns.TypeA)
	rec := dnstest.NewRecorder(&test.ResponseWriter{})

	// 2. Run the plugin's ServeDNS method
	za.ServeDNS(ctx, rec, req)

	// 3. Assert the metric values
	// We expect 1 query to have been reordered.
	if val := testutil.ToFloat64(reorderedQueriesCount); val != 1 {
		t.Errorf("Expected reorderedQueriesCount to be 1, got %f", val)
	}

	// We expect 1 record to have been reordered (192.168.1.10).
	if val := testutil.ToFloat64(reorderCount); val != 1 {
		t.Errorf("Expected reorderCount to be 1, got %f", val)
	}

	// We expect the latency histogram to have been observed once.
	if val := testutil.CollectAndCount(reorderLatency); val != 1 {
		t.Errorf("Expected reorderLatency to be observed once, got %d", val)
	}
}

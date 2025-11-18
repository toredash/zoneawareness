package zoneawareness

import (
	"bytes"
	"context"
	golog "log"
	"net"
	"strings"
	"testing"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/dnstest"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/plugin/test"

	"github.com/miekg/dns"
)

type mockHandler struct {
	msg *dns.Msg
}

func (h *mockHandler) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	w.WriteMsg(h.msg)
	return dns.RcodeSuccess, nil
}

func (h *mockHandler) Name() string { return "mock" }

func TestZoneawareness(t *testing.T) {
	// Create a new Zoneawareness Plugin.
	x := Zoneawareness{
		Zones:                     make(map[string]*Zone),
		currentAvailabilityZoneId: "euc1-az2", // Mock AZ ID
	}

	// Add a mock CIDR to the current zone
	_, mockCIDR, _ := net.ParseCIDR("10.0.0.0/24")
	x.Zones["euc1-az2"] = &Zone{CIDRs: []*net.IPNet{mockCIDR}}

	// Setup a new output buffer for logs.
	b := &bytes.Buffer{}
	golog.SetOutput(b)

	ctx := context.TODO()
	r := new(dns.Msg)
	r.SetQuestion("example.org.", dns.TypeA)

	// Create a mock response with some IPs matching the CIDR and some not.
	m := new(dns.Msg)
	m.SetReply(r)
	m.Answer = []dns.RR{
		test.A("example.org. 300 IN A 10.0.0.1"),    // Matches CIDR
		test.A("example.org. 300 IN A 192.168.1.1"), // Does not match CIDR
		test.A("example.org. 300 IN A 10.0.0.2"),    // Matches CIDR
	}

	// Create a mock handler that returns our mock message.
	x.Next = &mockHandler{msg: m}

	// Create a new Recorder that captures the result.
	rec := dnstest.NewRecorder(&test.ResponseWriter{})

	// Call our plugin directly, and check the result.
	_, err := x.ServeDNS(ctx, rec, r)
	if err != nil {
		t.Fatalf("Expected no error, but got %v", err)
	}

	// Check that the log output does NOT contain the reorder message by default.
	if a := b.String(); strings.Contains(a, "Reordered 2 answers") {
		t.Errorf("Expected log message to be suppressed, but got: %s", a)
	}

	// Verify the order of answers in the response.
	if len(rec.Msg.Answer) != 3 {
		t.Errorf("Expected 3 answers, got %d", len(rec.Msg.Answer))
	}

	// Check if preferred answers are at the beginning.
	if rec.Msg.Answer[0].(*dns.A).A.String() != "10.0.0.1" || rec.Msg.Answer[1].(*dns.A).A.String() != "10.0.0.2" {
		t.Errorf("Expected preferred IPs at the beginning, got %s, %s", rec.Msg.Answer[0].(*dns.A).A.String(), rec.Msg.Answer[1].(*dns.A).A.String())
	}
}

func TestZoneawarenessDebug(t *testing.T) {
	// Enable debug logging for this test.
	clog.D.Set()
	defer clog.D.Clear()

	// Create a new Zoneawareness Plugin.
	x := Zoneawareness{
		Zones:                     make(map[string]*Zone),
		currentAvailabilityZoneId: "euc1-az2", // Mock AZ ID
	}

	// Add a mock CIDR to the current zone
	_, mockCIDR, _ := net.ParseCIDR("10.0.0.0/24")
	x.Zones["euc1-az2"] = &Zone{CIDRs: []*net.IPNet{mockCIDR}}

	// Setup a new output buffer for logs.
	b := &bytes.Buffer{}
	golog.SetOutput(b)

	ctx := context.TODO()
	r := new(dns.Msg)
	r.SetQuestion("example.org.", dns.TypeA)

	// Create a mock response with some IPs matching the CIDR and some not.
	m := new(dns.Msg)
	m.SetReply(r)
	m.Answer = []dns.RR{
		test.A("example.org. 300 IN A 10.0.0.1"),    // Matches CIDR
		test.A("example.org. 300 IN A 192.168.1.1"), // Does not match CIDR
		test.A("example.org. 300 IN A 10.0.0.2"),    // Matches CIDR
	}

	// Create a mock handler that returns our mock message.
	x.Next = &mockHandler{msg: m}

	// Create a new Recorder that captures the result.
	rec := dnstest.NewRecorder(&test.ResponseWriter{})

	// Call our plugin directly, and check the result.
	_, err := x.ServeDNS(ctx, rec, r)
	if err != nil {
		t.Fatalf("Expected no error, but got %v", err)
	}

	// Check if the log output contains the expected reorder message.
	if a := b.String(); !strings.Contains(a, "[DEBUG] plugin/zoneawareness: Reordered 2 answers for query example.org.") {
		t.Errorf("Expected log message '[DEBUG] plugin/zoneawareness: Reordered 2 answers for query example.org.', but got: %s", a)
	}
}

func TestZoneawarenessCases(t *testing.T) {
	// Create a new Zoneawareness Plugin.
	x := Zoneawareness{
		Zones: make(map[string]*Zone),
	}

	_, az1Cidr, _ := net.ParseCIDR("192.0.2.0/24")
	_, az1CidrAAAA, _ := net.ParseCIDR("2001:db8:1::/48")
	x.Zones["use2-az1"] = &Zone{CIDRs: []*net.IPNet{az1Cidr, az1CidrAAAA}}

	_, az2Cidr, _ := net.ParseCIDR("192.2.0.0/24")
	_, az2CidrAAAA, _ := net.ParseCIDR("2001:db8:2::/48")
	x.Zones["use2-az2"] = &Zone{CIDRs: []*net.IPNet{az2Cidr, az2CidrAAAA}}

	tests := []struct {
		name             string
		zoneId           string
		question         dns.Question
		upstreamAnswers  []dns.RR
		expectedAnswers  []string // string representation of the RR
		expectReordering bool
		expectedRcode    int
		handler          plugin.Handler
	}{
		{
			name:     "multi-multi-a-az2",
			zoneId:   "use2-az2",
			question: dns.Question{Name: "multi-multi-a.coredns.io.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
			upstreamAnswers: []dns.RR{
				test.A("multi-multi-a.coredns.io. 300 IN A 192.0.2.1"),
				test.A("multi-multi-a.coredns.io. 300 IN A 192.2.0.1"),
				test.A("multi-multi-a.coredns.io. 300 IN A 192.2.0.2"),
				test.A("multi-multi-a.coredns.io. 300 IN A 192.2.0.3"),
			},
			expectedAnswers: []string{
				"multi-multi-a.coredns.io.	300	IN	A	192.2.0.1",
				"multi-multi-a.coredns.io.	300	IN	A	192.2.0.2",
				"multi-multi-a.coredns.io.	300	IN	A	192.2.0.3",
				"multi-multi-a.coredns.io.	300	IN	A	192.0.2.1",
			},
			expectReordering: true,
		},
		{
			name:     "multi-mixed-a-az1",
			zoneId:   "use2-az1",
			question: dns.Question{Name: "multi-mixed.coredns.io.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
			upstreamAnswers: []dns.RR{
				test.A("multi-mixed.coredns.io. 300 IN A 192.2.0.31"),
				test.A("multi-mixed.coredns.io. 300 IN A 192.0.2.31"),
			},
			expectedAnswers: []string{
				"multi-mixed.coredns.io.	300	IN	A	192.0.2.31",
				"multi-mixed.coredns.io.	300	IN	A	192.2.0.31",
			},
			expectReordering: true,
		},
		{
			name:     "multi-mixed-aaaa-az1",
			zoneId:   "use2-az1",
			question: dns.Question{Name: "multi-mixed.coredns.io.", Qtype: dns.TypeAAAA, Qclass: dns.ClassINET},
			upstreamAnswers: []dns.RR{
				test.AAAA("multi-mixed.coredns.io. 300 IN AAAA 2001:db8:2::32"),
				test.AAAA("multi-mixed.coredns.io. 300 IN AAAA 2001:db8:1::32"),
			},
			expectedAnswers: []string{
				"multi-mixed.coredns.io.	300	IN	AAAA	2001:db8:1::32",
				"multi-mixed.coredns.io.	300	IN	AAAA	2001:db8:2::32",
			},
			expectReordering: true,
		},
		{
			name:     "multi-no-match",
			zoneId:   "use2-az1",
			question: dns.Question{Name: "multi-no-match.coredns.io.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
			upstreamAnswers: []dns.RR{
				test.A("multi-no-match.coredns.io. 300 IN A 198.51.100.1"),
				test.A("multi-no-match.coredns.io. 300 IN A 198.51.100.2"),
			},
			expectedAnswers: []string{
				"multi-no-match.coredns.io.	300	IN	A	198.51.100.1",
				"multi-no-match.coredns.io.	300	IN	A	198.51.100.2",
			},
			expectReordering: false,
		},
		{
			name:             "no-answers",
			zoneId:           "use2-az1",
			question:         dns.Question{Name: "no-answers.coredns.io.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
			upstreamAnswers:  []dns.RR{},
			expectedAnswers:  []string{},
			expectReordering: false,
		},
	}

	ctx := context.TODO()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x.currentAvailabilityZoneId = tc.zoneId

			req := new(dns.Msg)
			req.SetQuestion(tc.question.Name, tc.question.Qtype)

			// Create a mock response
			m := new(dns.Msg)
			m.SetReply(req)
			m.Answer = tc.upstreamAnswers

			x.Next = &mockHandler{msg: m}

			rec := dnstest.NewRecorder(&test.ResponseWriter{})

			_, err := x.ServeDNS(ctx, rec, req)
			if err != nil {
				t.Fatalf("Expected no error, but got %v", err)
			}

			if len(rec.Msg.Answer) != len(tc.expectedAnswers) {
				t.Fatalf("Expected %d answers, but got %d", len(tc.expectedAnswers), len(rec.Msg.Answer))
			}

			for i, expected := range tc.expectedAnswers {
				actual := strings.Join(strings.Fields(rec.Msg.Answer[i].String()), "\t")
				if actual != expected {
					t.Errorf("Expected answer %d to be %q, but got %q", i, expected, actual)
				}
			}
		})
	}
}

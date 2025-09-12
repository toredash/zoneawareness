package zoneawareness

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
)

// setupTest replaces the external dependency functions with mocks for the duration of a test.
func setupTest(t *testing.T) {
	// Store original functions
	origIMDS := getConfigFromIMDSv2
	origEC2 := getSubnetsFromEC2

	// Set default mock behavior
	getConfigFromIMDSv2Func = func() (string, string, error) {
		return "", "", errors.New("IMDS not available in test")
	}
	getSubnetsFromEC2Func = func(ctx context.Context, azID string, region string) ([]types.Subnet, error) {
		return nil, errors.New("EC2 not available in test")
	}

	// The t.Cleanup function registers a function to be called when the test
	// and all its subtests complete. This is a perfect way to ensure our
	// original functions are restored.
	t.Cleanup(func() {
		getConfigFromIMDSv2Func = origIMDS
		getSubnetsFromEC2Func = origEC2
	})
}

func TestSetup(t *testing.T) {
	// Store original functions before any tests run
	origIMDS := getConfigFromIMDSv2Func
	origEC2 := getSubnetsFromEC2Func

	// Restore original functions when all tests in this file are done
	t.Cleanup(func() {
		getConfigFromIMDSv2Func = origIMDS
		getSubnetsFromEC2Func = origEC2
	})

	tests := []struct {
		name          string
		corefile      string
		awsZoneIDEnv  string // To mock os.Getenv("AWS_ZONE_ID")
		mockIMDS      func() (string, string, error)
		mockEC2       func(ctx context.Context, azID string, region string) ([]types.Subnet, error)
		expectedErr   string
		expectPlugin  bool
		expectedCIDRs []string
	}{
		{
			name:         "Basic valid config from Corefile with IMDS",
			corefile:     `zoneawareness use1-az1 192.168.1.0/24 10.0.0.0/8`,
			mockIMDS:     func() (string, string, error) { return "use1-az1", "us-east-1", nil },
			expectPlugin: true,
			expectedCIDRs: []string{
				"192.168.1.0/24",
				"10.0.0.0/8",
			},
		},
		{
			name:         "Config for different zone is ignored",
			corefile:     `zoneawareness usw2-az2 192.168.1.0/24`,
			mockIMDS:     func() (string, string, error) { return "use1-az1", "us-east-1", nil },
			expectPlugin: false, // Plugin should not be added as no CIDRs for the current zone.
		},
		{
			name:         "Invalid CIDR is skipped",
			corefile:     `zoneawareness use1-az1 192.168.1.0/24 10.0.0.0/33 172.16.0.0/16`,
			mockIMDS:     func() (string, string, error) { return "use1-az1", "us-east-1", nil },
			expectPlugin: true,
			expectedCIDRs: []string{
				"192.168.1.0/24",
				"172.16.0.0/16",
			},
		},
		{
			name:         "Invalid AWS Zone ID format is skipped",
			corefile:     `zoneawareness my-invalid-zone 192.168.1.0/24`,
			mockIMDS:     func() (string, string, error) { return "use1-az1", "us-east-1", nil },
			expectPlugin: false,
		},
		{
			name:         "No IMDS, but AWS_ZONE_ID env var is used",
			corefile:     `zoneawareness use1-az1 192.168.1.0/24`,
			awsZoneIDEnv: "use1-az1",
			mockIMDS:     func() (string, string, error) { return "", "", errors.New("no imds") },
			expectPlugin: true,
			expectedCIDRs: []string{
				"192.168.1.0/24",
			},
		},
		{
			name:         "No IMDS and no AWS_ZONE_ID env var",
			corefile:     `zoneawareness use1-az1 192.168.1.0/24`,
			mockIMDS:     func() (string, string, error) { return "", "", errors.New("no imds") },
			expectPlugin: false,
		},
		{
			name:     "Auto-discovery from EC2 and Corefile config are combined",
			corefile: `zoneawareness use1-az1 10.0.2.0/24`,
			mockIMDS: func() (string, string, error) { return "use1-az1", "us-east-1", nil },
			mockEC2: func(ctx context.Context, azID string, region string) ([]types.Subnet, error) {
				if azID == "use1-az1" {
					return []types.Subnet{
						{SubnetId: aws.String("subnet-1"), CidrBlock: aws.String("10.0.1.0/24")},
						{SubnetId: aws.String("subnet-2"), Ipv6CidrBlockAssociationSet: []types.SubnetIpv6CidrBlockAssociation{
							{Ipv6CidrBlock: aws.String("2001:db8::/64")},
						}},
					}, nil
				}
				return nil, nil
			},
			expectPlugin: true,
			expectedCIDRs: []string{
				"10.0.1.0/24",
				"2001:db8::/64",
				"10.0.2.0/24",
			},
		},
		{
			name:     "EC2 subnet discovery fails, but Corefile config is still used",
			corefile: `zoneawareness use1-az1 10.0.2.0/24`,
			mockIMDS: func() (string, string, error) { return "use1-az1", "us-east-1", nil },
			mockEC2: func(ctx context.Context, azID string, region string) ([]types.Subnet, error) {
				return nil, errors.New("failed to describe subnets")
			},
			expectPlugin: true,
			expectedCIDRs: []string{
				"10.0.2.0/24",
			},
		},
		{
			name:         "Empty config block, no IMDS, no env var",
			corefile:     `zoneawareness`,
			mockIMDS:     func() (string, string, error) { return "", "", errors.New("no imds") },
			expectPlugin: false,
		},
		{
			name:         "Config with only zone name",
			corefile:     `zoneawareness use1-az1`,
			mockIMDS:     func() (string, string, error) { return "use1-az1", "us-east-1", nil },
			expectPlugin: false, // No CIDRs provided
		},
		{
			name:     "EC2 subnet with no CIDR block is skipped",
			corefile: `zoneawareness use1-az1 10.0.2.0/24`,
			mockIMDS: func() (string, string, error) { return "use1-az1", "us-east-1", nil },
			mockEC2: func(ctx context.Context, azID string, region string) ([]types.Subnet, error) {
				return []types.Subnet{
					{SubnetId: aws.String("subnet-1"), CidrBlock: aws.String("10.0.1.0/24")},
					{SubnetId: aws.String("subnet-no-cidr")}, // This subnet has no CIDR and should be ignored
				}, nil
			},
			expectPlugin: true,
			expectedCIDRs: []string{
				"10.0.1.0/24",
				"10.0.2.0/24",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setupTest(t) // Setup mocks

			// Apply test-case specific mocks
			if tc.mockIMDS != nil {
				getConfigFromIMDSv2Func = tc.mockIMDS
			}
			if tc.mockEC2 != nil {
				getSubnetsFromEC2Func = tc.mockEC2
			}
			if tc.awsZoneIDEnv != "" {
				t.Setenv("AWS_ZONE_ID", tc.awsZoneIDEnv)
			}

			c := caddy.NewTestController("dns", tc.corefile)
			err := setup(c)

			if tc.expectedErr != "" {
				if err == nil {
					t.Fatalf("Expected error containing '%s', but got nil", tc.expectedErr)
				}
				if !strings.Contains(err.Error(), tc.expectedErr) {
					t.Fatalf("Expected error containing '%s', but got: %v", tc.expectedErr, err)
				}
				return // Error was expected, test is done.
			}

			if err != nil {
				t.Fatalf("Expected no error, but got: %v", err)
			}

			// Check if the plugin was added to the server block
			plugins := dnsserver.GetConfig(c).Plugin
			var za *Zoneawareness
			if len(plugins) > 0 {
				// The plugin is added via a closure, so we need to get the handler
				// and type-assert it. The handler should be our Zoneawareness struct.
				handler := plugins[0](nil) // Pass nil for the next handler
				var ok bool
				za, ok = handler.(*Zoneawareness)
				if !ok {
					if tc.expectPlugin {
						t.Fatalf("Expected plugin of type *Zoneawareness but got %T", handler)
					}
				}
			}

			if !tc.expectPlugin {
				if za != nil {
					t.Fatal("Expected no plugin to be added, but it was")
				}
				return // Test finished
			}

			if za == nil {
				t.Fatal("Expected plugin to be added, but it wasn't")
			}

			// Check the configured CIDRs
			currentZone, ok := za.Zones[za.currentAvailabilityZoneId]
			if !ok {
				t.Fatalf("Expected zone '%s' to be in the Zones map, but it wasn't", za.currentAvailabilityZoneId)
			}

			if len(currentZone.CIDRs) != len(tc.expectedCIDRs) {
				t.Fatalf("Expected %d CIDRs, but got %d. Got: %v", len(tc.expectedCIDRs), len(currentZone.CIDRs), currentZone.CIDRs)
			}

			// Convert net.IPNet to string for easy comparison
			gotCIDRs := make(map[string]struct{})
			for _, cidr := range currentZone.CIDRs {
				gotCIDRs[cidr.String()] = struct{}{}
			}

			for _, expectedCIDR := range tc.expectedCIDRs {
				if _, found := gotCIDRs[expectedCIDR]; !found {
					t.Errorf("Expected CIDR '%s' was not found in the configured list", expectedCIDR)
				}
			}
		})
	}
}
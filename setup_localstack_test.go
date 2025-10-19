package zoneawareness

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
)

// TestSetupWithLocalStack performs an integration test against a running LocalStack container.
// To run this test, start LocalStack (e.g., `docker-compose up`) and then run:
// go test -v -run TestSetupWithLocalStack
//
// This test verifies the auto-discovery of subnets from the EC2 API.
func TestSetupWithLocalStack(t *testing.T) {
	// Skip this test in short mode, as it requires external services.
	if testing.Short() {
		t.Skip("Skipping integration test in short mode.")
	}

	// --- Test Setup ---
	const (
		region     = "us-east-1"
		azID       = "use1-az1"
		vpcCIDR    = "10.0.0.0/16"
		subnetCIDR = "10.0.1.0/24"
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create an EC2 client configured to talk to LocalStack
	ec2Client, err := newLocalStackEC2Client(ctx, region)
	if err != nil {
		t.Fatalf("Failed to create EC2 client for LocalStack: %v", err)
	}

	// Create a VPC and Subnet in LocalStack for the test
	vpcID, subnetID := setupVPCAndSubnet(ctx, t, ec2Client, vpcCIDR, subnetCIDR, azID)

	// --- Mock Dependencies & Run Plugin Setup ---

	// 1. Mock the IMDS call to return our test AZ and Region
	originalIMDSFunc := getConfigFromIMDSv2Func
	getConfigFromIMDSv2Func = func() (string, string, error) {
		return azID, region, nil
	}
	// 2. Ensure the real EC2 function is used
	originalEC2Func := getSubnetsFromEC2Func
	getSubnetsFromEC2Func = getSubnetsFromEC2

	t.Cleanup(func() {
		// Restore original functions after the test
		getConfigFromIMDSv2Func = originalIMDSFunc
		getSubnetsFromEC2Func = originalEC2Func
		// Clean up AWS resources from LocalStack
		cleanupVPCAndSubnet(context.Background(), t, ec2Client, vpcID, subnetID)
	})

	// Set environment variables required by the AWS SDK to connect to LocalStack
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", region)
	// This is a custom endpoint for the EC2 service, pointing to LocalStack.
	// The AWS Go SDK v2 automatically uses this.
	os.Setenv("AWS_ENDPOINT_URL_EC2", "http://localhost:4566")
	defer os.Unsetenv("AWS_ENDPOINT_URL_EC2")

	// --- Execute Test ---
	// The Corefile is empty because we want to test auto-discovery, not static config.
	c := caddy.NewTestController("dns", "zoneawareness")
	err = setup(c)
	if err != nil {
		t.Fatalf("setup() returned an unexpected error: %v", err)
	}

	// --- Assertions ---
	plugins := dnsserver.GetConfig(c).Plugin
	if len(plugins) == 0 {
		t.Fatal("Expected plugin to be added, but it wasn't.")
	}

	handler := plugins[0](nil) // The plugin is added via a closure
	za, ok := handler.(*Zoneawareness)
	if !ok {
		t.Fatalf("Expected plugin of type *Zoneawareness but got %T", handler)
	}

	// Verify that the current zone and its CIDR were correctly discovered
	if za.currentAvailabilityZoneId != azID {
		t.Errorf("Expected current zone to be '%s', but got '%s'", azID, za.currentAvailabilityZoneId)
	}

	currentZone, ok := za.Zones[azID]
	if !ok {
		t.Fatalf("Expected zone '%s' to be configured, but it was not found.", azID)
	}

		found := false
	for _, prefix := range currentZone.CIDRs {
		if prefix.String() == subnetCIDR {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Expected discovered CIDR to be '%s', but it was not found in the list of discovered CIDRs: %v", subnetCIDR, currentZone.CIDRs)
	}

	t.Logf("Successfully verified that subnet '%s' was discovered from LocalStack in AZ '%s'", subnetCIDR, azID)
}

// TestSetupWithLocalStack_MultiAZ tests subnet discovery across multiple Availability Zones,
// with multiple VPCs and a mix of IPv4 and IPv6 subnets. It verifies that the plugin
// only discovers subnets within the same AZ as the simulated execution environment.
func TestSetupWithLocalStack_MultiAZ(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode.")
	}

	// --- Test Setup ---
	const (
		region          = "us-east-1"
		executionAZ     = "use1-az2" // The AZ where the plugin is "running"
		otherAZ1        = "use1-az1"
		otherAZ3        = "use1-az3"
		vpc1CIDR        = "10.1.0.0/16"
		vpc2CIDR        = "10.2.0.0/16"
		vpc3CIDR        = "2001:db8:1::/48"
		expectedSubnet1 = "10.1.1.0/24"
		expectedSubnet2 = "10.2.1.0/24"
		expectedSubnet3 = "2001:db8:1:1::/64" // IPv6
	)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ec2Client, err := newLocalStackEC2Client(ctx, region)
	if err != nil {
		t.Fatalf("Failed to create EC2 client for LocalStack: %v", err)
	}

	// --- Create Network Resources ---
	vpc1ID := setupVPC(ctx, t, ec2Client, vpc1CIDR, false)
	vpc2ID := setupVPC(ctx, t, ec2Client, vpc2CIDR, false)
	vpc3ID := setupVPC(ctx, t, ec2Client, vpc3CIDR, true)

	// Subnets that should be discovered (in executionAZ)
	subnet1ID := setupSubnet(ctx, t, ec2Client, vpc1ID, expectedSubnet1, executionAZ, false)
	subnet2ID := setupSubnet(ctx, t, ec2Client, vpc2ID, expectedSubnet2, executionAZ, false)
	subnet3ID := setupSubnet(ctx, t, ec2Client, vpc3ID, expectedSubnet3, executionAZ, true)

	// Subnets that should be ignored (in other AZs)
	ignoredSubnet1ID := setupSubnet(ctx, t, ec2Client, vpc1ID, "10.1.2.0/24", otherAZ1, false)
	ignoredSubnet2ID := setupSubnet(ctx, t, ec2Client, vpc2ID, "10.2.2.0/24", otherAZ3, false)
	ignoredSubnet3ID := setupSubnet(ctx, t, ec2Client, vpc3ID, "2001:db8:1:2::/64", otherAZ1, true)

	// --- Mock Dependencies & Run Plugin Setup ---
	originalIMDSFunc := getConfigFromIMDSv2Func
	getConfigFromIMDSv2Func = func() (string, string, error) {
		return executionAZ, region, nil
	}
	originalEC2Func := getSubnetsFromEC2Func
	getSubnetsFromEC2Func = getSubnetsFromEC2

	t.Cleanup(func() {
		getConfigFromIMDSv2Func = originalIMDSFunc
		getSubnetsFromEC2Func = originalEC2Func

		cleanupCtx := context.Background()
		// Cleanup is LIFO: subnets first, then VPCs
		cleanupSubnet(cleanupCtx, t, ec2Client, subnet1ID)
		cleanupSubnet(cleanupCtx, t, ec2Client, subnet2ID)
		cleanupSubnet(cleanupCtx, t, ec2Client, subnet3ID)
		cleanupSubnet(cleanupCtx, t, ec2Client, ignoredSubnet1ID)
		cleanupSubnet(cleanupCtx, t, ec2Client, ignoredSubnet2ID)
		cleanupSubnet(cleanupCtx, t, ec2Client, ignoredSubnet3ID)
		cleanupVPC(cleanupCtx, t, ec2Client, vpc1ID)
		cleanupVPC(cleanupCtx, t, ec2Client, vpc2ID)
		cleanupVPC(cleanupCtx, t, ec2Client, vpc3ID)
	})

	// Set environment variables for LocalStack
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", region)
	os.Setenv("AWS_ENDPOINT_URL_EC2", "http://localhost:4566")
	defer os.Unsetenv("AWS_ENDPOINT_URL_EC2")

	// --- Execute Test ---
	c := caddy.NewTestController("dns", "zoneawareness")
	err = setup(c)
	if err != nil {
		t.Fatalf("setup() returned an unexpected error: %v", err)
	}

	// --- Assertions ---
	plugins := dnsserver.GetConfig(c).Plugin
	if len(plugins) == 0 {
		t.Fatal("Expected plugin to be added, but it wasn't.")
	}

	handler := plugins[0](nil)
	za, ok := handler.(*Zoneawareness)
	if !ok {
		t.Fatalf("Expected plugin of type *Zoneawareness but got %T", handler)
	}

	// 1. Verify the current AZ ID is correct
	if za.currentAvailabilityZoneId != executionAZ {
		t.Errorf("Expected current zone to be '%s', but got '%s'", executionAZ, za.currentAvailabilityZoneId)
	}

	// 2. Verify that ONLY the execution AZ was discovered
	if len(za.Zones) != 1 {
		t.Fatalf("Expected 1 zone to be discovered, but found %d zones: %v", len(za.Zones), za.Zones)
	}
	currentZone, ok := za.Zones[executionAZ]
	if !ok {
		t.Fatalf("Expected zone '%s' to be configured, but it was not found.", executionAZ)
	}

	// 3. Verify that all subnets created for the execution AZ were discovered.
	//    Other subnets (default ones, dummy IPv4 blocks) are ignored.
	expectedCIDRs := map[string]bool{
		expectedSubnet1: false,
		expectedSubnet2: false,
		expectedSubnet3: false,
	}

	discoveredCIDRsForLog := []string{}
	for _, prefix := range currentZone.CIDRs {
		discoveredCIDRsForLog = append(discoveredCIDRsForLog, prefix.String())
		if _, exists := expectedCIDRs[prefix.String()]; exists {
			expectedCIDRs[prefix.String()] = true
		}
	}
	t.Logf("Discovered CIDRs in zone %s: %v", executionAZ, discoveredCIDRsForLog)

	allFound := true
	for cidr, found := range expectedCIDRs {
		if !found {
			t.Errorf("Expected CIDR '%s' was not discovered.", cidr)
			allFound = false
		}
	}

	if !allFound {
		t.Fatal("Not all expected CIDRs were discovered.")
	}

	t.Logf("Successfully verified discovery of expected subnets in execution AZ '%s'", executionAZ)
	t.Logf("Successfully verified that subnets from other AZs were ignored.")
}

// newLocalStackEC2Client creates an AWS EC2 client configured for LocalStack.
func newLocalStackEC2Client(ctx context.Context, region string) (*ec2.Client, error) {
	resolver := aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
		return aws.Endpoint{
			URL:           "http://localhost:4566",
			SigningRegion: region,
		}, nil
	})

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithEndpointResolverWithOptions(resolver),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "test")),
	)
	if err != nil {
		return nil, err
	}

	// Check if LocalStack is reachable
	client := ec2.NewFromConfig(cfg)
	_, err = client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{})
	if err != nil {
		return nil, errors.New("LocalStack not reachable. Make sure it's running. Error: " + err.Error())
	}

	return client, nil
}

// setupVPCAndSubnet creates a VPC and a subnet in LocalStack.
func setupVPCAndSubnet(ctx context.Context, t *testing.T, client *ec2.Client, vpcCIDR, subnetCIDR, azID string) (string, string) {
	t.Helper()
	vpcID := setupVPC(ctx, t, client, vpcCIDR, false)
	subnetID := setupSubnet(ctx, t, client, vpcID, subnetCIDR, azID, false)
	return vpcID, subnetID
}

// cleanupVPCAndSubnet deletes the created subnet and VPC.
func cleanupVPCAndSubnet(ctx context.Context, t *testing.T, client *ec2.Client, vpcID, subnetID string) {
	t.Helper()
	cleanupSubnet(ctx, t, client, subnetID)
	cleanupVPC(ctx, t, client, vpcID)
}

// setupVPC creates a VPC in LocalStack.
func setupVPC(ctx context.Context, t *testing.T, client *ec2.Client, cidr string, isIPv6 bool) string {
	t.Helper()
	var input ec2.CreateVpcInput
	if isIPv6 {
		// LocalStack's CreateVpc seems to require an IPv4 CIDR even when creating an IPv6-only VPC.
		// We provide a dummy, non-overlapping CIDR for the IPv4 block.
		input = ec2.CreateVpcInput{
			CidrBlock:     aws.String("10.255.255.0/24"),
			Ipv6CidrBlock: aws.String(cidr),
		}
	} else {
		input = ec2.CreateVpcInput{
			CidrBlock: aws.String(cidr),
		}
	}

	vpcOut, err := client.CreateVpc(ctx, &input)
	if err != nil {
		t.Fatalf("Failed to create VPC in LocalStack with CIDR %s: %v", cidr, err)
	}
	vpcID := *vpcOut.Vpc.VpcId
	t.Logf("Created VPC %s with CIDR %s", vpcID, cidr)
	return vpcID
}

// setupSubnet creates a subnet in the given VPC.
func setupSubnet(ctx context.Context, t *testing.T, client *ec2.Client, vpcID, cidr, azID string, isIPv6 bool) string {
	t.Helper()
	var input ec2.CreateSubnetInput
	if isIPv6 {
		// A dummy IPv4 block is needed for LocalStack. It must be unique per subnet in the VPC.
		// We can derive a unique CIDR from the IPv6 CIDR to ensure it doesn't collide.
		ip, _, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("Failed to parse CIDR for dummy generation: %v", err)
		}
		dummyIPv4CIDR := fmt.Sprintf("10.255.255.%d/28", ip.To16()[7]*16+ip.To16()[15])

		input = ec2.CreateSubnetInput{
			VpcId:              aws.String(vpcID),
			CidrBlock:          aws.String(dummyIPv4CIDR),
			Ipv6CidrBlock:      aws.String(cidr),
			AvailabilityZoneId: aws.String(azID),
		}
	} else {
		input = ec2.CreateSubnetInput{
			VpcId:              aws.String(vpcID),
			CidrBlock:          aws.String(cidr),
			AvailabilityZoneId: aws.String(azID),
		}
	}

	subnetOut, err := client.CreateSubnet(ctx, &input)
	if err != nil {
		t.Fatalf("Failed to create Subnet in LocalStack with CIDR %s: %v", cidr, err)
	}
	subnetID := *subnetOut.Subnet.SubnetId
	t.Logf("Created Subnet %s in AZ %s with CIDR %s", subnetID, azID, cidr)
	return subnetID
}

// cleanupSubnet deletes a subnet.
func cleanupSubnet(ctx context.Context, t *testing.T, client *ec2.Client, subnetID string) {
	t.Helper()
	_, err := client.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: aws.String(subnetID)})
	if err != nil {
		t.Logf("Warning: failed to delete subnet %s: %v", subnetID, err)
	} else {
		t.Logf("Deleted subnet %s", subnetID)
	}
}

// cleanupVPC deletes a VPC.
func cleanupVPC(ctx context.Context, t *testing.T, client *ec2.Client, vpcID string) {
	t.Helper()
	_, err := client.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)})
	if err != nil {
		t.Logf("Warning: failed to delete vpc %s: %v", vpcID, err)
	} else {
		t.Logf("Deleted VPC %s", vpcID)
	}
}

// This is a dummy test to ensure net.ParseCIDR is imported if no other test uses it.
func TestDummyForImports(t *testing.T) {
	_, _, _ = net.ParseCIDR("127.0.0.1/32")
}

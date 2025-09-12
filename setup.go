package zoneawareness

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/coredns/caddy"

	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
	clog "github.com/coredns/coredns/plugin/pkg/log"
)

/*
 RDS Aurora does not display all IPs for their reader endpoint
 Not for Redis or Valkey either.
 But for VPC Endpoints, it does show all IPs. Requires Private DNS name enabled.
  -> But then cross-AZ traffic is not billed, so no cost savings.
 NLB supports this kinda already via "Availability Zone routing configuration", but in EKS a CoreDNS instance in another zone might resolve to the "wrong" IP. Also "Any Availability Zone - Default" is the default response.
 ALB and CLB does return all IPs
*/

var log = clog.NewWithPlugin(pluginName)

// init registers this plugin.
func init() { plugin.Register(pluginName, setup) }

// Regex pattern for AWS Availability Zone IDs (e.g., use2-az1, euw1-az2, apse1-az3)
// https://docs.aws.amazon.com/global-infrastructure/latest/regions/aws-availability-zones.html
// https://docs.aws.amazon.com/local-zones/latest/ug/available-local-zones.html
var awsZoneIDPattern = regexp.MustCompile(`^[a-z]{2,4}[0-9](-[a-z]{3}[0-9])?-az[0-9]$`)

const pluginName = "zoneawareness"

// setup is the function that gets called when the config parser see the token "zoneawareness". Setup is responsible
// for parsing any extra options the zoneawareness plugin may have. The first token this function sees is "zoneawareness".
//
// It can be configured in Corefile like this
// zoneawareness use2-az3 100.111.97.0/24
// zoneawareness use2-az2 100.111.98.0/24 100.111.99.0/24
// zoneawareness use2-az1 23.192.228.0/24
func setup(c *caddy.Controller) error {
	l := &Zoneawareness{Zones: make(map[string]*Zone), currentAvailabilityZoneId: ""}

	// Attempt to fetch Availability Zone ID and Region from EC2 IMDSv2
	instanceAvailabilityZoneId, instanceRegion, err := getConfigFromIMDSv2Func()
	if err != nil {
		log.Infof("Could not fetch AZ and Region from IMDSv2: %v. Will rely on other configuration methods.", err)
	} else if instanceAvailabilityZoneId != "" && instanceRegion != "" {
		l.currentAvailabilityZoneId = instanceAvailabilityZoneId
		log.Infof("Successfully fetched placement/availability-zone-id '%s' and region '%s' from EC2 IMDSv2.", l.currentAvailabilityZoneId, instanceRegion)

		// Describe subnets using the discovered AZ and Region
		subnets, err := getSubnetsFromEC2Func(context.Background(), l.currentAvailabilityZoneId, instanceRegion)
		if err != nil {
			log.Errorf("Failed to describe subnets: %v", err)
			// Do not return error, just log and continue without subnets
			// This means the plugin will still be active, but without auto-discovered subnets.
		} else {
			// Add subnets to the zone
			for _, subnet := range subnets {
				// Process IPv4 CIDR block
				if subnet.CidrBlock != nil && *subnet.CidrBlock != "" {
					cidrStr := *subnet.CidrBlock
					_, parsedCIDR, parseErr := net.ParseCIDR(cidrStr)
					if parseErr != nil {
						log.Warningf("Invalid IPv4 CIDR format for subnet %s (%s): %v", *subnet.SubnetId, cidrStr, parseErr)
					} else {
						zone, exists := l.Zones[l.currentAvailabilityZoneId]
						if !exists {
							log.Infof("Adding new zone '%s'", l.currentAvailabilityZoneId)
							zone = &Zone{}
							l.Zones[l.currentAvailabilityZoneId] = zone
						}
						zone.CIDRs = append(zone.CIDRs, parsedCIDR)
						log.Infof("%s added to zone '%s' from subnet %s", cidrStr, l.currentAvailabilityZoneId, *subnet.SubnetId)
					}
				}

				// Process IPv6 CIDR blocks
				for _, ipv6Assoc := range subnet.Ipv6CidrBlockAssociationSet {
					if ipv6Assoc.Ipv6CidrBlock != nil && *ipv6Assoc.Ipv6CidrBlock != "" {
						cidrStr := *ipv6Assoc.Ipv6CidrBlock
						_, parsedCIDR, parseErr := net.ParseCIDR(cidrStr)
						if parseErr != nil {
							log.Warningf("Invalid IPv6 CIDR format for subnet %s (%s): %v", *subnet.SubnetId, cidrStr, parseErr)
						} else {
							zone, exists := l.Zones[l.currentAvailabilityZoneId]
							if !exists {
								log.Infof("Adding new zone '%s'", l.currentAvailabilityZoneId)
								zone = &Zone{}
								l.Zones[l.currentAvailabilityZoneId] = zone
							}
							zone.CIDRs = append(zone.CIDRs, parsedCIDR)
							log.Infof("%s added to zone '%s' from subnet %s", cidrStr, l.currentAvailabilityZoneId, *subnet.SubnetId)
						}
					}
				}
			}
		}
	}

	// Alternatively, check environment variable AWS_ZONE_ID
	if l.currentAvailabilityZoneId == "" {
		if awsZoneIDPattern.MatchString(os.Getenv("AWS_ZONE_ID")) {
			l.currentAvailabilityZoneId = os.Getenv("AWS_ZONE_ID")
			log.Infof("Using AWS_ZONE_ID environment variable: %s", l.currentAvailabilityZoneId)
		}
	}

	if l.currentAvailabilityZoneId == "" {
		log.Infof("No valid AWS Zone ID found from IMDSv2 or environment variable. Zoneawareness plugin will not be active.")
		return nil
	}

	// Parse arguments from Corefile if present
	for c.Next() {
		args := c.RemainingArgs()

		if len(args) >= 2 {
			zoneName := args[0]

			// If the zone name is not the current zone, skip adding it
			// Should reduces lookup time
			if zoneName != l.currentAvailabilityZoneId {
				log.Infof("Zone %s ignored", zoneName)
				continue
			}

			// Validate the zone name against the AWS Zone ID pattern
			if !awsZoneIDPattern.MatchString(zoneName) {
				log.Warningf("Invalid AWS Zone ID format for '%s'. Expected format like 'use2-az1'.", zoneName)
				continue
				// return plugin.Error("zoneawareness", c.Errf("invalid AWS Zone ID format for '%s'. Expected format like 'use2-az1'.", zoneName))
			}

			cidrArgs := args[1:] // All remaining arguments are potential CIDRs
			// Process all CIDR arguments for this zoneName
			for _, cidrStr := range cidrArgs {
				_, cidr, err := net.ParseCIDR(cidrStr)
				if err != nil {
					log.Warningf("Invalid data for zone '%s': %v", zoneName, err)
					continue
					//return plugin.Error("zoneawareness", c.Errf("invalid CIDR format for zone '%s': %v", zoneName, err))
				}

				zone, exists := l.Zones[zoneName]
				if !exists {
					log.Infof("Adding new zone '%s'", zoneName)
					zone = &Zone{}
					l.Zones[zoneName] = zone
				}
				zone.CIDRs = append(zone.CIDRs, cidr)
				log.Infof("Added %s to zone '%s'", cidrStr, zoneName)
			}
		}
	}

	// Conditionally add the plugin to the chain.
	if currentZoneData, ok := l.Zones[l.currentAvailabilityZoneId]; ok && len(currentZoneData.CIDRs) > 0 {
		dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
			log.Infof("Plugin added for current zone '%s' with %d CIDR(s).", l.currentAvailabilityZoneId, len(currentZoneData.CIDRs))
			l.HasSynced = true // Mark as synced now that it's successfully configured and being added
			l.Next = next
			for _, cidr := range currentZoneData.CIDRs {
				log.Debugf("%s", cidr.String())
			}
			return l
		})
	} else {
		log.Infof("Zoneawareness plugin NOT added: No CIDRs were configured or found for the current operational zone '%s'.", l.currentAvailabilityZoneId)
	}
	return nil
}

var (
	getConfigFromIMDSv2Func = getConfigFromIMDSv2
	getSubnetsFromEC2Func   = getSubnetsFromEC2
)

// getConfigFromIMDSv2 fetches the availability zone from AWS EC2 IMDSv2.
func getConfigFromIMDSv2() (string, string, error) {
	const imdsTimeout = 2 * time.Second // Short timeout to fail fast

	ctx, cancel := context.WithTimeout(context.Background(), imdsTimeout)
	defer cancel()

	// Load default AWS configuration. The IMDS client will use this to find credentials
	// and region if necessary, though for IMDS it primarily needs to know it's on EC2.
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return "", "", fmt.Errorf("failed to load AWS SDK config: %w", err)
	}

	// Create an IMDS client
	client := imds.NewFromConfig(cfg)

	// Get Availability Zone ID
	azOutput, err := client.GetMetadata(ctx, &imds.GetMetadataInput{
		Path: "placement/availability-zone-id",
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to get AZ ID from IMDS (instance may not be EC2 or IMDSv2 disabled/misconfigured): %w", err)
	}
	defer azOutput.Content.Close()

	azIDBytes, err := io.ReadAll(azOutput.Content)
	if err != nil {
		return "", "", fmt.Errorf("failed to read AZ ID from IMDS response body: %w", err)
	}
	azID := strings.TrimSpace(string(azIDBytes))

	if !awsZoneIDPattern.MatchString(azID) {
		return "", "", fmt.Errorf("fetched AZ ID '%s' from IMDS has an invalid format", azID)
	}

	// Get Region
	regionOutput, err := client.GetMetadata(ctx, &imds.GetMetadataInput{
		Path: "placement/region",
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to get region from IMDS: %w", err)
	}
	defer regionOutput.Content.Close()

	regionBytes, err := io.ReadAll(regionOutput.Content)
	if err != nil {
		return "", "", fmt.Errorf("failed to read region from IMDS response body: %w", err)
	}
	region := strings.TrimSpace(string(regionBytes))

	return azID, region, nil
}

// getSubnetsFromEC2 fetches subnets from the AWS EC2 API, filtered by Availability Zone ID.
func getSubnetsFromEC2(ctx context.Context, azID string, region string) ([]types.Subnet, error) {
	// Load default AWS configuration. This will automatically try to use IMDS for credentials and region.
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS SDK config: %w", err)
	}

	// Create an EC2 client
	ec2Client := ec2.NewFromConfig(cfg)

	// Describe subnets, filtering by the provided Availability Zone ID
	output, err := ec2Client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("availability-zone-id"),
				Values: []string{azID},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe subnets for AZ ID '%s': %w", azID, err)
	}

	return output.Subnets, nil
}

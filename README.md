# zoneawareness - CoreDNS plugin to reorder DNS answers according to zonal preference 

`zonewareness` is a CoreDNS plugins that attempts to keep client-server traffic within the same availability zone in AWS (or on-prem), by reordering the DNS response to prioritize local IPs. This reduces latency by avoiding un-necessary crossing the AZ network boundaryies, and avoiding cross-AZ data transfer cost.

It achieves this by describing the subnets in the current VPC, mapping each CIDR to an AZ. CoreDNS is then able to sort the answer section for DNS responses, returning IPs that is physically closer first.

Example:
```shell
# The answer section is random for each query without the plugin
(host in eu-central-1, az1) $ dig alb-or-similar-multi-az-service.eu-central-1.elb.amazonaws.com
[...]
;; ANSWER SECTION:
alb-or-similar-multi-az-service.eu-central-1.elb.amazonaws.com. 60 IN A 192.168.0.246 (euc1-az2)
alb-or-similar-multi-az-service.eu-central-1.elb.amazonaws.com. 60 IN A 192.168.1.68 (euc1-az3)
alb-or-similar-multi-az-service.eu-central-1.elb.amazonaws.com. 60 IN A 192.168.2.15 (euc1-az1)

# AFTER enabling the plugin, the answer section will always return local IPs first
(host in eu-central-1, az1) $ dig alb-or-similar-multi-az-service.eu-central-1.elb.amazonaws.com
[...]
;; ANSWER SECTION:
alb-or-similar-multi-az-service.eu-central-1.elb.amazonaws.com. 60 IN A 192.168.2.15 (euc1-az1)
alb-or-similar-multi-az-service.eu-central-1.elb.amazonaws.com. 60 IN A 192.168.0.246 (euc1-az2)
alb-or-similar-multi-az-service.eu-central-1.elb.amazonaws.com. 60 IN A 192.168.1.68 (euc1-az3)
```

Additional CIDR-to-AZ mapping can be added to the configuration to enable same-AZ communication across AWS accounts and VPC peerings, and is the preferred way to use enable this plugin for on-prem setup with multiple datacenters/zones.

Users of EKS might want to try my nodelocal dnscache container image published at this repository. 

# How can I test this in EKS environment?

Your EKS cluster must be configured to use ["NodeLocal DNSCache"](https://kubernetes.io/docs/tasks/administer-cluster/nodelocaldns/). Pre-built multi-arch images with this zoneawareness plugin for testing this is available at the release page. The only addition is the zoneawareness plugin in from this repository. 

 When NodeLocal DNSCache is deployed and working, two steps must be performed:
* Update the DaemonSet to use this customized container image
* Update Corefile to enable the `zoneawareness` plugin for the root zone (.)

1) Update your DaemonSet to use the new container image:
```sh
kubectl patch daemonset node-local-dns -n kube-system --type=strategic -p '{"spec":{"template":{"spec":{"containers":[{"name":"node-cache","image":"ghcr.io/toredash/zoneawareness/k8s-dns-node-cache:1.26.7"}]}}}}'
```

2a) Update your `Corefile` configuration file for node-local-dns (a Configmap named `node-local-dns` in `kube-system`) to enable the zoneawarness plugin:
```sh
# Make a copy of the current config:
kubectl get configmap node-local-dns -n kube-system -o yaml > node-local-dns-cm.yaml
cp node-local-dns-cm.yaml node-local-dns-cm.orig.yaml
# edit `node-local-dns-cm.orig.yaml` editor of choice to add zoneawareness to the root zone
```
Add the zoneawareness line within the root (.) zone:
```git
    .:53 {
        errors
        cache 30
+        zoneawareness
        reload
        loop
        bind 169.254.0.10 172.20.0.10
        forward . 172.20.0.10
        prometheus :9253
        }
```
2b) Apply the modified configmap:
```shell
kubectl apply -f node-local-dns-cm.orig.yaml -n kube-system
```

3) Restart the DaemonSet
```shell
kubectl rollout restart daemonset node-local-dns -n kube-system
````

If everything worked out fine, you should se similar entries in node-local-dns Pods:
```
[INFO] plugin/zoneawareness: Successfully fetched placement/availability-zone-id 'euc1-az2' and region 'eu-central-1' from EC2 IMDSv2.
[INFO] plugin/zoneawareness: Adding new zone 'euc1-az2'
[INFO] plugin/zoneawareness: 192.168.160.0/19 added to zone 'euc1-az2' from subnet subnet-07637a0dd59dc1750
[INFO] plugin/zoneawareness: 192.168.64.0/19 added to zone 'euc1-az2' from subnet subnet-06f510d4c623bd574
[INFO] plugin/zoneawareness: 172.31.16.0/20 added to zone 'euc1-az2' from subnet subnet-0ba2437591adb534a
[INFO] plugin/zoneawareness: Plugin added for current zone 'euc1-az2' with 3 CIDR(s).
```

To revert the changes:

```sh
kubectl rollout undo daemonset/node-local-dns -n kube-system
kubectl apply -f node-local-dns-cm.yaml -n kube-system # Note: applying the original ConfigMap
```

# What problem does this plugin solve ?

Short: Latency and/or cost. When traffic (un-neccessarily) has to cross AZ boundaries for services in AWS that is multi-AZ, you add additional latency and/or cost. Crossing over to another AZ, [increases latency by ~175-375%](https://github.com/toredash/automatic-zone-placement?tab=readme-ov-file#performance-impact), and data transfer can cost $0.01/GB (Region-DataTransfer-AZ-AZ-Bytes, in most regions).

Long version:

For backends that are present in multiple AZs, it becomes difficult to contain traffic from clients towards these services within the same AZ. A multi AZ service, like AWS ALB, returns the IP address for all instances in all zones, and ideally the client should be aware of which subnets maps to which zones. More often than not the client does not know which AZ it is running in, so knowing what zone a remote service is in is even harder.

If want to reduce latency and/or cost, the `zoneawareness` plugin for CoreDNS solves this problem without having to modify your application code.

Let's say you have an internal API endpoint (internal-api.corp.com) in your VPC (or peered VPC) that have IPs present in all 3x AZs in eu-north-1, a typical DNS reponse for this endpoint would look like this:
```sh
% dig internal-api.corp.com
[..]
;; ANSWER SECTION:
internal-api.corp.com. 60 IN A 192.168.118.246
internal-api.corp.com. 60 IN A 192.168.145.68
internal-api.corp.com. 60 IN A 192.168.161.15
```
Note: the hostname can be anything, the above example could be three different EC2 instances, an single AWS ALB instance or a VPC Endpoint. The requirement for this plugin to work is that -multiple- IP addresses are returned.

Three IPs are returned, and -we- know that each of those IPs are located in different subnets and AZs ([https://docs.aws.amazon.com/vpc/latest/userguide/configure-subnets.html](subnets in AWS are zonal resources)), as we created 3x /24 subnets when the VPC was first setup. But the client application does not know this, and most likely not the developers either. -Ideally-, the client should select the IP that has the "most local presence", aka. latency.

How can we do that ? By _re-ordering_ the ANSWER section to help the client application select the most optimal IP-

If the client is running in `eu-central-1a`, it would help the client if the ANSWER section went from:
```
;; ANSWER SECTION:
internal-api.corp.com. 60 IN A 192.168.118.246    <- eu-central-1c
internal-api.corp.com. 60 IN A 192.168.145.68     <- eu-central-1a
internal-api.corp.com. 60 IN A 192.168.161.15     <- eu-central-1c
```
to the optimized version, note that now eu-central-1a is listed first:
```
;; ANSWER SECTION:
internal-api.corp.com. 60 IN A 192.168.145.68     <- eu-central-1a (optimized ordering, local AZ listed first)
internal-api.corp.com. 60 IN A 192.168.118.246    <- eu-central-1c
internal-api.corp.com. 60 IN A 192.168.161.15     <- eu-central-1c
```

The client usually selects the first available record to reach out to. By doing this on the DNS level it remains transparent for the client appliation, and you still have access to the other endpoints like you normally would (for HA, redudancy and such)

Does it matter ? Depends if you care enough for latency and data-transfer cost. I've written about this before: https://github.com/toredash/automatic-zone-placement?tab=readme-ov-file#performance-impac


## How can I build my own coredns binary with this plugin ?
For EC2 and on-prem, you need to [build your own CoreDNS binary with this plugin enabled](https://coredns.io/2017/07/25/compile-time-enabling-or-disabling-plugins/#build-with-compile-time-configuration-file). The zoneawareness plugin should be listed below/after the cache plugin in [plugin.cfg](https://github.com/coredns/coredns/blob/604e1675cfe3fc87a2035ae015384b7a98df510e/plugin.cfg#L50)

## How can I build my own nodelocal dnscache contaier image with this plugin?




## Syntax

~~~ txt
zoneawareness
~~~

## Metrics

If monitoring is enabled (via the *prometheus* directive) the following metric is exported:

* `coredns_zoneawareness_request_count_total{server}` - query count to the *zoneawareness* plugin.

The `server` label indicated which server handled the request, see the *metrics* plugin for details.

## Ready

This plugin reports readiness to the ready plugin. It will be immediately ready.

## Zoneawarenesss

In this configuration, we forward all queries to 9.9.9.9 and print "zoneawareness" whenever we receive
a query.

~~~ corefile
. {
  forward . 9.9.9.9
  zoneawareness
}
~~~

Or without any external connectivity:

~~~ corefile
. {
  whoami
  zoneawareness
}
~~~

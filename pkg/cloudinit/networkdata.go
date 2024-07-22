/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloudinit

import (
	"crypto/rand"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/context"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	ipamv1 "github.com/metal3-io/ip-address-manager/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apitypes "k8s.io/apimachinery/pkg/types"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

// Enum to indicate IP protocol types
type IPProtocolType int32

const (
	IP4 IPProtocolType = iota
	IP6
)

// CloudInitNetworkConfigV1 is a cloud-init network configuration version 1.
type CloudInitNetworkConfigV1 struct {
	Network NetworkConfig `json:"network"`
}

type NetworkConfig struct {
	Version int      `json:"version"`
	Config  []Config `json:"config"`
}

type Config struct {
	Type       string   `json:"type"`
	Name       string   `json:"name"`
	MacAddress string   `json:"mac_address,omitempty"`
	MTU        int      `json:"mtu,omitempty"`
	Subnets    []Subnet `json:"subnets,omitempty"`
	AcceptRA   *bool    `json:"accept-ra,omitempty"`
}

type Subnet struct {
	Type           string   `json:"type"`
	Address        string   `json:"address,omitempty"`
	Routes         []Route  `json:"routes,omitempty"`
	DNSNameservers []string `json:"dns_nameservers,omitempty"`
	DNSSearch      []string `json:"dns_search,omitempty"`
}

type Route struct {
	Gateway string `json:"gateway"`
	Network string `json:"network"`
	Netmask string `json:"netmask"`
	Metric  int    `json:"metric,omitempty"`
}

type KubeVirtInterfaceConfig struct {
	ID                   int
	Mtu                  int
	UseDhcp              bool
	PrimaryIntf          bool
	AssociatedIPV4Pool   string
	IPPoolNamespaceName  string
	AssociatedIP6Pool    string
	IP6PoolNamespaceName string
	Intf                 kubevirtv1.Interface
}

type KubeVirtInterfaceInfo struct {
	mac     net.HardwareAddr
	ip4Info struct {
		ipv4Addr net.IPAddr
		ipv4Mask net.IPMask
		ipv4Gw   net.IPAddr
		// If the IP addresses are defined, dhcpv4 MUST be false.
		// Parsing code will ignore it in any case.
		dhcpv4 bool
	}
	// IPv6 address for interface
	ip6Info struct {
		ipv6Addr net.IPAddr
		ipv6Mask net.IPMask
		ipv6Gw   net.IPAddr
	}
	// Name to be assigned to the interface after matching by MAC/driver etc. in cloud-init
	setName string
	// MTU to be assigned to the interface
	mtu         int
	nameservers []net.IPAddr
	primaryIntf bool
}

func updateInterfaceInfoFromIPAddressFromClaim(
	ctx *context.MachineContext,
	fabricClient client.Client,
	intf *KubeVirtInterfaceInfo,
	ipProtocolType IPProtocolType,
	namespace string,
	ipClaimList []*ipamv1.IPClaim) (bool, error) {
	ipClaimName := CreateIPClaimName(ctx.KubevirtMachine.GetName(), intf.setName, ipProtocolType)
	var ipClaim *ipamv1.IPClaim = nil
	for _, ipc := range ipClaimList {
		if ipc.Name == ipClaimName {
			ipClaim = ipc
			break
		}
	}
	if ipClaim == nil {
		ctx.Info("ipClaim object is a null pointer", "ip-claim", ipClaimName)
		return true, errors.Errorf("ipClaim %s: ipClaim object is nil!", ipClaimName)
	}

	if ipClaim.Status.Address == nil {
		ctx.Info("ipClaim Status and Address object is a null pointer", "ip-claim", ipClaimName)
		return true, errors.Errorf("ipClaim %s: no ip address object is ready/associated!", ipClaimName)
	}

	// This "ip" object should be the result of an IPClaim on the associated IP-Pool
	ip := &ipamv1.IPAddress{}
	key := client.ObjectKey{
		Name:      ipClaim.Status.Address.Name,
		Namespace: namespace,
	}
	err := fabricClient.Get(ctx, key, ip)
	if err != nil {
		return false, err
	}
	if ip.Spec.Address == "" {
		ctx.Info("ipClaim object has no associated IP address", "ip-claim", ipClaimName)
		return true, errors.Errorf("ipClaim %s: no ip address associated!", ipClaimName)
	}
	nameservers := ip.Spec.DNSServers
	if ipProtocolType == IP4 {
		intf.ip4Info.dhcpv4 = false
		intf.ip4Info.ipv4Addr = net.IPAddr{IP: net.ParseIP(string(ip.Spec.Address)), Zone: ""}
		if ip.Spec.Gateway != nil {
			intf.ip4Info.ipv4Gw = net.IPAddr{IP: net.ParseIP(string(*ip.Spec.Gateway)), Zone: ""}
		} else {
			intf.ip4Info.ipv4Gw = net.IPAddr{IP: net.ParseIP("0.0.0.0"), Zone: ""}
		}
		_, mask, _ := net.ParseCIDR(fmt.Sprintf("255.255.255.255/%d", ip.Spec.Prefix))
		intf.ip4Info.ipv4Mask = mask.Mask
	} else {
		intf.ip6Info.ipv6Addr = net.IPAddr{IP: net.ParseIP(string(ip.Spec.Address)), Zone: ""}
		if ip.Spec.Gateway != nil {
			intf.ip6Info.ipv6Gw = net.IPAddr{IP: net.ParseIP(string(*ip.Spec.Gateway)), Zone: ""}
		} else {
			intf.ip6Info.ipv6Gw = net.IPAddr{IP: net.ParseIP("0:0:0:0:0:0:0:0"), Zone: ""}
		}
		_, mask, _ := net.ParseCIDR(fmt.Sprintf("::1/%d", ip.Spec.Prefix))
		intf.ip6Info.ipv6Mask = mask.Mask
	}
	for _, dnsServer := range nameservers {
		intf.nameservers = append(intf.nameservers, net.IPAddr{IP: net.ParseIP(string(dnsServer)), Zone: ""})
	}
	return false, nil
}

func GetValidIntfName(name string, id int) string {
	// We were passing "default" as the network name and we try to map network name to
	// interface name, and so the first interface was going to be renamed "default".
	// It would throw an error because the kernel would not like it.
	// https://elixir.bootlin.com/linux/v5.15.18/source/include/net/ip.h#L347
	// This would cause the cloud-init to crash-exit and deployment to fail.
	// Now, use default ==> default_<id> to work-around the issue.
	proposedIntfName := name
	invalidNames := map[string]bool{"default": true, "all": true}
	if _, found := invalidNames[proposedIntfName]; !found {
		return proposedIntfName
	}
	// something like default_0
	return fmt.Sprintf("%s_%d", proposedIntfName, id)
}

// need to generate interfaces network config json in an ordered format
func convertMapToList(intfName2Info *map[string]KubeVirtInterfaceConfig) []KubeVirtInterfaceConfig {
	length := len(*intfName2Info)
	intfOrderedList := make([]KubeVirtInterfaceConfig, length)
	for _, intfCfg := range *intfName2Info {
		if intfCfg.ID >= length {
			// PANIC check to ensure that we do NOT crash if the IDs are in wrong order.
			return nil
		}
		intfOrderedList[intfCfg.ID] = intfCfg
	}
	return intfOrderedList
}

// use the intfName2Info table to create fully formed list of interfaces
func getInterfacesInfo(
	ctx *context.MachineContext,
	fabricClient client.Client,
	intfName2Info *map[string]KubeVirtInterfaceConfig,
	ipClaimList []*ipamv1.IPClaim) ([]KubeVirtInterfaceInfo, bool, error) {
	var intfs []KubeVirtInterfaceInfo = make([]KubeVirtInterfaceInfo, len(*intfName2Info))
	intfVector := convertMapToList(intfName2Info)
	if intfVector == nil {
		return nil, false, errors.Errorf("error in interface index parsing %v", intfName2Info)
	}
	for i, intfCfg := range intfVector {
		var intf KubeVirtInterfaceInfo = KubeVirtInterfaceInfo{}
		mac, err := net.ParseMAC(intfCfg.Intf.MacAddress)
		if err != nil {
			return nil, false, err
		}
		intf.mac = mac
		intf.setName = GetValidIntfName(intfCfg.Intf.Name, intfCfg.ID)
		intf.mtu = intfCfg.Mtu
		intf.ip4Info.dhcpv4 = intfCfg.UseDhcp

		// The goal of this primary interface tag is to say "only assign v4 Gw, v6 Gw, and DNS servers to _this_ interface".
		// The entity that adds this tag MUST make sure that it is only added to only ONE interface in the VM.
		// If the tag is added to multiple interface, we will error out.
		// Another note - callers need to ensure that the IP-Pool associated with this interface MUST have v4 Gw, v6 Gw, and DNS
		// configured. If not, the VM will be unreachable after boot-up
		intf.primaryIntf = intfCfg.PrimaryIntf

		if intfCfg.AssociatedIPV4Pool != "" {
			shouldRetry, err := updateInterfaceInfoFromIPAddressFromClaim(
				ctx,
				fabricClient,
				&intf,
				IP4,
				intfCfg.IPPoolNamespaceName,
				ipClaimList)
			if shouldRetry {
				ctx.Info("Retry from updateInterfaceInfoFromIPAddressFromClaim")
				return nil, true, err
			}
			if err != nil {
				return nil, false, err
			}
		}
		if intfCfg.AssociatedIP6Pool != "" {
			shouldRetry, err := updateInterfaceInfoFromIPAddressFromClaim(
				ctx,
				fabricClient,
				&intf,
				IP6,
				intfCfg.IP6PoolNamespaceName,
				ipClaimList)
			if shouldRetry {
				ctx.Info("Retry from updateInterfaceInfoFromIPAddressFromClaim v6")
				return nil, true, err
			}
			if err != nil {
				return nil, false, err
			}
		}
		intfs[i] = intf
	}

	return intfs, false, nil
}

func getCloudInitNetworkData(networkConfigSecret *corev1.Secret) (*CloudInitNetworkConfigV1, error) {
	networkData := new(CloudInitNetworkConfigV1)
	if err := yaml.Unmarshal(networkConfigSecret.Data["networkdata"], networkData); err != nil {
		return nil, err
	}

	return networkData, nil
}

func GetCloudInitNoCloudNetworkDataV1(
	ctx *context.MachineContext,
	fabricClient client.Client,
	intfName2Info *map[string]KubeVirtInterfaceConfig,
	ipClaimList []*ipamv1.IPClaim) (string, error) {

	interfaces, shouldRetry, err := getInterfacesInfo(ctx, fabricClient, intfName2Info, ipClaimList)
	if shouldRetry || err != nil {
		ctx.Info("Failed to load network configuration")
		if err != nil {
			err = errors.Wrapf(err, "failed to load network configuration")
		} else {
			err = errors.New("failed to load network configuration")
		}
		return "", err
	}

	// Convert to netconfig and gzip and base64 encode
	return createCloudInitNetworkDataV1Yaml(interfaces), nil
}

// Checking for mac assignment against a value from kubevirtmachine will not
// help to detect already allocated addresses, this should be done against the
// value from virtualmachine. To solve the problem, we generate this only once
// and store the generated values in a dedicated secret, so that subsequent
// reconciliations will always reuse already generated values instead of
// regenerating the addresses.
func AssignRandomMac(
	intf2NameInfo *map[string]KubeVirtInterfaceConfig) (*map[string]KubeVirtInterfaceConfig, error) {
	ret := make(map[string]KubeVirtInterfaceConfig)
	for key, value := range *intf2NameInfo {
		ret[key] = value
	}
	// prefix of all MACs on this system
	prefixMac := []byte{0xaa, 0xbb, 0xcc}
	for intf, cfg := range *intf2NameInfo {
		if cfg.Intf.MacAddress != "" {
			// already has an assigned value
			continue
		}
		// generate a random Mac to assign to interface
		mac, err := GenerateRandomUnicastMac(true, prefixMac)
		if err != nil {
			return nil, err
		}
		cfg.Intf.MacAddress = mac.String()
		ret[intf] = cfg
	}
	return &ret, nil
}

func GetNetworkConfigSecret(
	ctx *context.MachineContext,
	fabricClient client.Client,
	targetNamespace string) (*corev1.Secret, error) {
	secret := new(corev1.Secret)
	secretName := apitypes.NamespacedName{
		Namespace: targetNamespace,
		Name:      *ctx.Machine.Spec.Bootstrap.DataSecretName + "-networkdata",
	}
	err := fabricClient.Get(ctx, secretName, secret)
	if err != nil {
		return nil, err
	}

	return secret, nil
}

// Map of interface name to interface information object and ID
func MapIntfNameToIntfConfig(interfaces []kubevirtv1.Interface) (*map[string]KubeVirtInterfaceConfig, error) {
	primaryInterfacesFound := 0
	interfacesWithDHCPEnabled := 0
	intfName2Info := make(map[string]KubeVirtInterfaceConfig)
	for i, intf := range interfaces {
		_, isIntfAlreadyPresent := intfName2Info[intf.Name]
		if isIntfAlreadyPresent {
			return nil, errors.Errorf("multiple interfaces with same name found")
		}
		// Parse the Tag value and determine the IP-Pool if any
		useDhcp, associatedIPPool, associatedIPv6Pool, mtu, primaryIntf := parseIntfTag(intf.Tag)

		if primaryIntf {
			primaryInterfacesFound++
		}
		if useDhcp {
			interfacesWithDHCPEnabled++
		}

		// Does the IP-Pool name have an associated Namespace of the form NamespaceName/PoolName?
		// If yes, then split and use that namespace for IPClaim generation
		v4NameSpace, v4Pool := SplitBySlash(associatedIPPool)
		v6NameSpace, v6Pool := SplitBySlash(associatedIPv6Pool)

		// This is the name we will push into NetworkConfig v2 to rename interface inside VM.
		// interfaceName is also relevant because it is used to Key into the list of Networks
		// and then find the Network Name and corresponding Pool-Name. The pool-name allows
		// us to create an ip-claim on that pool, which in-turn generates a ip-address object
		// which we use to assign to the interface inside the VM
		intfName2Info[intf.Name] = KubeVirtInterfaceConfig{
			ID:                   i,
			Intf:                 intf,
			AssociatedIPV4Pool:   v4Pool,
			IPPoolNamespaceName:  v4NameSpace,
			AssociatedIP6Pool:    v6Pool,
			IP6PoolNamespaceName: v6NameSpace,
			UseDhcp:              useDhcp,
			Mtu:                  mtu,
			PrimaryIntf:          primaryIntf,
		}
	}

	if primaryInterfacesFound > 1 {
		return nil, errors.Errorf("multiple primary interfaces found")
	}

	if primaryInterfacesFound == 0 {
		totalInterfaces := len(intfName2Info)
		if totalInterfaces == 1 {
			// Make it the default/primary if there is only one interface
			for _, intf := range intfName2Info {
				intf.PrimaryIntf = true
			}
		} else {
			// Unless all interfaces have DHCP enabled, we MUST have one and only one
			// interface marked as primary interface
			if interfacesWithDHCPEnabled != totalInterfaces {
				return nil, errors.Errorf("no primary interfaces found and not all are on DHCP")
			}
		}
	}

	return &intfName2Info, nil
}

func ApplyNetworkConfig(
	interfaces []kubevirtv1.Interface,
	networkConfigSecret *corev1.Secret) error {
	cloudInitNetworkConfig, err := getCloudInitNetworkData(networkConfigSecret)
	if err != nil {
		return err
	}

	for _, cfg := range cloudInitNetworkConfig.Network.Config {
		for index, inf := range interfaces {
			if inf.Name == cfg.Name {
				interfaces[index].MacAddress = cfg.MacAddress
			}
		}
	}

	return nil
}

// String returns the YAML representation of the cloud-init network configuration.
func (cfg *CloudInitNetworkConfigV1) String() string {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return ""
	}
	return string(data)
}

// createCloudInitNetworkDataV1Yaml generates a cloud-init network configuration version 1.
func createCloudInitNetworkDataV1Yaml(interfaces []KubeVirtInterfaceInfo) string {
	var cloudInitNetworkConfigV1 CloudInitNetworkConfigV1
	cloudInitNetworkConfigV1.Network.Version = 1

	for _, intf := range interfaces {

		// Add a config for each interface
		var physicalInterfaceConfig Config
		physicalInterfaceConfig.Type = "physical"
		physicalInterfaceConfig.Name = intf.setName
		physicalInterfaceConfig.MacAddress = intf.mac.String()
		physicalInterfaceConfig.MTU = intf.mtu

		// this might seem awkward, using a pointer to a bool.
		// but its needed to force the json marshaller to produce a string "false"
		// in the accept-ra value.  otherwise a regular bool false will just omit
		// the field altogether.  which we dont want because if unset
		// it defaults to true in netplan/networkd
		acceptRA := new(bool)
		*acceptRA = false
		physicalInterfaceConfig.AcceptRA = acceptRA

		// Add a subnet for IPv4
		if !intf.ip4Info.ipv4Addr.IP.Equal(net.IP{}) {
			var v4subnet Subnet
			v4subnet.Type = "static"
			maskLen, _ := intf.ip4Info.ipv4Mask.Size()
			v4subnet.Address = fmt.Sprintf("%s/%d", intf.ip4Info.ipv4Addr.String(), maskLen)
			// Add an IPv4 default route for the primary interface
			if intf.primaryIntf {
				v4subnet.Routes = []Route{
					{
						Gateway: intf.ip4Info.ipv4Gw.String(),
						Network: "0.0.0.0",
						Netmask: "0",
						Metric:  1024,
					},
				}
				// Add DNS nameservers if they exist.
				// Per the comments above, we only add DNS servers to the primary interface.
				if len(intf.nameservers) > 0 {
					for _, nameserver := range intf.nameservers {
						v4subnet.DNSNameservers = append(v4subnet.DNSNameservers, nameserver.String())
					}
					v4subnet.DNSSearch = []string{"."}
				}
			}
			physicalInterfaceConfig.Subnets = append(physicalInterfaceConfig.Subnets, v4subnet)
		} else if intf.ip4Info.dhcpv4 {
			physicalInterfaceConfig.Subnets = append(physicalInterfaceConfig.Subnets, Subnet{Type: "dhcp4"})
		}

		// Add a subnet for IPv6
		if !intf.ip6Info.ipv6Addr.IP.Equal(net.IP{}) {
			var v6subnet Subnet
			v6subnet.Type = "static6"
			maskLen, _ := intf.ip6Info.ipv6Mask.Size()
			v6subnet.Address = fmt.Sprintf("%s/%d", intf.ip6Info.ipv6Addr.String(), maskLen)
			// Add an IPv6 default route for the primary interface
			if intf.primaryIntf {
				v6subnet.Routes = []Route{
					{
						Gateway: intf.ip6Info.ipv6Gw.String(),
						Network: "::",
						Netmask: "0",
						Metric:  8192,
					},
				}
			}
			physicalInterfaceConfig.Subnets = append(physicalInterfaceConfig.Subnets, v6subnet)
		}
		cloudInitNetworkConfigV1.Network.Config = append(cloudInitNetworkConfigV1.Network.Config, physicalInterfaceConfig)
	}

	return cloudInitNetworkConfigV1.String()
}

func CreateIPClaimName(vmName string, intfName string, ipProtocolType IPProtocolType) string {

	protocol := "v4"
	if ipProtocolType == IP6 {
		protocol = "v6"
	}
	ipClaimName := fmt.Sprintf("%s-%s-%s", vmName, intfName, protocol)
	// sanitize ipClaimName: a lowercase RFC 1123 subdomain must consist of
	// lower case alphanumeric characters, '-' or '.', and must start and end
	// with an alphanumeric character (e.g. 'example.com', regex used for
	// validation is
	// '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')
	ipClaimName = strings.ReplaceAll(ipClaimName, "_", "-")
	return ipClaimName
}

// dhcpv4=false,ippool="val",ip6pool="v6pool"
// dhcpv4=true,ip6pool="v6pool",mtu=1320,defaultintf=true
func parseIntfTag(tagStr string) (useDhcp bool, associatedIPPool string, associatedIPv6Pool string, mtuVal int, primaryIntf bool) {
	validTags := map[string]bool{"dhcpv4": true, "ippool": true, "ip6pool": true, "mtu": true, "defaultintf": true}
	if len(tagStr) == 0 {
		// if there is NO tag, assume that caller expects dhcp
		return true, "", "", 0, false
	}
	primaryIntf = false
	// Split the CSV list by commas
	tagTokens := strings.Split(tagStr, ",")
	for _, tagToken := range tagTokens {
		tags := strings.Split(tagToken, "=")
		if len(tags) != 2 {
			// All tags must be of the form key=value
			return false, "", "", mtuVal, false
		}
		k := strings.TrimSpace(tags[0])
		if _, ok := validTags[k]; !ok {
			return false, "", "", mtuVal, false
		}
		if k == "ippool" {
			associatedIPPool = strings.TrimSpace(tags[1])
			if associatedIPPool == "" || len(associatedIPPool) == 0 {
				useDhcp = true
			}
			continue
		}
		if k == "ip6pool" {
			associatedIPv6Pool = strings.TrimSpace(tags[1])
			continue
		}
		if k == "mtu" {
			mtu, err := strconv.Atoi(strings.TrimSpace(tags[1]))
			if err != nil {
				mtuVal = 0
			} else {
				mtuVal = mtu
			}
			continue
		}
		if k == "defaultintf" {
			pri := strings.TrimSpace(tags[1])
			if strings.Compare(strings.ToLower(pri), "true") == 0 {
				primaryIntf = true
			}
			continue
		}

		// Must be dhcpv4
		dhcpState := strings.TrimSpace(tags[1])
		if strings.Compare(strings.ToLower(dhcpState), "true") == 0 {
			useDhcp = true
		}
	}
	return useDhcp, associatedIPPool, associatedIPv6Pool, mtuVal, primaryIntf
}

const defaultNamespace = "default"

func SplitBySlash(val string) (namespaceName string, poolName string) {
	if val == "" {
		namespaceName = defaultNamespace
		poolName = ""
		return
	}
	tags := strings.Split(val, "/")
	if len(tags) == 1 {
		// All tags must be of the form namespace/poolname
		// Now, if there is no "/", assume that val is poolName
		// and namespaceName is "default"
		namespaceName = defaultNamespace
		poolName = tags[0]
		return
	}

	namespaceName = tags[0]
	poolName = tags[1]
	return
}

// Caller will provide a tag/annotation in the VM template to map interfaces to ip-pools
// The interfaces will have a name string as well, such as "Workload1"
// We will generate the MAC addresses randomly as they are per VM not per VM template

func GenerateRandomUnicastMac(usePrefix bool, prefix []byte) (net.HardwareAddr, error) {
	buf := make([]byte, 6)
	var mac net.HardwareAddr
	_, err := rand.Read(buf)
	if err != nil {
		return mac, err
	}
	// Mark unicast
	buf[0] |= 2
	if usePrefix && len(prefix) >= 3 {
		buf[0] = prefix[0]
		buf[1] = prefix[1]
		buf[2] = prefix[2]
	}
	mac = append(mac, buf[0], buf[1], buf[2], buf[3], buf[4], buf[5])
	return mac, nil
}

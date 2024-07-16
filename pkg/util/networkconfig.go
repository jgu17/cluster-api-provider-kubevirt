package util

import (
	"fmt"
	"net"

	"github.com/pkg/errors"
	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/context"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	ipamv1 "github.com/metal3-io/ip-address-manager/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apitypes "k8s.io/apimachinery/pkg/types"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

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

func GetCloudInitConfigDriveNetworkData(
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
	return makeCloudInitNetworkConfigV1(interfaces), nil
}

func buildKernelArgs(ctx *context.MachineContext) (string, error) {
	if ctx.KubevirtMachine.Spec.VirtualMachineTemplate.KernelArgs == nil {
		return "", nil
	}

	kernelArgs := *ctx.KubevirtMachine.Spec.VirtualMachineTemplate.KernelArgs

	// Prevent the kernel cmd line for bailing later. x64 only allows 2048 bytes worth of
	// total length, spaces and all. Prevent unknown issues later
	if len(kernelArgs) >= 2048 {
		strErr := "generating kernel args grub config failed because size >= 2048"
		err := errors.Errorf(strErr)
		ctx.Info(strErr)
		return "", err
	}

	return kernelArgs, nil
}

func buildKernelArgsGrubConfig(kernelArgs string) string {
	grubConfig := "# GRUB Environment Block\n#\nafo_cmdline="
	grubConfig += kernelArgs
	grubConfig += "\n"
	return grubConfig
}

func CreateOrUpdateKernelArgsSecretNormal(
	ctx *context.MachineContext,
	infraClusterClient client.Client,
	targetNamespace string) (*corev1.Secret, error) {
	kernelArgsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      *ctx.Machine.Spec.Bootstrap.DataSecretName + "-kernel-args",
			Namespace: targetNamespace,
		},
	}

	mutateFn := func() (err error) {
		if kernelArgsSecret.ObjectMeta.Labels != nil && kernelArgsSecret.ObjectMeta.Labels[clusterv1.ClusterNameLabel] == ctx.KubevirtCluster.GetName() {
			return nil
		}

		kernelArgs, err := buildKernelArgs(ctx)
		if err != nil {
			wrappedErr := errors.Wrap(err, "failed to build kubevirt machine kernel args")
			return wrappedErr
		}

		if kernelArgs != "" {
			kernelArgs = buildKernelArgsGrubConfig(kernelArgs)
		}

		kernelArgsSecret.Data = map[string][]byte{
			KernelArgsSecretKey: []byte(kernelArgs),
		}
		kernelArgsSecret.Type = clusterv1.ClusterSecretType
		if kernelArgsSecret.ObjectMeta.Labels == nil {
			kernelArgsSecret.ObjectMeta.Labels = map[string]string{}
		}
		kernelArgsSecret.ObjectMeta.Labels[clusterv1.ClusterNameLabel] = ctx.KubevirtCluster.GetName()

		return nil
	}

	result, err := controllerutil.CreateOrUpdate(ctx, infraClusterClient, kernelArgsSecret, mutateFn)
	if err != nil {
		return kernelArgsSecret, err
	}

	switch result {
	case controllerutil.OperationResultCreated:
		ctx.Info("Created kernel args secret")
	case controllerutil.OperationResultUpdated:
		ctx.Info("Updated kernel args secret")
	case controllerutil.OperationResultNone:
		fallthrough
	default:
	}

	return kernelArgsSecret, nil
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
		useDhcp, associatedIPPool, associatedIPv6Pool, mtu, primaryIntf := ParseIntfTag(intf.Tag)

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

// CloudInitNetworkConfigV1 is a cloud-init network configuration version 1.
type CloudInitNetworkConfigV1 struct {
	Network networkConfig `json:"network"`
}

type networkConfig struct {
	Version int      `json:"version"`
	Config  []config `json:"config"`
}

type config struct {
	Type       string   `json:"type"`
	Name       string   `json:"name"`
	MacAddress string   `json:"mac_address,omitempty"`
	MTU        int      `json:"mtu,omitempty"`
	Subnets    []subnet `json:"subnets,omitempty"`
	AcceptRA   *bool    `json:"accept-ra,omitempty"`
}

type subnet struct {
	Type           string   `json:"type"`
	Address        string   `json:"address,omitempty"`
	Routes         []route  `json:"routes,omitempty"`
	DNSNameservers []string `json:"dns_nameservers,omitempty"`
	DNSSearch      []string `json:"dns_search,omitempty"`
}

type route struct {
	Gateway string `json:"gateway"`
	Network string `json:"network"`
	Netmask string `json:"netmask"`
	Metric  int    `json:"metric,omitempty"`
}

// String returns the YAML representation of the cloud-init network configuration.
func (cfg *CloudInitNetworkConfigV1) String() string {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return ""
	}
	return string(data)
}

// makeCloudInitNetworkConfigV1 generates a cloud-init network configuration version 1.
func makeCloudInitNetworkConfigV1(interfaces []KubeVirtInterfaceInfo) string {

	var cloudInitNetworkConfigV1 CloudInitNetworkConfigV1
	cloudInitNetworkConfigV1.Network.Version = 1

	for _, intf := range interfaces {

		// Add a config for each interface
		var physicalInterfaceConfig config
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
			var v4subnet subnet
			v4subnet.Type = "static"
			maskLen, _ := intf.ip4Info.ipv4Mask.Size()
			v4subnet.Address = fmt.Sprintf("%s/%d", intf.ip4Info.ipv4Addr.String(), maskLen)
			// Add an IPv4 default route for the primary interface
			if intf.primaryIntf {
				v4subnet.Routes = []route{
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
			physicalInterfaceConfig.Subnets = append(physicalInterfaceConfig.Subnets, subnet{Type: "dhcp4"})
		}

		// Add a subnet for IPv6
		if !intf.ip6Info.ipv6Addr.IP.Equal(net.IP{}) {
			var v6subnet subnet
			v6subnet.Type = "static6"
			maskLen, _ := intf.ip6Info.ipv6Mask.Size()
			v6subnet.Address = fmt.Sprintf("%s/%d", intf.ip6Info.ipv6Addr.String(), maskLen)
			// Add an IPv6 default route for the primary interface
			if intf.primaryIntf {
				v6subnet.Routes = []route{
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

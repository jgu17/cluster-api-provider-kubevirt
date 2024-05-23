package util

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"net"
	"strings"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	ipamv1 "github.com/metal3-io/ip-address-manager/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	ctx context.Context,
	logger logr.Logger,
	fabricClient client.Client,
	machineName string,
	intf *KubeVirtInterfaceInfo,
	ipProtocolType IPProtocolType,
	namespace string,
	ipClaimList []*ipamv1.IPClaim) (bool, error) {
	ipClaimName := CreateIPClaimName(machineName, intf.setName, ipProtocolType)
	var ipClaim *ipamv1.IPClaim = nil
	for _, ipc := range ipClaimList {
		if ipc.Name == ipClaimName {
			ipClaim = ipc
			break
		}
	}
	if ipClaim == nil {
		logger.Info("ipClaim object is a null pointer", "ip-claim", ipClaimName)
		return true, errors.Errorf("ipClaim %s: ipClaim object is nil!", ipClaimName)
	}

	if ipClaim.Status.Address == nil {
		logger.Info("ipClaim Status and Address object is a null pointer", "ip-claim", ipClaimName)
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
		logger.Info("ipClaim object has no associated IP address", "ip-claim", ipClaimName)
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

// Code to generate Cloud-Init Network Config V2 YAML
// XXX TODO - Right now, this is hacky code - Ideally we should use
// something like https://pkg.go.dev/github.com/juju/juju/network/netplan#Ethernet
// subject to software licensing
func makeCloudInitNetworkConfigV2(interfaces []KubeVirtInterfaceInfo) string {
	intfTemplate := `
  id%d:
    match:
      macaddress: '%s'
    set-name: %s`

	mtuTemplate := `
    mtu: %d`

	addressTemplate := `
    addresses:`

	address := `
      - %s`

	gw4Template := `
    gateway4: %s`

	gw6Template := `
    gateway6: %s`

	dhcpTemplate := `
    dhcp4: %s`

	nameserverTemplate := `
    nameservers:
      search: [.]
      addresses: [%s]`

	netCfgV2Template := `
version: 2
ethernets:`

	yamlOutput := netCfgV2Template
	for i, intf := range interfaces {
		skipIP4 := false
		intfAssigned := fmt.Sprintf(intfTemplate, i, intf.mac.String(), intf.setName)
		if intf.mtu != 0 {
			intfAssigned += fmt.Sprintf(mtuTemplate, intf.mtu)
		}
		if intf.ip4Info.dhcpv4 {
			intfAssigned += fmt.Sprintf(dhcpTemplate, "true")
			skipIP4 = true
		}

		addressHeaderAdded := false
		gw4 := ""
		if !skipIP4 {
			intfAssigned += fmt.Sprintf(dhcpTemplate, "false")
			ip4 := intf.ip4Info.ipv4Addr.String()
			mask4, _ := intf.ip4Info.ipv4Mask.Size()
			if mask4 != 0 {
				if !addressHeaderAdded {
					intfAssigned += addressTemplate
					addressHeaderAdded = true
				}
				cidr4 := ip4 + fmt.Sprintf("/%d", mask4)
				addr4 := fmt.Sprintf(address, cidr4)
				intfAssigned += addr4
			}
			if intf.ip4Info.ipv4Gw.IP.String() != "0.0.0.0" {
				gw4 = fmt.Sprintf(gw4Template, intf.ip4Info.ipv4Gw.String())
			}
		}

		gw6 := ""
		ip6 := intf.ip6Info.ipv6Addr.String()
		if ip6 != "" {
			mask6, _ := intf.ip6Info.ipv6Mask.Size()
			if mask6 != 0 {
				if !addressHeaderAdded {
					intfAssigned += addressTemplate
					addressHeaderAdded = true
				}
				cidr6 := ip6 + fmt.Sprintf("/%d", mask6)
				addr6 := fmt.Sprintf(address, cidr6)
				intfAssigned += addr6
			}
			if intf.ip6Info.ipv6Gw.IP.String() != "::" {
				gw6 = fmt.Sprintf(gw6Template, intf.ip6Info.ipv6Gw.String())
			}
		}

		if gw4 != "" && intf.primaryIntf {
			intfAssigned += gw4
		}

		if gw6 != "" && intf.primaryIntf {
			intfAssigned += gw6
		}

		if len(intf.nameservers) > 0 {
			csvDNS := ""
			for _, dns := range intf.nameservers {
				csvDNS += dns.IP.String() + ", "
			}
			csvDNS = strings.TrimSuffix(csvDNS, ", ")
			intfAssigned += fmt.Sprintf(nameserverTemplate, csvDNS)
		}

		yamlOutput += intfAssigned
	}
	return yamlOutput
}

// use the interfaces argument to create gzipped + base64 network config for use in the VM spec
func getNetConfGzB64(interfaces []KubeVirtInterfaceInfo, doGz bool, version int) (string, error) {
	var netConfig string
	switch version {
	case 1:
		netConfig = makeCloudInitNetworkConfigV1(interfaces)
	case 2:
		netConfig = makeCloudInitNetworkConfigV2(interfaces)
	default:
		return "", fmt.Errorf("unsupported cloud-init network config version %d", version)
	}

	if !doGz {
		return EncodeAsBase64(*bytes.NewBuffer([]byte(netConfig))), nil
	}
	netConfigGz, err := GzipString(netConfig)
	if err != nil {
		return "", err
	}
	return EncodeAsBase64(netConfigGz), nil
}

// use the intfName2Info table to create fully formed list of interfaces
func getInterfacesInfo(
	ctx context.Context,
	logger logr.Logger,
	fabricClient client.Client,
	machineName string,
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
				logger,
				fabricClient,
				machineName,
				&intf,
				IP4,
				intfCfg.IPPoolNamespaceName,
				ipClaimList)
			if shouldRetry {
				logger.Info("Retry from updateInterfaceInfoFromIPAddressFromClaim")
				return nil, true, err
			}
			if err != nil {
				return nil, false, err
			}
		}
		if intfCfg.AssociatedIP6Pool != "" {
			shouldRetry, err := updateInterfaceInfoFromIPAddressFromClaim(
				ctx,
				logger,
				fabricClient,
				machineName,
				&intf,
				IP6,
				intfCfg.IP6PoolNamespaceName,
				ipClaimList)
			if shouldRetry {
				logger.Info("Retry from updateInterfaceInfoFromIPAddressFromClaim v6")
				return nil, true, err
			}
			if err != nil {
				return nil, false, err
			}
		}
		logger.Info("Interface configuration", "machineName", machineName, "configuration", fmt.Sprintf("%+v", intf))
		intfs[i] = intf
	}

	return intfs, false, nil
}

func getNetworkConfig(
	networkConfigSecret *corev1.Secret) (*map[string]KubeVirtInterfaceConfig, error) {

	networkConfig := new(map[string]KubeVirtInterfaceConfig)
	decoder := gob.NewDecoder(bytes.NewReader(networkConfigSecret.Data["value"]))
	err := decoder.Decode(networkConfig)
	if err != nil {
		return nil, err
	}

	return networkConfig, nil
}

func getNetConfigKernelArg(
	ctx context.Context,
	fabricClient client.Client,
	machineName string,
	logger logr.Logger,
	networkConfigSecret *corev1.Secret,
	ipClaimList []*ipamv1.IPClaim) (string, error) {
	intfName2Info, err := getNetworkConfig(networkConfigSecret)
	if err != nil {
		return "", err
	}

	interfaces, shouldRetry, err := getInterfacesInfo(ctx, logger, fabricClient, machineName, intfName2Info, ipClaimList)
	if shouldRetry || err != nil {
		logger.Info("Failed to load network configuration")
		if err != nil {
			err = errors.Wrapf(err, "failed to load network configuration")
		} else {
			err = errors.New("failed to load network configuration")
		}
		return "", err
	}

	// Convert to netconfig and gzip and base64 encode
	netCfg, err := getNetConfGzB64(interfaces, true, 1)
	if err != nil {
		return "", err
	}

	kernelArg := "network-config=" + netCfg
	return kernelArg, nil
}

func buildKernelArgs(
	machineName string,
	logger logr.Logger,
	netConfigKernelArg string,
	templateKernelArgs *string,
) (string, error) {
	kernelArgs := netConfigKernelArg
	if templateKernelArgs != nil {
		kernelArgs += " "
		kernelArgs += *templateKernelArgs
	}

	logger.Info("Kernel arguments passed to Virtual Machine", "kernelArgs", kernelArgs, "virtualMachine", machineName)
	// Prevent the kernel cmd line for bailing later. x64 only allows 2048 bytes worth of
	// total length, spaces and all. Prevent unknown issues later
	if len(kernelArgs) >= 2048 {
		strErr := "generating kernel args grub config failed because size >= 2048"
		err := errors.Errorf(strErr)
		logger.Info(strErr)
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
	ctx context.Context,
	infraClusterClient client.Client,
	machineName, targetNamespace, clusterName string,
	logger logr.Logger,
	templateKernelArgs *string,
	networkConfigSecret *corev1.Secret,
	ipClaimList []*ipamv1.IPClaim) (*corev1.Secret, error) {
	kernelArgsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      CreateKernelArgsSecretName(machineName),
			Namespace: targetNamespace,
		},
	}

	mutateFn := func() (err error) {
		if kernelArgsSecret.ObjectMeta.Labels != nil && kernelArgsSecret.ObjectMeta.Labels[clusterv1.ClusterNameLabel] == clusterName {
			return nil
		}

		// TOGO gujames POC if network data can use the cloudinit data source construct in kubevirt api
		netConfigKernelArg, err := getNetConfigKernelArg(ctx, infraClusterClient, machineName, logger, networkConfigSecret, ipClaimList)
		if err != nil {
			return errors.Wrap(err, "failed to create netconfig kernel argument")
		}

		kernelArgs, err := buildKernelArgs(
			machineName,
			logger,
			netConfigKernelArg,
			templateKernelArgs)
		if err != nil {
			wrappedErr := errors.Wrap(err, "failed to build kubevirt machine kernel args")
			return wrappedErr
		}

		kernelArgsGrubConfig := buildKernelArgsGrubConfig(kernelArgs)

		kernelArgsSecret.Data = map[string][]byte{
			KernelArgsSecretKey: []byte(kernelArgsGrubConfig),
		}
		if kernelArgsSecret.ObjectMeta.Labels == nil {
			kernelArgsSecret.ObjectMeta.Labels = map[string]string{}
		}
		kernelArgsSecret.ObjectMeta.Labels[clusterv1.ClusterNameLabel] = clusterName

		return nil
	}

	result, err := controllerutil.CreateOrUpdate(ctx, infraClusterClient, kernelArgsSecret, mutateFn)
	if err != nil {
		return kernelArgsSecret, err
	}

	switch result {
	case controllerutil.OperationResultCreated:
		logger.Info("Created kernel args secret")
	case controllerutil.OperationResultUpdated:
		logger.Info("Updated kernel args secret")
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
func assignRandomMac(
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

func getNetworkConfigSecret(
	ctx context.Context,
	fabricClient client.Client,
	machineName, targetNamespace string) (*corev1.Secret, error) {
	secret := new(corev1.Secret)
	secretName := apitypes.NamespacedName{
		Namespace: targetNamespace,
		Name:      CreateNetworkConfigSecretName(machineName),
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

func CreateOrUpdateNetworkConfigSecret(
	ctx context.Context,
	infraCluserClient client.Client,
	machineName, infraClusterNamespace, clusterName string,
	logger logr.Logger,
	interfaces []kubevirtv1.Interface,
) (*corev1.Secret, error) {
	secret, err := getNetworkConfigSecret(ctx, infraCluserClient, machineName, infraClusterNamespace)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
	} else {
		return secret, nil
	}

	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      CreateNetworkConfigSecretName(machineName),
			Namespace: infraClusterNamespace,
		},
	}

	var intfName2Info *map[string]KubeVirtInterfaceConfig = nil

	mutateFn := func() (err error) {
		if secret.ObjectMeta.Labels != nil && secret.ObjectMeta.Labels[clusterv1.ClusterNameLabel] == clusterName {
			return nil
		}

		// Generate a Network Config v2 Yaml with all this information + rename the interface to desired name
		intfName2Info, err = MapIntfNameToIntfConfig(interfaces)
		if err != nil {
			logger.Info("generating mac address config failed, retry from MapIntfNameToIntfConfig")
			return err
		}

		// Generate Mac addresses for each interface in the VM template config.
		intfName2Info, err = assignRandomMac(intfName2Info)
		if err != nil {
			logger.Info("generating mac address config failed, retry from assignRandomMac")
			return err
		}

		buffer := bytes.Buffer{}
		encoder := gob.NewEncoder(&buffer)
		err = encoder.Encode(*intfName2Info)
		if err != nil {
			logger.Info("encoding mac address config failed")
			return err
		}
		secret.Data = map[string][]byte{
			"value": buffer.Bytes(),
		}

		if secret.ObjectMeta.Labels == nil {
			secret.ObjectMeta.Labels = map[string]string{}
		}
		secret.ObjectMeta.Labels[clusterv1.ClusterNameLabel] = clusterName

		return nil
	}

	result, err := controllerutil.CreateOrUpdate(ctx, infraCluserClient, secret, mutateFn)
	if err != nil {
		return nil, err
	}

	switch result {
	case controllerutil.OperationResultCreated:
		logger.Info("Created network config secret", "payload", intfName2Info)
	case controllerutil.OperationResultUpdated:
		logger.Info("Updated network config secret")
	case controllerutil.OperationResultNone:
		fallthrough
	default:
	}

	return secret, nil
}

func ApplyNetworkConfig(
	interfaces []kubevirtv1.Interface,
	networkConfigSecret *corev1.Secret) error {
	intfName2Info, err := getNetworkConfig(networkConfigSecret)
	if err != nil {
		return err
	}

	for _, cfg := range *intfName2Info {
		interfaces[cfg.ID].MacAddress = cfg.Intf.MacAddress
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

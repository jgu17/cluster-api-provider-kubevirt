package util

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/pkg/errors"
)

// Enum to indicate IP protocol types
type IPProtocolType int32

const (
	IP4 IPProtocolType = iota
	IP6
)

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

func GzipString(input string) (bytes.Buffer, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if zw == nil {
		return buf, errors.Errorf("could not open gzip writer stream")
	}
	_, err := zw.Write([]byte(input))
	if err != nil {
		// Don't care about return code in case of fail-close
		zw.Close()
		return buf, err
	}
	err = zw.Flush()
	if err != nil {
		// Don't care about return code in case of fail-close
		zw.Close()
		return buf, err
	}
	// This is important - you cannot defer the close
	// https://stackoverflow.com/questions/16890648/how-can-i-use-the-compress-gzip-package-to-gzip-a-file/67774730#67774730
	// If you defer, you get an unexpected EOF!
	err = zw.Close()
	return buf, err
}

func EncodeAsBase64(input bytes.Buffer) string {
	sEnc := base64.StdEncoding.EncodeToString(input.Bytes())
	return sEnc
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

// dhcpv4=false,ippool="val",ip6pool="v6pool"
// dhcpv4=true,ip6pool="v6pool",mtu=1320,defaultintf=true
func ParseIntfTag(tagStr string) (useDhcp bool, associatedIPPool string, associatedIPv6Pool string, mtuVal int, primaryIntf bool) {
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

func CreateKernelArgsSecretName(machine string) string {
	return fmt.Sprintf("%s-kernel-args", machine)
}

// TODO move into annotation on the kubevirtmachine object
func CreateNetworkConfigSecretName(machine string) string {
	return fmt.Sprintf("%s-network-config", machine)
}

func CreateClusterServiceAccountSecretName(serviceAccountName string) string {
	return serviceAccountName + "-token"
}

// TODO gujames rename the label and key
const KernelArgsVolumeLabel = "afo-cfg"
const KernelArgsSecretKey = "afo.cfg"
const KernelArgsVolumeName = "kernelargsvolume"
const TokenSecretVolumeName = "cluster-sa-token"

// To identify the mounted volume from the VM, inserting a specific serial
// number, referenced from cluster templates (generated using pseudo code:
// prefix(base32(TokenSecretVolumeName), 20))

// #nosec G101 -- merely an identifier
const TokenSecretDiskSerial = "6MNWHK43UMVZC243BFV2"

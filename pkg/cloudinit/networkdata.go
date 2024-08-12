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
	"net"
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

/*
Copyright © 2022 - 2026 SUSE LLC

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

package server

import (
	"fmt"
	"net"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
)

// registrationIPAnnotation is the source IP elemental-register reports it
// connected from (its registration-ip annotation, merged with the prefix).
const registrationIPAnnotation = "elemental.cattle.io/registration-ip"

// setRegistrationIPLabel mirrors the registration-ip annotation into the
// label of the same name, so an inventory can be found by the IP shown on the
// host console. Without a usable IP the label is removed: a stale one would
// point at the wrong inventory.
func setRegistrationIPLabel(inventory *elementalv1.MachineInventory) {
	value := registrationIPLabelValue(inventory.Annotations[registrationIPAnnotation])
	if value == "" {
		delete(inventory.Labels, elementalv1.RegistrationIPLabel)
		return
	}
	if inventory.Labels == nil {
		inventory.Labels = map[string]string{}
	}
	inventory.Labels[elementalv1.RegistrationIPLabel] = value
}

// registrationIPLabelValue turns an IP as elemental-register reports it (an
// IPv6 address possibly in brackets and with a zone) into a label value:
// IPv4 in dotted form, IPv6 written in full with '-' for ':', e.g.
// fd00-0000-0000-0000-0000-0000-0000-0021. Anything else gives "".
func registrationIPLabelValue(reported string) string {
	address := strings.TrimSpace(reported)
	address = strings.TrimSuffix(strings.TrimPrefix(address, "["), "]")
	if zone := strings.IndexByte(address, '%'); zone >= 0 {
		address = address[:zone]
	}
	ip := net.ParseIP(address)
	if ip == nil {
		return ""
	}
	var value string
	if v4 := ip.To4(); v4 != nil {
		value = v4.String()
	} else {
		ip = ip.To16()
		groups := make([]string, 0, net.IPv6len/2)
		for index := 0; index < net.IPv6len; index += 2 {
			groups = append(groups, fmt.Sprintf("%02x%02x", ip[index], ip[index+1]))
		}
		value = strings.Join(groups, "-")
	}
	if len(validation.IsValidLabelValue(value)) > 0 {
		return ""
	}
	return value
}

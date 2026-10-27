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

// Package identity normalizes the SMBIOS identifiers that tie a host to its
// MachineInventory: the system UUID, the system serial number and the
// baseboard serial number. elemental-register derives new emulated TPM seeds
// from them and the registration server fingerprints them, so both sides
// share one definition of what a real value is.
//
// The rules are versioned through their consumers: seed derivation v2 and
// fingerprint v1 both use the rules below. Changing what counts as a
// placeholder changes derived seeds and stored fingerprints, so a change here
// needs a new seed version and a new fingerprint version, never an edit of
// the current ones.
package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// FingerprintVersion prefixes every fingerprint this package computes.
const FingerprintVersion = "v1"

// Fields are the normalized SMBIOS identifiers. An empty field was missing,
// unreadable or a known placeholder.
type Fields struct {
	// SystemUUID is the SMBIOS type 1 UUID in lower-case 8-4-4-4-12 form.
	SystemUUID string
	// SystemSerial is the SMBIOS type 1 serial number.
	SystemSerial string
	// BoardSerial is the SMBIOS type 2 serial number.
	BoardSerial string
}

// NewFields normalizes raw identifiers as read from dmidecode or sysfs.
func NewFields(systemUUID, systemSerial, boardSerial string) Fields {
	return Fields{
		SystemUUID:   NormalizeUUID(systemUUID),
		SystemSerial: NormalizeSerial(systemSerial),
		BoardSerial:  NormalizeSerial(boardSerial),
	}
}

// Valid reports whether at least one identifier carries a real value.
func (f Fields) Valid() bool {
	return f.SystemUUID != "" || f.SystemSerial != "" || f.BoardSerial != ""
}

// Fingerprint identifies hardware without storing its identifiers.
type Fingerprint struct {
	// Value is "v1:<hex sha256>".
	Value string
	// Strong is true when at least one identifier was valid. A weak
	// fingerprint is the same constant for every host whose identifiers are
	// all placeholders, so it can confirm nothing.
	Strong bool
}

// Strength returns the value of the fingerprint strength annotation.
func (fp Fingerprint) Strength() string {
	if fp.Strong {
		return "strong"
	}
	return "weak"
}

// Fingerprint hashes the normalized identifiers. Missing fields hash as empty
// strings, so a host keeps its fingerprint as long as its identifiers do.
func (f Fields) Fingerprint() Fingerprint {
	material := strings.Join([]string{
		"elemental hardware fingerprint " + FingerprintVersion,
		"system-uuid=" + f.SystemUUID,
		"system-serial=" + f.SystemSerial,
		"baseboard-serial=" + f.BoardSerial,
	}, "\n")
	sum := sha256.Sum256([]byte(material))
	return Fingerprint{
		Value:  FingerprintVersion + ":" + hex.EncodeToString(sum[:]),
		Strong: f.Valid(),
	}
}

// SMBIOS section and key names as decoded by pkg/dmidecode from dmidecode
// output, which is what elemental-register sends in MsgSmbios.
const (
	smbiosSystemSection = "System Information"
	smbiosBoardSection  = "Base Board Information"
	smbiosUUIDKey       = "UUID"
	smbiosSerialKey     = "Serial Number"
)

// FieldsFromSMBIOS extracts the identifiers from a decoded MsgSmbios payload.
func FieldsFromSMBIOS(data map[string]interface{}) Fields {
	return NewFields(
		smbiosValue(data, smbiosSystemSection, smbiosUUIDKey),
		smbiosValue(data, smbiosSystemSection, smbiosSerialKey),
		smbiosValue(data, smbiosBoardSection, smbiosSerialKey),
	)
}

// RawUUIDFromSMBIOS returns the system UUID exactly as the host reported it,
// placeholders included. MachineRegistration templates render
// ${System Information/UUID} from this value.
func RawUUIDFromSMBIOS(data map[string]interface{}) string {
	return smbiosValue(data, smbiosSystemSection, smbiosUUIDKey)
}

func smbiosValue(data map[string]interface{}, section, key string) string {
	values, ok := data[section].(map[string]interface{})
	if !ok {
		return ""
	}
	value, ok := values[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

// placeholderUUIDs are firmware defaults shipped on many boards. The first
// is the common AMI default as dmidecode and sysfs print it on SMBIOS 2.6+,
// the second the same bytes without the little-endian swap of the first
// three fields.
var placeholderUUIDs = map[string]bool{
	"03000200-0400-0500-0006-000700080009": true,
	"00020003-0004-0005-0006-000700080009": true,
}

// CanonicalUUID returns raw in lower-case 8-4-4-4-12 form when it parses as
// a UUID, and the trimmed lower-case input otherwise. Unlike NormalizeUUID it
// keeps placeholders, which makes it suitable for comparing two renderings
// of the same reported value.
func CanonicalUUID(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	compact := strings.ReplaceAll(strings.Trim(s, "{}"), "-", "")
	if len(compact) != 32 || !isHex(compact) {
		return s
	}
	return compact[0:8] + "-" + compact[8:12] + "-" + compact[12:16] + "-" + compact[16:20] + "-" + compact[20:32]
}

// NormalizeUUID returns the canonical UUID, or "" when raw is not a UUID
// (dmidecode prints "Not Settable" for all zeros and "Not Present" for all
// ones) or is a known placeholder.
func NormalizeUUID(raw string) string {
	canonical := CanonicalUUID(raw)
	compact := strings.ReplaceAll(canonical, "-", "")
	if len(compact) != 32 || !isHex(compact) {
		return ""
	}
	if repeatsOneChar(compact) || placeholderUUIDs[canonical] {
		return ""
	}
	return canonical
}

// placeholderSerials are serial number values, reduced to lower-case
// letters and digits, that firmware ships instead of a real serial.
var placeholderSerials = map[string]bool{
	"tobefilledbyoem":        true,
	"tobefilledbyoemname":    true,
	"defaultstring":          true,
	"default":                true,
	"none":                   true,
	"na":                     true,
	"null":                   true,
	"unknown":                true,
	"unspecified":            true,
	"empty":                  true,
	"invalid":                true,
	"tbd":                    true,
	"oem":                    true,
	"sn":                     true,
	"serial":                 true,
	"serialnum":              true,
	"serialnumber":           true,
	"systemserialnumber":     true,
	"baseboardserialnumber":  true,
	"boardserialnumber":      true,
	"chassisserialnumber":    true,
	"type2boardserialnumber": true,
	"0123456789":             true,
	"123456789":              true,
	"1234567890":             true,
	"12345678":               true,
	"1234567":                true,
	"123456":                 true,
}

// NormalizeSerial returns the serial number with surrounding and repeated
// white space removed and letters upper-cased, or "" when raw is empty or a
// placeholder. Like plainauth, any value starting with "Not " ("Not
// Specified", "Not Available", ...) counts as missing.
func NormalizeSerial(raw string) string {
	s := strings.Join(strings.Fields(raw), " ")
	if s == "" || strings.HasPrefix(strings.ToLower(s), "not ") {
		return ""
	}
	key := alphanumericKey(s)
	if key == "" || repeatsOneChar(key) || placeholderSerials[key] {
		return ""
	}
	return strings.ToUpper(s)
}

// NormalizeMAC lower-cases a MAC address. It does not judge it: the MAC is
// picked by the same rules plainauth uses for auth "mac".
func NormalizeMAC(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

var uuidPattern = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// generatedInventoryName matches the names the registration server makes up
// when a MachineRegistration has no usable name template: "m-" followed by a
// random UUID, which says nothing about the hardware.
var generatedInventoryName = regexp.MustCompile(`^m-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// UUIDsInName returns the UUIDs embedded in a MachineInventory name, as
// rendered from templates such as "node-${System Information/UUID}". Names
// generated by the server carry a random UUID and yield none.
func UUIDsInName(name string) []string {
	name = strings.ToLower(name)
	if generatedInventoryName.MatchString(name) {
		return nil
	}
	return uuidPattern.FindAllString(name, -1)
}

// Describe renders the identifiers for logs and events, marking missing ones.
func (f Fields) Describe() string {
	show := func(v string) string {
		if v == "" {
			return "<none>"
		}
		return v
	}
	return fmt.Sprintf("system-uuid=%s system-serial=%s baseboard-serial=%s", show(f.SystemUUID), show(f.SystemSerial), show(f.BoardSerial))
}

func alphanumericKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func repeatsOneChar(s string) bool {
	if s == "" {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

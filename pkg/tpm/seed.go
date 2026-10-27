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

package tpm

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/jaypipes/ghw"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/identity"
	"github.com/rancher/elemental-operator/pkg/log"
	"github.com/rancher/elemental-operator/pkg/plainauth"
)

// SeedVersionV2 is recorded in the registration state for seeds derived by
// DeriveSeedV2. States written before seed versions existed carry none.
const SeedVersionV2 = 2

// Seed is the emulated TPM seed chosen for one registration.
type Seed struct {
	Value int64
	// Version is SeedVersionV2 for a seed derived in this run, 0 otherwise.
	Version int
	// Salt is the salt the seed was derived with (Version 2 only).
	Salt uint32
	// Static is true for a seed taken verbatim from emulated-tpm-seed.
	Static bool
	// Previous is true for a seed kept from the registration state.
	Previous bool
}

// GetTPMSeed picks the emulated TPM seed.
//
// A seed recorded in the registration state (usePreviousSeed) always wins:
// an installed host keeps its identity whatever emulated-tpm-seed says now,
// including the -1 default that replaced 1. Without one, a static seed from
// the config is used as is, and -1 derives a new seed from the hardware with
// DeriveSeedV2 and the given salt. 0 would make the simulator pick a random
// seed on every start, an identity that cannot survive a reboot, so it is
// treated like -1.
func GetTPMSeed(reg elementalv1.Registration, usePreviousSeed bool, previousSeed int64, salt uint32) (Seed, error) {
	if usePreviousSeed {
		return Seed{Value: previousSeed, Previous: true}, nil
	}
	switch reg.EmulatedTPMSeed {
	case elementalv1.TPMRandomSeedValue:
	case 0:
		log.Warning("emulated-tpm-seed 0 would give the host a new identity at every start; deriving the seed from the hardware instead")
	default:
		log.Warningf("Using the static emulated-tpm-seed %d: every host registering with it shares one identity; prefer -1 (derive from the hardware)", reg.EmulatedTPMSeed)
		return Seed{Value: reg.EmulatedTPMSeed, Static: true}, nil
	}
	value, err := DeriveSeedV2(salt)
	if err != nil {
		return Seed{}, err
	}
	return Seed{Value: value, Version: SeedVersionV2, Salt: salt}, nil
}

// SeedV1FromUUID is the derivation elemental-register used for
// emulated-tpm-seed -1 before v2: the low 64 bits of the SMBIOS UUID. It
// ignores the first half of the UUID and cannot handle placeholder UUIDs.
// It is no longer used for new identities; states keep the seeds it
// produced, and it stays to reproduce those identities.
func SeedV1FromUUID(uuid string) (int64, bool) {
	var i big.Int
	if _, ok := i.SetString(strings.Replace(uuid, "-", "", -1), 16); !ok {
		return 0, false
	}
	return i.Int64(), true
}

// SeedMaterial is what a v2 seed is derived from.
type SeedMaterial struct {
	// Fields are the normalized SMBIOS identifiers.
	Fields identity.Fields
	// MAC is the lowest physical NIC MAC. It is only read, and only part of
	// the material, when none of Fields is valid.
	MAC string
}

// Valid reports whether the material can tell this host from others.
func (m SeedMaterial) Valid() bool {
	return m.Fields.Valid() || m.MAC != ""
}

// Describe names the identifiers the seed came from, for logs.
func (m SeedMaterial) Describe() string {
	if m.Fields.Valid() {
		return m.Fields.Describe()
	}
	return "mac=" + m.MAC
}

// ErrNoHardwareIdentity is returned when a host offers nothing to derive a
// v2 seed from.
var ErrNoHardwareIdentity = errors.New("no hardware identifier to derive the emulated TPM seed from")

// SeedV2 hashes the material and the salt and returns the first 8 bytes of
// the SHA-256 as an int64. The values 0 (random simulator seed) and -1
// ("derive") are never returned. The simulator only uses the seed modulo
// 2^31-1; the registration server catches the remaining collisions and R4
// moves to the next salt.
func SeedV2(m SeedMaterial, salt uint32) (int64, error) {
	if !m.Valid() {
		return 0, ErrNoHardwareIdentity
	}
	material := strings.Join([]string{
		"elemental-register emulated TPM seed v" + strconv.Itoa(SeedVersionV2),
		"system-uuid=" + m.Fields.SystemUUID,
		"system-serial=" + m.Fields.SystemSerial,
		"baseboard-serial=" + m.Fields.BoardSerial,
		"mac=" + m.MAC,
		"salt=" + strconv.FormatUint(uint64(salt), 10),
	}, "\x00")
	sum := sha256.Sum256([]byte(material))
	for {
		seed := int64(binary.BigEndian.Uint64(sum[:8]))
		if seed != 0 && seed != elementalv1.TPMRandomSeedValue {
			return seed, nil
		}
		// Practically unreachable (2^-63); stay deterministic anyway.
		sum = sha256.Sum256(append(sum[:], "next"...))
	}
}

// Host identifier readers, replaced in tests.
var (
	readHardwareIDs = readDMIHardwareIDs
	readHostMAC     = plainauth.HostMACAddress
)

// readDMIHardwareIDs reads the identifiers from /sys/class/dmi/id, the same
// source the v1 derivation used. ghw reports unreadable entries as
// "unknown", which normalization treats as missing.
func readDMIHardwareIDs() (uuid, systemSerial, boardSerial string) {
	if product, err := ghw.Product(ghw.WithDisableWarnings()); err == nil {
		uuid, systemSerial = product.UUID, product.SerialNumber
	} else {
		log.Warningf("cannot read SMBIOS product data: %v", err)
	}
	if board, err := ghw.Baseboard(ghw.WithDisableWarnings()); err == nil {
		boardSerial = board.SerialNumber
	} else {
		log.Warningf("cannot read SMBIOS baseboard data: %v", err)
	}
	return uuid, systemSerial, boardSerial
}

// ReadSeedMaterial collects the v2 seed material from this host.
func ReadSeedMaterial() (SeedMaterial, error) {
	uuid, systemSerial, boardSerial := readHardwareIDs()
	m := SeedMaterial{Fields: identity.NewFields(uuid, systemSerial, boardSerial)}
	if m.Fields.Valid() {
		return m, nil
	}
	mac, err := readHostMAC()
	if err != nil {
		return m, fmt.Errorf("%w: SMBIOS system UUID %q, system serial %q and baseboard serial %q are missing or placeholders, and %v; "+
			"fix the identifiers in the firmware, set a unique static registration.emulated-tpm-seed, or use a hardware TPM",
			ErrNoHardwareIdentity, uuid, systemSerial, boardSerial, err)
	}
	m.MAC = identity.NormalizeMAC(mac)
	log.Warningf("SMBIOS identifiers are missing or placeholders (system UUID %q, system serial %q, baseboard serial %q); deriving the emulated TPM seed from MAC %s",
		uuid, systemSerial, boardSerial, m.MAC)
	return m, nil
}

// DeriveSeedV2 derives a new emulated TPM seed for this host. There is no
// random fallback: a random seed would change the identity at every
// reinstall.
func DeriveSeedV2(salt uint32) (int64, error) {
	m, err := ReadSeedMaterial()
	if err != nil {
		return 0, err
	}
	seed, err := SeedV2(m, salt)
	if err != nil {
		return 0, err
	}
	log.Infof("TPM emulation: derived seed v%d with salt %d from %s", SeedVersionV2, salt, m.Describe())
	return seed, nil
}

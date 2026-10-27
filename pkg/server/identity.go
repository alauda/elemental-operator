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
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/identity"
	"github.com/rancher/elemental-operator/pkg/log"
	"github.com/rancher/elemental-operator/pkg/register"
)

// Event reasons for hardware identity decisions, recorded on the
// MachineInventory concerned.
const (
	EventReasonIdentityConflict       = "IdentityConflict"
	EventReasonIdentityRebound        = "IdentityRebound"
	EventReasonIdentityRebindRequired = "IdentityRebindRequired"
)

// identityConflictMessage starts every identity-conflict annotation; the
// MachineInventory controller shows the annotation in its Ready condition.
const identityConflictMessage = "hardware fingerprint does not match the registered host"

// serverOwnedAnnotations are never taken from what a registering host sends
// or from MachineRegistration templates: they always come from the stored
// MachineInventory and the server's identity decisions.
var serverOwnedAnnotations = []string{
	elementalv1.HardwareFingerprintAnnotation,
	elementalv1.HardwareFingerprintStrengthAnnotation,
	elementalv1.IdentityConflictAnnotation,
	elementalv1.AllowIdentityRebindAnnotation,
}

func isServerOwnedAnnotation(key string) bool {
	return slices.Contains(serverOwnedAnnotations, key)
}

var (
	errIdentityConflict       = errors.New("identity conflict")
	errIdentityRebindRequired = errors.New("identity rebind required")
)

// registrationSession is what the server learns about the registering host
// before MsgGet, and what it decided about its identity.
type registrationSession struct {
	// stored is the MachineInventory as authentication found it, before
	// this session changed anything; nil for a new identity.
	stored *elementalv1.MachineInventory
	// smbios is the MsgSmbios payload; nil when the host sent none.
	smbios map[string]interface{}
	// annotationsData is the raw MsgAnnotations payload and
	// clientAnnotations its decoded, un-prefixed keys.
	annotationsData   []byte
	clientAnnotations map[string]string
	// observedNetwork is the MsgObservedNetworkConfig payload.
	observedNetwork *elementalv1.ObservedNetwork
	// update is true when the host sent MsgUpdate, i.e. it registered
	// before (an installed host).
	update bool
	// nameEvidence is true when the MachineRegistration renders the system
	// UUID into inventory names, so a UUID in a name says which hardware
	// created the inventory.
	nameEvidence bool

	// fingerprint is recorded on the inventory at commit.
	fingerprint *identity.Fingerprint
	// consumeRebind removes the one-shot allow-identity-rebind annotation.
	consumeRebind bool
	// clearConflict removes the identity-conflict annotation.
	clearConflict bool
	// committed run once the inventory is stored.
	committed []func(*elementalv1.MachineInventory)
}

func newRegistrationSession(inventory *elementalv1.MachineInventory, registration *elementalv1.MachineRegistration) *registrationSession {
	s := &registrationSession{nameEvidence: nameCarriesSystemUUID(registration)}
	if !isNewInventory(inventory) {
		s.stored = inventory.DeepCopy()
	}
	return s
}

// nameCarriesSystemUUID tells whether the registration renders the SMBIOS
// system UUID into inventory names. Only then is a UUID found in a name
// evidence about the hardware: ${Random/UUID} renders UUIDs as well.
func nameCarriesSystemUUID(registration *elementalv1.MachineRegistration) bool {
	if registration == nil {
		return false
	}
	name := registration.Spec.MachineName
	return strings.Contains(name, "${System Information/UUID}") && !strings.Contains(name, "${Random/")
}

func (s *registrationSession) setAnnotations(data []byte) {
	s.annotationsData = data
	s.clientAnnotations = map[string]string{}
	_ = json.Unmarshal(data, &s.clientAnnotations)
}

// selfHealing tells whether the registering client resolves an identity
// conflict on its own. That is the case for the first registration (no
// MsgUpdate) of an emulated TPM identity whose seed the client derived with
// v2 (tpm-seed-version=2): such clients answer ErrorCodeIdentityConflict by
// deriving the seed again with the next salt (see
// register.TPMSeedVersionAnnotation). Static seeds, hardware TPMs, plain
// auth, installed hosts and clients that predate the annotation all keep
// presenting the same identity.
func (s *registrationSession) selfHealing() bool {
	return !s.update &&
		s.clientAnnotations["auth"] == "emulated-tpm" &&
		s.clientAnnotations[register.TPMSeedVersionAnnotation] == "2"
}

func (s *registrationSession) describeClient() string {
	auth := s.clientAnnotations["auth"]
	if auth == "" {
		auth = "unknown"
	}
	if ip := s.clientAnnotations["registration-ip"]; ip != "" {
		return fmt.Sprintf("auth %s from %s", auth, ip)
	}
	return "auth " + auth
}

// applyIdentityAnnotations makes the server-owned annotations of the
// inventory about to be committed what the stored inventory had, plus the
// decisions of this session.
func (s *registrationSession) applyIdentityAnnotations(inventory *elementalv1.MachineInventory) {
	if inventory.Annotations == nil {
		inventory.Annotations = map[string]string{}
	}
	for _, key := range serverOwnedAnnotations {
		value, found := "", false
		if s.stored != nil {
			value, found = s.stored.Annotations[key]
		}
		if found {
			inventory.Annotations[key] = value
		} else {
			delete(inventory.Annotations, key)
		}
	}
	if s.fingerprint != nil {
		strength := elementalv1.HardwareFingerprintWeak
		if s.fingerprint.Strong {
			strength = elementalv1.HardwareFingerprintStrong
		}
		inventory.Annotations[elementalv1.HardwareFingerprintAnnotation] = s.fingerprint.Value
		inventory.Annotations[elementalv1.HardwareFingerprintStrengthAnnotation] = strength
	}
	if s.consumeRebind {
		delete(inventory.Annotations, elementalv1.AllowIdentityRebindAnnotation)
	}
	if s.clearConflict {
		delete(inventory.Annotations, elementalv1.IdentityConflictAnnotation)
	}
}

func (s *registrationSession) afterCommit(inventory *elementalv1.MachineInventory) {
	for _, f := range s.committed {
		f(inventory)
	}
}

// checkHardwareIdentity ties the inventory to the registering hardware
// before anything is committed. For an inventory found by its hash it
// compares the hardware fingerprint (R3); for a new identity it looks for
// the inventory the same hardware registered before (R5). It returns the
// inventory to commit, or an error once the client has been told why not.
func (i *InventoryServer) checkHardwareIdentity(conn *websocket.Conn, protoVersion register.MessageType, sess *registrationSession, inventory *elementalv1.MachineInventory) (*elementalv1.MachineInventory, error) {
	if sess.smbios == nil {
		if sess.stored != nil && sess.stored.Annotations[elementalv1.HardwareFingerprintAnnotation] != "" {
			// The inventory was fingerprinted, so the host used to send it.
			i.recordEvent(sess.stored, corev1.EventTypeWarning, EventReasonIdentityConflict,
				"registration without SMBIOS data (no-smbios): the hardware fingerprint cannot be checked")
		}
		log.Debugf("No SMBIOS data from the registering host: hardware fingerprint not checked")
		return inventory, nil
	}
	fields := identity.FieldsFromSMBIOS(sess.smbios)
	fingerprint := fields.Fingerprint()
	if isNewInventory(inventory) {
		return i.checkNewIdentity(conn, protoVersion, sess, inventory, fields, fingerprint)
	}
	return i.checkKnownIdentity(conn, protoVersion, sess, inventory, fields, fingerprint)
}

// checkKnownIdentity handles a registration whose hash matched an inventory.
func (i *InventoryServer) checkKnownIdentity(conn *websocket.Conn, protoVersion register.MessageType, sess *registrationSession, inventory *elementalv1.MachineInventory, fields identity.Fields, fingerprint identity.Fingerprint) (*elementalv1.MachineInventory, error) {
	stored := sess.stored
	recorded := stored.Annotations[elementalv1.HardwareFingerprintAnnotation]
	var mismatch string
	switch {
	case recorded == fingerprint.Value:
		sess.fingerprint = &fingerprint
		return inventory, nil
	case recorded != "" && !identity.IsCurrentFingerprint(recorded):
		// Written by another release (e.g. before a downgrade): this server
		// cannot recompute it, so it neither compares nor replaces it.
		log.Warningf("MachineInventory %s/%s records hardware fingerprint %q of a version this server does not know; not checking it", stored.Namespace, stored.Name, recorded)
		return inventory, nil
	case recorded != "":
		mismatch = fmt.Sprintf("the registering host has fingerprint %s, the inventory records %s", fingerprint.Value, recorded)
	default:
		// Inventories from before fingerprints: check what they recorded
		// about the hardware first, then backfill.
		mismatch = oldEvidenceContradiction(stored, identity.RawUUIDFromSMBIOS(sess.smbios), sess.nameEvidence)
		if mismatch == "" {
			log.Infof("Recording hardware fingerprint %s (%s) on MachineInventory %s/%s", fingerprint.Value, fingerprint.Strength(), stored.Namespace, stored.Name)
			sess.fingerprint = &fingerprint
			return inventory, nil
		}
	}
	// An operator can vouch for new hardware, e.g. after a mainboard swap. A
	// host that derived this identity for its first registration is not that
	// hardware: it collided and picks another identity instead.
	if !sess.selfHealing() && rebindAllowedFor(stored, fields, fingerprint) {
		sess.fingerprint = &fingerprint
		sess.consumeRebind = true
		sess.clearConflict = true
		message := fmt.Sprintf("accepted new hardware (%s) as allowed by %s: %s", fields.Describe(), elementalv1.AllowIdentityRebindAnnotation, mismatch)
		sess.committed = append(sess.committed, func(committed *elementalv1.MachineInventory) {
			i.recordEvent(committed, corev1.EventTypeNormal, EventReasonIdentityRebound, message)
		})
		log.Infof("MachineInventory %s/%s: %s", stored.Namespace, stored.Name, message)
		return inventory, nil
	}
	return nil, i.refuseIdentityConflict(conn, protoVersion, sess, fields, mismatch)
}

// refuseIdentityConflict leaves the stored inventory as it is, tells the
// client, and records the conflict. A client that cannot pick another
// identity also gets the inventory marked: whoever holds that identity can
// fetch its system-agent configuration, so a person has to look at it.
func (i *InventoryServer) refuseIdentityConflict(conn *websocket.Conn, protoVersion register.MessageType, sess *registrationSession, fields identity.Fields, mismatch string) error {
	stored := sess.stored
	detail := fmt.Sprintf("%s: %s (registering host: %s, %s)", identityConflictMessage, mismatch, fields.Describe(), sess.describeClient())
	clientMessage := fmt.Sprintf("identity conflict: MachineInventory %s/%s belongs to other hardware; %s", stored.Namespace, stored.Name, mismatch)
	if sess.selfHealing() {
		i.recordEvent(stored, corev1.EventTypeWarning, EventReasonIdentityConflict, detail+"; the host derived this identity and retries with another seed salt")
		clientMessage += "; derive another identity and register again"
	} else {
		i.recordEvent(stored, corev1.EventTypeWarning, EventReasonIdentityConflict, detail+"; marked with "+elementalv1.IdentityConflictAnnotation)
		if err := i.markIdentityConflict(stored, detail); err != nil {
			log.Errorf("Cannot mark MachineInventory %s/%s with %s: %v", stored.Namespace, stored.Name, elementalv1.IdentityConflictAnnotation, err)
		}
		clientMessage += fmt.Sprintf("; the inventory is marked %s until an operator resolves it", elementalv1.IdentityConflictAnnotation)
	}
	log.Warningf("Refusing registration for MachineInventory %s/%s: %s", stored.Namespace, stored.Name, detail)
	if err := writeError(conn, protoVersion, register.ErrorMessage{Message: clientMessage, Code: register.ErrorCodeIdentityConflict}); err != nil {
		log.Errorf("Error reporting back error to client: %v", err)
	}
	return fmt.Errorf("%w on MachineInventory %s/%s: %s", errIdentityConflict, stored.Namespace, stored.Name, mismatch)
}

// markIdentityConflict adds the identity-conflict annotation and nothing
// else to the stored inventory.
func (i *InventoryServer) markIdentityConflict(stored *elementalv1.MachineInventory, detail string) error {
	marked := stored.DeepCopy()
	if marked.Annotations == nil {
		marked.Annotations = map[string]string{}
	}
	marked.Annotations[elementalv1.IdentityConflictAnnotation] = detail + " [" + time.Now().UTC().Format(time.RFC3339) + "]"
	return i.Patch(i, marked, client.MergeFrom(stored))
}

// checkNewIdentity handles a registration whose hash matched no inventory:
// the same hardware may have registered before under another identity, e.g.
// a host registered with the v1 seed reinstalled from a new ISO, a host
// moving from emulation to its hardware TPM, or from sys-uuid to tpm.
func (i *InventoryServer) checkNewIdentity(conn *websocket.Conn, protoVersion register.MessageType, sess *registrationSession, inventory *elementalv1.MachineInventory, fields identity.Fields, fingerprint identity.Fingerprint) (*elementalv1.MachineInventory, error) {
	sess.fingerprint = &fingerprint
	if !fingerprint.Strong {
		// Every host with placeholder identifiers shares a weak fingerprint.
		return inventory, nil
	}
	candidate, err := i.findInventoryForHardware(fingerprint, fields, identity.RawUUIDFromSMBIOS(sess.smbios), sess.nameEvidence)
	if err != nil {
		if writeErr := writeError(conn, protoVersion, register.NewErrorMessage(err)); writeErr != nil {
			log.Errorf("Error reporting back error to client: %v", writeErr)
		}
		return nil, err
	}
	if candidate == nil {
		return inventory, nil
	}
	var refusal string
	allowed := rebindAllowedFor(candidate, fields, fingerprint)
	switch {
	case allowed:
		return i.rebindIdentity(sess, inventory, candidate, true), nil
	case hasIdentityConflict(candidate):
		refusal = fmt.Sprintf("it is marked %s; resolve the conflict, or annotate it with %s set to this host's system UUID (or true) to move its identity here",
			elementalv1.IdentityConflictAnnotation, elementalv1.AllowIdentityRebindAnnotation)
	case weakIdentity(candidate):
		return i.rebindIdentity(sess, inventory, candidate, false), nil
	default:
		refusal = fmt.Sprintf("it is bound to a hardware TPM; annotate it with %s set to this host's system UUID (or true) to move its identity",
			elementalv1.AllowIdentityRebindAnnotation)
	}
	message := fmt.Sprintf("the registering hardware (%s, %s) matches MachineInventory %s/%s, but %s", fields.Describe(), sess.describeClient(), candidate.Namespace, candidate.Name, refusal)
	i.recordEvent(candidate, corev1.EventTypeWarning, EventReasonIdentityRebindRequired, message)
	log.Warningf("Refusing registration: %s", message)
	if err := writeError(conn, protoVersion, register.ErrorMessage{Message: message, Code: register.ErrorCodeIdentityRebindRequired}); err != nil {
		log.Errorf("Error reporting back error to client: %v", err)
	}
	return nil, fmt.Errorf("%w: %s", errIdentityRebindRequired, message)
}

// rebindIdentity moves the new identity onto the inventory the hardware
// registered before. Name, labels (pool membership) and annotations stay;
// only the hash changes, plus what this registration reports anyway.
func (i *InventoryServer) rebindIdentity(sess *registrationSession, inventory, candidate *elementalv1.MachineInventory, allowed bool) *elementalv1.MachineInventory {
	target := candidate.DeepCopy()
	previous := describeHashes(candidate)
	target.Spec.TPMHash = inventory.Spec.TPMHash
	target.Spec.MachineHash = inventory.Spec.MachineHash
	if sess.observedNetwork != nil {
		target.Spec.ObservedNetwork = sess.observedNetwork
	}
	// What the previous identity reported about itself (auth, tpm-*) must
	// not outlive it: a stale auth=emulated-tpm would let the next rebind
	// treat a hardware TPM as a weak identity.
	for key := range target.Annotations {
		if key == elementalv1.MachineInventoryAuthAnnotation || strings.HasPrefix(key, "elemental.cattle.io/tpm-") {
			delete(target.Annotations, key)
		}
	}
	if sess.annotationsData != nil {
		if err := mergeInventoryAnnotations(sess.annotationsData, target); err != nil {
			log.Warningf("Cannot merge client annotations into MachineInventory %s/%s: %v", target.Namespace, target.Name, err)
		}
	}
	sess.stored = candidate.DeepCopy()
	// An operator's allow-identity-rebind also settles a conflict on it.
	sess.consumeRebind = allowed
	sess.clearConflict = allowed
	message := fmt.Sprintf("identity moved from %s (auth %s) to %s (%s): same hardware fingerprint",
		previous, authOf(candidate), describeHashes(target), sess.describeClient())
	if sess.consumeRebind {
		message += ", allowed by " + elementalv1.AllowIdentityRebindAnnotation
	}
	sess.committed = append(sess.committed, func(committed *elementalv1.MachineInventory) {
		i.recordEvent(committed, corev1.EventTypeNormal, EventReasonIdentityRebound, message)
	})
	log.Infof("MachineInventory %s/%s: %s", candidate.Namespace, candidate.Name, message)
	return target
}

// findInventoryForHardware looks, cluster wide like the hash lookup, for the
// one inventory that recorded this strong fingerprint or, for inventories
// from before fingerprints, whose name or machine-uuid annotation carries
// the same real system UUID without contradiction. More than one match is
// ambiguous and moves nothing.
func (i *InventoryServer) findInventoryForHardware(fingerprint identity.Fingerprint, fields identity.Fields, rawUUID string, nameEvidence bool) (*elementalv1.MachineInventory, error) {
	list := &elementalv1.MachineInventoryList{}
	if err := i.List(i, list); err != nil {
		return nil, fmt.Errorf("cannot list MachineInventories: %w", err)
	}
	var matches []*elementalv1.MachineInventory
	for index := range list.Items {
		m := &list.Items[index]
		if m.DeletionTimestamp != nil {
			continue
		}
		if recorded := m.Annotations[elementalv1.HardwareFingerprintAnnotation]; recorded != "" {
			// Fingerprints of other versions cannot be compared.
			if recorded == fingerprint.Value {
				matches = append(matches, m)
			}
			continue
		}
		if fields.SystemUUID != "" && carriesUUID(m, fields.SystemUUID, nameEvidence) && oldEvidenceContradiction(m, rawUUID, nameEvidence) == "" {
			matches = append(matches, m)
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return matches[0].DeepCopy(), nil
	default:
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, m.Namespace+"/"+m.Name)
		}
		log.Warningf("Hardware fingerprint %s matches several MachineInventories (%s); not moving any identity", fingerprint.Value, strings.Join(names, ", "))
		return nil, nil
	}
}

// oldEvidenceContradiction compares what an inventory without fingerprint
// recorded about its hardware with the system UUID the host reports: the
// machine-uuid annotation (rendered at every registration) and, when the
// registration renders the system UUID into names (nameEvidence), the UUID
// in the name (rendered at creation). It returns why they disagree.
func oldEvidenceContradiction(inventory *elementalv1.MachineInventory, rawUUID string, nameEvidence bool) string {
	reported := identity.CanonicalUUID(rawUUID)
	if recorded := strings.TrimSpace(inventory.Annotations[elementalv1.MachineUUIDAnnotation]); recorded != "" && identity.CanonicalUUID(recorded) != reported {
		return fmt.Sprintf("annotation %s is %q, the registering host reports system UUID %q", elementalv1.MachineUUIDAnnotation, recorded, rawUUID)
	}
	if !nameEvidence {
		return ""
	}
	if inName := identity.UUIDsInName(inventory.Name); len(inName) > 0 && !slices.Contains(inName, reported) {
		return fmt.Sprintf("the inventory name carries system UUID %s, the registering host reports %q", strings.Join(inName, ", "), rawUUID)
	}
	return ""
}

func carriesUUID(inventory *elementalv1.MachineInventory, uuid string, nameEvidence bool) bool {
	if identity.CanonicalUUID(inventory.Annotations[elementalv1.MachineUUIDAnnotation]) == uuid {
		return true
	}
	return nameEvidence && slices.Contains(identity.UUIDsInName(inventory.Name), uuid)
}

// weakIdentity tells whether anybody knowing the hardware identifiers can
// present the inventory's current identity: emulated TPMs, sys-uuid and
// mac. Such identities move to the same hardware automatically; a hardware
// TPM needs the allow-identity-rebind annotation.
func weakIdentity(inventory *elementalv1.MachineInventory) bool {
	switch authOf(inventory) {
	case "emulated-tpm", "sys-uuid", "mac":
		return true
	case "tpm":
		return false
	}
	// No auth annotation (clients before v1.1.4): only plain auth is
	// recognizable.
	return inventory.Spec.TPMHash == "" && inventory.Spec.MachineHash != ""
}

func authOf(inventory *elementalv1.MachineInventory) string {
	return inventory.Annotations[elementalv1.MachineInventoryAuthAnnotation]
}

// rebindAllowedFor tells whether an operator allowed this hardware to take
// the inventory's identity: allow-identity-rebind is "true" (whichever
// hardware registers next), or names the hardware by its fingerprint, system
// UUID or a serial number, which settles which of two colliding hosts wins.
func rebindAllowedFor(inventory *elementalv1.MachineInventory, fields identity.Fields, fingerprint identity.Fingerprint) bool {
	value := strings.TrimSpace(inventory.Annotations[elementalv1.AllowIdentityRebindAnnotation])
	switch {
	case value == "":
		return false
	case strings.EqualFold(value, "true"), value == fingerprint.Value:
		return true
	}
	if uuid := identity.NormalizeUUID(value); uuid != "" && uuid == fields.SystemUUID {
		return true
	}
	serial := identity.NormalizeSerial(value)
	return serial != "" && (serial == fields.SystemSerial || serial == fields.BoardSerial)
}

func hasIdentityConflict(inventory *elementalv1.MachineInventory) bool {
	_, found := inventory.Annotations[elementalv1.IdentityConflictAnnotation]
	return found
}

func describeHashes(inventory *elementalv1.MachineInventory) string {
	switch {
	case inventory.Spec.TPMHash != "":
		return "tpmHash " + inventory.Spec.TPMHash
	case inventory.Spec.MachineHash != "":
		return "machineHash " + inventory.Spec.MachineHash
	default:
		return "no hash"
	}
}

func (i *InventoryServer) recordEvent(inventory *elementalv1.MachineInventory, eventType, reason, message string) {
	if i.EventRecorder == nil {
		return
	}
	i.EventRecorder.Event(inventory, eventType, reason, message)
}

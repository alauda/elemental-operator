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
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	elementalv1 "github.com/rancher/elemental-operator/api/v1beta1"
	"github.com/rancher/elemental-operator/pkg/log"
	"github.com/rancher/elemental-operator/pkg/register"
)

// Live install sessions: with install.device-selection: inventory, a host
// booted from the live ISO with --install reports its boot and disks
// (MsgLiveInstallSession) before MsgGet. The server records them in
// status.install and holds the host (MsgInstallHold) until a person picks the
// install disk with the one-shot install-target annotation; the next
// registration then consumes the annotation and releases the install. The
// released host reports how its install goes (MsgInstallReport) on a short
// session of its own.

// Event reasons of live install sessions, recorded on the MachineInventory.
const (
	EventReasonInstallReleased    = "InstallReleased"
	EventReasonInstallInterrupted = "InstallInterrupted"
	EventReasonInstallFailed      = "InstallFailed"
	EventReasonInstallCompleted   = "InstallCompleted"
	EventReasonSelectionDiscarded = "SelectionDiscarded"
)

// targetAmbiguousReason is what elemental-register reports when several disks
// carry the released ID; the API has no constant for it.
const targetAmbiguousReason = "TargetAmbiguous"

// installTargetRequiresSelection is install.device in the configuration of
// an inventory-mode registration that releases no install. No such disk
// exists, so a live ISO too old to wait for a selection fails before it
// touches a disk, while installed systems ignore the install section.
const installTargetRequiresSelection = "/dev/disk/by-id/elemental-install-target-requires-inventory-selection"

// allocationAnnotations are set by the baremetal provider on an inventory it
// allocated to a machine: such a host is never installed over.
var allocationAnnotations = []string{
	"baremetal.alauda.io/owner-cluster",
	"baremetal.alauda.io/owner-machine",
	"baremetal.alauda.io/owner-baremetalmachine",
}

const (
	// selectionRetrySeconds holds a host whose selection could not be
	// consumed only briefly: the next registration checks it again.
	selectionRetrySeconds = 5
	// liveBootAlternation: a live boot coming back within it after another
	// boot registered for the same inventory is a second host presenting
	// the same identity.
	liveBootAlternation = 2 * time.Minute
	// liveBootMemory is how long the boots of an inventory nobody registers
	// for are remembered.
	liveBootMemory = 10 * time.Minute
)

// Bounds of what a live install session stores on the inventory.
const (
	maxBootIDLength       = 128
	maxInstallDisks       = 128
	maxInstallDiskStrings = 16
	maxInstallField       = 256
	maxInstallMessage     = 1024
)

// now is the time of install session decisions.
func (i *InventoryServer) now() time.Time {
	if i.clock != nil {
		return i.clock()
	}
	return time.Now()
}

// decodeLiveInstallSession parses a MsgLiveInstallSession payload and bounds
// it: everything in it ends up in the inventory status.
func decodeLiveInstallSession(data []byte) (*register.LiveInstallSession, error) {
	session := &register.LiveInstallSession{}
	if err := json.Unmarshal(data, session); err != nil {
		return nil, fmt.Errorf("cannot decode live install session: %w", err)
	}
	session.BootID = strings.TrimSpace(session.BootID)
	switch {
	case session.BootID == "":
		return nil, errors.New("live install session has no boot ID")
	case len(session.BootID) > maxBootIDLength || strings.IndexFunc(session.BootID, notVisibleASCII) >= 0:
		return nil, fmt.Errorf("live install session boot ID %q is malformed", truncate(session.BootID, 64))
	}
	switch session.Installer {
	case "":
		session.Installer = elementalv1.InstallerToolkit
	case elementalv1.InstallerToolkit, elementalv1.InstallerKubeOS:
	default:
		return nil, fmt.Errorf("live install session has unknown installer %q", truncate(string(session.Installer), 64))
	}
	session.RegisterVersion = truncate(session.RegisterVersion, maxInstallField)
	session.SurveyError = truncate(strings.TrimSpace(session.SurveyError), maxInstallMessage)
	if len(session.Candidates) > maxInstallDisks {
		log.Warningf("Live boot %s reports %d disks, keeping the first %d", session.BootID, len(session.Candidates), maxInstallDisks)
		session.Candidates = session.Candidates[:maxInstallDisks]
	}
	for index := range session.Candidates {
		boundCandidate(&session.Candidates[index])
	}
	// Dropping stale disks only ever blocks an install: the host stops at a
	// stale system disk it was not allowed to wipe.
	if len(session.StaleSystemDisks) > maxInstallDisks {
		log.Warningf("Live boot %s reports %d stale system disks, keeping the first %d", session.BootID, len(session.StaleSystemDisks), maxInstallDisks)
		session.StaleSystemDisks = session.StaleSystemDisks[:maxInstallDisks]
	}
	for index := range session.StaleSystemDisks {
		boundStaleSystemDisk(&session.StaleSystemDisks[index])
	}
	return session, nil
}

func boundCandidate(candidate *elementalv1.InstallCandidate) {
	for _, field := range []*string{&candidate.ID, &candidate.Path, &candidate.Model, &candidate.Serial, &candidate.WWN, &candidate.Transport} {
		*field = truncate(*field, maxInstallField)
	}
	candidate.Reasons = boundStrings(candidate.Reasons)
	candidate.HasData = boundStrings(candidate.HasData)
}

// boundStaleSystemDisk also makes sure only the host's own verdict, Wipe or
// Blocked, gets on the record: anything else is Blocked, so it is never
// approved for wiping.
func boundStaleSystemDisk(disk *elementalv1.StaleSystemDisk) {
	disk.ID = truncate(disk.ID, maxInstallField)
	disk.Evidence = boundStrings(disk.Evidence)
	switch disk.Action {
	case elementalv1.StaleSystemDiskWipe, elementalv1.StaleSystemDiskBlocked:
	default:
		reason := fmt.Sprintf("elemental-register reported action %q", disk.Action)
		if disk.Reason != "" {
			reason += ": " + disk.Reason
		}
		disk.Action, disk.Reason = elementalv1.StaleSystemDiskBlocked, reason
	}
	disk.Reason = truncate(disk.Reason, maxInstallField)
}

func boundStrings(values []string) []string {
	if len(values) > maxInstallDiskStrings {
		values = values[:maxInstallDiskStrings]
	}
	for index := range values {
		values[index] = truncate(values[index], maxInstallField)
	}
	return values
}

func notVisibleASCII(r rune) bool {
	return r <= ' ' || r > '~'
}

// truncate cuts s to at most limit bytes without splitting a character.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// applyInstallSelection makes the install-target annotation of the inventory
// about to be committed what the stored inventory had: a person sets it on
// the MachineInventory, never a MachineRegistration template.
func (s *registrationSession) applyInstallSelection(inventory *elementalv1.MachineInventory) {
	value, found := "", false
	if s.stored != nil {
		value, found = s.stored.Annotations[elementalv1.InstallTargetDeviceAnnotation]
	}
	if !found {
		delete(inventory.Annotations, elementalv1.InstallTargetDeviceAnnotation)
		return
	}
	if inventory.Annotations == nil {
		inventory.Annotations = map[string]string{}
	}
	inventory.Annotations[elementalv1.InstallTargetDeviceAnnotation] = value
}

// inventoryMode tells whether a person picks the install disk on the
// MachineInventory.
func inventoryMode(registration *elementalv1.MachineRegistration) bool {
	return registration.Spec.Config.Elemental.Install.DeviceSelection == elementalv1.DeviceSelectionInventory
}

// installRound is one registration of a live install session.
type installRound struct {
	// inventory is the committed inventory.
	inventory *elementalv1.MachineInventory
	session   *register.LiveInstallSession
	now       time.Time
	// next is the status.install the round writes and targetReady its
	// InstallTargetReady condition; nil removes the condition.
	next        *elementalv1.InstallStatus
	targetReady *metav1.Condition
	// installedTo is the disk the host's installed system is on, when it is
	// known while the host waits in the live ISO.
	installedTo string
}

// handleLiveInstallGet answers MsgGet of a host that sent a live install
// session. Like handleGet it commits the inventory first; it then records the
// session in status.install and, in inventory mode, either releases the
// install to the disk a person picked or holds the host.
func (i *InventoryServer) handleLiveInstallGet(conn *websocket.Conn, protoVersion register.MessageType, inventory *elementalv1.MachineInventory, registration *elementalv1.MachineRegistration, sess *registrationSession) error {
	committed, err := i.commitMachineInventory(inventory)
	if err != nil {
		if writeErr := writeError(conn, protoVersion, register.NewErrorMessage(err)); writeErr != nil {
			log.Errorf("Error reporting back error to client: %v", writeErr)
		}
		return err
	}
	sess.afterCommit(committed)

	session := sess.liveInstall
	round := &installRound{inventory: committed, session: session, now: i.now()}
	round.next = (&elementalv1.InstallStatus{
		BootID:           session.BootID,
		Installer:        session.Installer,
		LastSeenAt:       metav1.NewTime(round.now),
		Candidates:       session.Candidates,
		StaleSystemDisks: session.StaleSystemDisks,
	}).DeepCopy()
	// A client that cannot be held installs right away, as in legacy mode.
	if !inventoryMode(registration) || protoVersion < register.MsgInstallHold {
		return i.installWithoutSelection(conn, protoVersion, round, registration)
	}
	return i.installFromSelection(conn, protoVersion, round, registration)
}

// installWithoutSelection records the session of a host that installs right
// away to the disk install.device or install.device-selector picks; in
// inventory mode that is no disk at all.
func (i *InventoryServer) installWithoutSelection(conn *websocket.Conn, protoVersion register.MessageType, round *installRound, registration *elementalv1.MachineRegistration) error {
	round.next.Phase = elementalv1.InstallPhaseInstalling
	// Nobody picks a disk, so there is no InstallTargetReady to show.
	round.targetReady = nil
	if err := i.writeInstallStatus(round); err != nil {
		log.Errorf("Cannot record the live install session of MachineInventory %s/%s: %v", round.inventory.Namespace, round.inventory.Name, err)
	}
	return i.sendConfig(conn, protoVersion, round.inventory, registration, withoutInstallRelease(registration))
}

// installFromSelection decides about a host that waits for a person to pick
// its install disk with the install-target annotation. The annotation is
// one-shot: it is consumed when it releases the install and discarded when it
// was set before this live boot registered or does not pass validation.
func (i *InventoryServer) installFromSelection(conn *websocket.Conn, protoVersion register.MessageType, round *installRound, registration *elementalv1.MachineRegistration) error {
	inventory, session := round.inventory, round.session
	prev := inventory.Status.Install
	newBoot := prev == nil || prev.BootID != session.BootID
	round.next.Message = installHistory(prev, newBoot)
	round.installedTo = installedTarget(prev, newBoot)
	if newBoot && prev != nil && prev.Phase == elementalv1.InstallPhaseInstalling {
		i.recordEvent(inventory, corev1.EventTypeWarning, EventReasonInstallInterrupted, round.next.Message)
	}

	selection, selected := inventory.Annotations[elementalv1.InstallTargetDeviceAnnotation]
	boots := i.liveBoots.observe(inventory.UID, session.BootID, round.now)
	if boots.alternating {
		pair := []string{session.BootID, boots.other}
		sort.Strings(pair)
		message := fmt.Sprintf("live boots %s and %s take turns registering: two hosts present the identity of this MachineInventory from the live ISO; give each host its own identity", pair[0], pair[1])
		if boots.returned {
			i.recordEvent(inventory, corev1.EventTypeWarning, EventReasonIdentityConflict, message)
		}
		if selected {
			i.discardSelection(inventory, selection, "two hosts present the identity of this MachineInventory")
		}
		return i.holdInstall(conn, round, elementalv1.IdentityConflictReason, message, register.DefaultInstallHoldSeconds)
	}

	hint := selectionHint(inventory, session)
	switch {
	case !selected:
		return i.holdInstall(conn, round, elementalv1.AwaitingSelectionReason, hint, register.DefaultInstallHoldSeconds)
	case newBoot:
		why := fmt.Sprintf("it was set before live boot %s registered", session.BootID)
		i.discardSelection(inventory, selection, why)
		return i.holdInstall(conn, round, elementalv1.SelectionDiscardedReason, discardedSelection(selection, why)+"; "+hint, register.DefaultInstallHoldSeconds)
	}
	candidate, reason, problem := checkSelection(inventory, session, selection)
	if candidate == nil {
		i.discardSelection(inventory, selection, problem)
		return i.holdInstall(conn, round, reason, discardedSelection(selection, problem), register.DefaultInstallHoldSeconds)
	}
	return i.releaseInstall(conn, protoVersion, round, registration, candidate)
}

// installHistory is status.install.message of a held host: what became of the
// last install attempt.
func installHistory(prev *elementalv1.InstallStatus, newBoot bool) string {
	if prev == nil {
		return ""
	}
	target := ""
	if prev.TargetDeviceID != "" {
		target = " to " + prev.TargetDeviceID
	}
	switch {
	case newBoot && prev.Phase == elementalv1.InstallPhaseInstalling:
		return fmt.Sprintf("the install released%s in live boot %s did not complete", target, prev.BootID)
	case newBoot && prev.Phase == elementalv1.InstallPhaseInstalled:
		return fmt.Sprintf("the host booted the live ISO again after the install%s completed; if the installation media stayed attached, remove it and boot from disk, or pick a disk to install again", target)
	case newBoot:
		return ""
	case prev.Phase == elementalv1.InstallPhaseInstalling:
		return fmt.Sprintf("the install released%s in this live boot did not complete", target)
	case prev.Phase == elementalv1.InstallPhaseFailed:
		return "the last install attempt in this live boot failed: " + prev.Message
	case prev.Phase == elementalv1.InstallPhaseAwaitingTarget:
		return prev.Message
	default:
		return ""
	}
}

// installedTarget is the disk a waiting host's installed system is on: the
// target of a completed install, kept while the host waits in the live ISO
// after it (installation media left attached, or a reinstall that was not
// pursued) until an install is released to another disk. An interrupted or
// failed install leaves no installed system it would name.
func installedTarget(prev *elementalv1.InstallStatus, newBoot bool) string {
	switch {
	case prev == nil:
		return ""
	case newBoot && prev.Phase == elementalv1.InstallPhaseInstalled,
		!newBoot && prev.Phase == elementalv1.InstallPhaseAwaitingTarget:
		return prev.TargetDeviceID
	default:
		return ""
	}
}

// selectionHint tells a person how to release the install.
func selectionHint(inventory *elementalv1.MachineInventory, session *register.LiveInstallSession) string {
	if session.SurveyError != "" {
		return "the host could not list its disks: " + session.SurveyError
	}
	eligible := 0
	for _, candidate := range session.Candidates {
		if candidate.Eligible {
			eligible++
		}
	}
	if eligible == 0 {
		return fmt.Sprintf("none of the %d disks the host reported can be the install target (status.install.candidates)", len(session.Candidates))
	}
	return fmt.Sprintf("%d of %d disks the host reported can be the install target (status.install.candidates); pick one with: kubectl -n %s annotate machineinventory %s %s=<id>",
		eligible, len(session.Candidates), inventory.Namespace, inventory.Name, elementalv1.InstallTargetDeviceAnnotation)
}

func discardedSelection(selection, why string) string {
	return fmt.Sprintf("discarded %s=%q: %s", elementalv1.InstallTargetDeviceAnnotation, truncate(selection, maxInstallField), why)
}

// checkSelection validates the install-target annotation against what the
// host reported in this registration. It returns the chosen candidate, or the
// InstallTargetReady reason and why the selection cannot be used.
func checkSelection(inventory *elementalv1.MachineInventory, session *register.LiveInstallSession, selection string) (*elementalv1.InstallCandidate, string, string) {
	if session.SurveyError != "" {
		return nil, elementalv1.TargetNotObservedReason, "the host could not list its disks: " + session.SurveyError
	}
	// Without the provider's webhook nobody rejected a hand-written form
	// such as wwn:0x5000C500...; match it the way the host writes IDs.
	id := register.NormalizeDeviceID(selection)
	if id == "" {
		return nil, elementalv1.TargetNotObservedReason, "the value is empty"
	}
	var matches []*elementalv1.InstallCandidate
	for index := range session.Candidates {
		if session.Candidates[index].ID == id {
			matches = append(matches, &session.Candidates[index])
		}
	}
	switch len(matches) {
	case 0:
		return nil, elementalv1.TargetNotObservedReason, fmt.Sprintf("live boot %s reported no disk with this ID (status.install.candidates)", session.BootID)
	case 1:
	default:
		return nil, elementalv1.TargetNotObservedReason, fmt.Sprintf("live boot %s reported %d disks with this ID", session.BootID, len(matches))
	}
	candidate := matches[0]
	switch {
	case !candidate.Eligible:
		why := "the disk cannot be the install target"
		if len(candidate.Reasons) > 0 {
			why += ": " + strings.Join(candidate.Reasons, "; ")
		}
		return nil, elementalv1.TargetIneligibleReason, why
	case strings.HasPrefix(id, "unidentified:"):
		return nil, elementalv1.TargetIneligibleReason, "the disk has no stable ID"
	}
	if owners := allocationOf(inventory); owners != "" {
		return nil, elementalv1.TargetIneligibleReason, fmt.Sprintf("the inventory is allocated (%s): an allocated host is never installed over", owners)
	}
	if hasIdentityConflict(inventory) {
		return nil, elementalv1.IdentityConflictReason, fmt.Sprintf("the MachineInventory is marked %s: resolve the identity conflict first", elementalv1.IdentityConflictAnnotation)
	}
	if blocked := blockedStaleSystemDisks(session.StaleSystemDisks, id); blocked != "" {
		return nil, elementalv1.BlockedReason, fmt.Sprintf("stale system disks cannot be wiped safely: %s; deal with them, then pick the disk again", blocked)
	}
	return candidate, "", ""
}

// allocationOf describes the allocation annotations of the inventory, "" when
// it is not allocated.
func allocationOf(inventory *elementalv1.MachineInventory) string {
	var owners []string
	for _, key := range allocationAnnotations {
		if value := strings.TrimSpace(inventory.Annotations[key]); value != "" {
			owners = append(owners, key+"="+value)
		}
	}
	return strings.Join(owners, ", ")
}

// blockedStaleSystemDisks lists the stale system disks other than the target
// that the host cannot wipe safely.
func blockedStaleSystemDisks(disks []elementalv1.StaleSystemDisk, target string) string {
	var blocked []string
	for _, disk := range disks {
		if disk.Action == elementalv1.StaleSystemDiskBlocked && disk.ID != target {
			blocked = append(blocked, describeStaleSystemDisk(disk))
		}
	}
	return strings.Join(blocked, "; ")
}

func describeStaleSystemDisk(disk elementalv1.StaleSystemDisk) string {
	if disk.Reason == "" {
		return disk.ID
	}
	return disk.ID + " (" + disk.Reason + ")"
}

func describeCandidate(candidate *elementalv1.InstallCandidate) string {
	var facts []string
	if candidate.Path != "" {
		facts = append(facts, candidate.Path)
	}
	if candidate.SizeBytes > 0 {
		facts = append(facts, fmt.Sprintf("%d bytes", candidate.SizeBytes))
	}
	if candidate.Model != "" {
		facts = append(facts, candidate.Model)
	}
	if len(facts) == 0 {
		return candidate.ID
	}
	return fmt.Sprintf("%s (%s)", candidate.ID, strings.Join(facts, ", "))
}

// releaseInstall consumes the selection and sends the configuration that
// installs to the chosen disk and wipes the stale system disks a person saw
// listed. The release is recorded before the configuration leaves.
func (i *InventoryServer) releaseInstall(conn *websocket.Conn, protoVersion register.MessageType, round *installRound, registration *elementalv1.MachineRegistration, candidate *elementalv1.InstallCandidate) error {
	inventory := round.inventory
	selection := inventory.Annotations[elementalv1.InstallTargetDeviceAnnotation]
	// Guarded by the resourceVersion of the committed inventory: what gets
	// consumed is the value validated above.
	if err := i.removeSelection(inventory); err != nil {
		log.Warningf("Cannot consume %s of MachineInventory %s/%s: %v", elementalv1.InstallTargetDeviceAnnotation, inventory.Namespace, inventory.Name, err)
		message := fmt.Sprintf("%s=%q is checked again: the MachineInventory changed while it was consumed", elementalv1.InstallTargetDeviceAnnotation, selection)
		if !apierrors.IsConflict(err) {
			message = fmt.Sprintf("%s=%q is checked again: it could not be consumed: %v", elementalv1.InstallTargetDeviceAnnotation, selection, err)
		}
		return i.holdInstall(conn, round, elementalv1.AwaitingSelectionReason, message, selectionRetrySeconds)
	}

	next := round.next
	next.Phase = elementalv1.InstallPhaseInstalling
	next.TargetDeviceID = candidate.ID
	next.Message = ""
	wipe := []string{}
	for index := range next.StaleSystemDisks {
		disk := &next.StaleSystemDisks[index]
		switch {
		case disk.ID == candidate.ID:
			disk.Action = elementalv1.StaleSystemDiskTarget
		case disk.Action == elementalv1.StaleSystemDiskWipe:
			wipe = append(wipe, disk.ID)
		}
	}
	released := fmt.Sprintf("the install was released to %s in live boot %s", describeCandidate(candidate), round.session.BootID)
	round.targetReady = &metav1.Condition{
		Type:    elementalv1.InstallTargetReadyCondition,
		Status:  metav1.ConditionTrue,
		Reason:  elementalv1.ReleasedReason,
		Message: released,
	}
	if err := i.writeInstallStatus(round); err != nil {
		err = fmt.Errorf("cannot record the install release on MachineInventory %s/%s: %w", inventory.Namespace, inventory.Name, err)
		if writeErr := writeError(conn, protoVersion, register.NewErrorMessage(err)); writeErr != nil {
			log.Errorf("Error reporting back error to client: %v", writeErr)
		}
		return err
	}
	if len(wipe) > 0 {
		released += "; it wipes the stale system disks " + strings.Join(wipe, ", ")
	}
	i.recordEvent(inventory, corev1.EventTypeNormal, EventReasonInstallReleased, released)
	log.Infof("MachineInventory %s/%s: %s", inventory.Namespace, inventory.Name, released)

	target := elementalv1.InstallTarget{
		DeviceID: candidate.ID,
		Expect: elementalv1.InstallTargetExpect{
			SizeBytes: candidate.SizeBytes,
			WWN:       candidate.WWN,
			Serial:    candidate.Serial,
			Model:     candidate.Model,
		},
		// Set even when empty: then no stale system disk may be wiped.
		WipeDeviceIDs: &wipe,
	}
	return i.sendConfig(conn, protoVersion, inventory, registration, installReleasedTo(target))
}

// holdInstall records the round with the host waiting for an install target
// and answers MsgGet with MsgInstallHold: the host touches no disk and
// registers again after retryAfter seconds. reason is the InstallTargetReady
// reason.
func (i *InventoryServer) holdInstall(conn *websocket.Conn, round *installRound, reason, message string, retryAfter int) error {
	inventory := round.inventory
	message = truncate(message, maxInstallMessage)
	round.next.Phase = elementalv1.InstallPhaseAwaitingTarget
	round.next.TargetDeviceID = round.installedTo
	round.targetReady = &metav1.Condition{
		Type:    elementalv1.InstallTargetReadyCondition,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	}
	if err := i.writeInstallStatus(round); err != nil {
		log.Errorf("Cannot record the live install session of MachineInventory %s/%s: %v", inventory.Namespace, inventory.Name, err)
	}
	data, err := json.Marshal(register.InstallHold{Reason: reason, Message: message, RetryAfterSeconds: retryAfter})
	if err != nil {
		return fmt.Errorf("cannot encode install hold: %w", err)
	}
	log.Infof("Holding live boot %s of MachineInventory %s/%s: %s: %s", round.session.BootID, inventory.Namespace, inventory.Name, reason, message)
	// The host does not answer a hold: SendJSONData would wait for MsgReady.
	if err := register.WriteMessage(conn, register.MsgInstallHold, data); err != nil {
		return fmt.Errorf("cannot send install hold: %w", err)
	}
	return nil
}

// writeInstallStatus writes the status.install of the round and the two
// install conditions, and nothing else of the status.
func (i *InventoryServer) writeInstallStatus(round *installRound) error {
	install := round.next.DeepCopy()
	install.Message = truncate(install.Message, maxInstallMessage)
	stale := staleSystemDisksCondition(install.StaleSystemDisks)
	return i.patchInstallStatus(round.inventory, func(status *elementalv1.MachineInventoryStatus) bool {
		status.Install = install.DeepCopy()
		if round.targetReady != nil {
			setInstallCondition(status, *round.targetReady, round.now)
		} else {
			meta.RemoveStatusCondition(&status.Conditions, elementalv1.InstallTargetReadyCondition)
		}
		setInstallCondition(status, stale, round.now)
		return true
	})
}

// staleSystemDisksCondition is the StaleSystemDisks condition of a live
// session: what the install is going to do about them.
func staleSystemDisksCondition(disks []elementalv1.StaleSystemDisk) metav1.Condition {
	var blocked, detected []string
	for _, disk := range disks {
		switch disk.Action {
		case elementalv1.StaleSystemDiskTarget:
		case elementalv1.StaleSystemDiskBlocked:
			blocked = append(blocked, describeStaleSystemDisk(disk))
		default:
			detected = append(detected, disk.ID)
		}
	}
	condition := metav1.Condition{Type: elementalv1.StaleSystemDisksCondition, Status: metav1.ConditionFalse}
	switch {
	case len(blocked) > 0:
		condition.Reason = elementalv1.BlockedReason
		condition.Message = "stale system disks cannot be wiped safely, the install stops until a person deals with them: " + strings.Join(blocked, "; ")
	case len(detected) > 0:
		condition.Reason = elementalv1.DetectedReason
		condition.Message = "stale system disks will be wiped when the install runs: " + strings.Join(detected, ", ")
	default:
		condition.Status = metav1.ConditionTrue
		condition.Reason = elementalv1.NoneDetectedReason
		condition.Message = "no stale system disk was detected"
	}
	return condition
}

func setInstallCondition(status *elementalv1.MachineInventoryStatus, condition metav1.Condition, now time.Time) {
	condition.Message = truncate(condition.Message, maxInstallMessage)
	condition.LastTransitionTime = metav1.NewTime(now)
	meta.SetStatusCondition(&status.Conditions, condition)
}

// patchInstallStatus lets update change the status of the inventory and
// patches it, starting from the inventory as given and, after a conflict,
// from a fresh read. update returns false when there is nothing to write. On
// success the inventory is what the API server stored.
func (i *InventoryServer) patchInstallStatus(inventory *elementalv1.MachineInventory, update func(*elementalv1.MachineInventoryStatus) bool) error {
	current := inventory.DeepCopy()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		base := current.DeepCopy()
		if !update(&current.Status) {
			return nil
		}
		err := i.Status().Patch(i, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if err == nil {
			current.DeepCopyInto(inventory)
			return nil
		}
		if apierrors.IsConflict(err) {
			fresh := &elementalv1.MachineInventory{}
			if getErr := i.Get(i, client.ObjectKeyFromObject(inventory), fresh); getErr != nil {
				return getErr
			}
			if fresh.UID != inventory.UID {
				return fmt.Errorf("MachineInventory UID changed from %q to %q", inventory.UID, fresh.UID)
			}
			current = fresh
		}
		return err
	})
}

// removeSelection removes the install-target annotation, provided the
// inventory did not change since it was read: what is removed is the value
// that was looked at. On success the inventory is what the API server stored.
func (i *InventoryServer) removeSelection(inventory *elementalv1.MachineInventory) error {
	if _, found := inventory.Annotations[elementalv1.InstallTargetDeviceAnnotation]; !found {
		return nil
	}
	patched := inventory.DeepCopy()
	delete(patched.Annotations, elementalv1.InstallTargetDeviceAnnotation)
	if err := i.Patch(i, patched, client.MergeFromWithOptions(inventory, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	patched.DeepCopyInto(inventory)
	return nil
}

// discardSelection removes the install-target annotation without releasing
// anything. When the inventory changed meanwhile the annotation stays for the
// next registration to look at again.
func (i *InventoryServer) discardSelection(inventory *elementalv1.MachineInventory, selection, why string) {
	if err := i.removeSelection(inventory); err != nil {
		log.Warningf("Cannot discard %s of MachineInventory %s/%s: %v", elementalv1.InstallTargetDeviceAnnotation, inventory.Namespace, inventory.Name, err)
		return
	}
	message := discardedSelection(selection, why)
	i.recordEvent(inventory, corev1.EventTypeWarning, EventReasonSelectionDiscarded, message)
	log.Infof("MachineInventory %s/%s: %s", inventory.Namespace, inventory.Name, message)
}

// settleInstall runs after a registration without a live install session: an
// installed system (or a client from before sessions). An install that was
// released or failed is then over, the system it installed registered; a
// selection left on the inventory goes with it. registered is set for a
// registration update, i.e. a host with registration state: an installed
// system. Only such a host also ends a wait for an install target, as when
// the installation media stayed attached and the host went back to the live
// ISO once before someone booted it from its disk.
func (i *InventoryServer) settleInstall(inventory *elementalv1.MachineInventory, registered bool) {
	if !installUnsettled(inventory.Status.Install, registered) {
		return
	}
	if err := i.removeSelection(inventory); err != nil {
		log.Warningf("Cannot remove %s from MachineInventory %s/%s: %v", elementalv1.InstallTargetDeviceAnnotation, inventory.Namespace, inventory.Name, err)
	}
	now := i.now()
	err := i.patchInstallStatus(inventory, func(status *elementalv1.MachineInventoryStatus) bool {
		install := status.Install
		if !installUnsettled(install, registered) {
			return false
		}
		waited := install.Phase == elementalv1.InstallPhaseAwaitingTarget
		install.Phase = elementalv1.InstallPhaseInstalled
		install.Message = ""
		if waited {
			// The disks were surveyed by a live boot that installed
			// nothing: they say nothing about the running system.
			meta.RemoveStatusCondition(&status.Conditions, elementalv1.InstallTargetReadyCondition)
			meta.RemoveStatusCondition(&status.Conditions, elementalv1.StaleSystemDisksCondition)
			return true
		}
		setInstallCondition(status, installedStaleSystemDisksCondition(install.StaleSystemDisks), now)
		return true
	})
	if err != nil {
		log.Errorf("Cannot record the install of MachineInventory %s/%s as done: %v", inventory.Namespace, inventory.Name, err)
		return
	}
	log.Infof("MachineInventory %s/%s: the installed system registered", inventory.Namespace, inventory.Name)
}

func installUnsettled(install *elementalv1.InstallStatus, registered bool) bool {
	if install == nil {
		return false
	}
	switch install.Phase {
	case elementalv1.InstallPhaseInstalling, elementalv1.InstallPhaseFailed:
		return true
	case elementalv1.InstallPhaseAwaitingTarget:
		return registered
	default:
		return false
	}
}

func installedStaleSystemDisksCondition(disks []elementalv1.StaleSystemDisk) metav1.Condition {
	var wiped []string
	for _, disk := range disks {
		if disk.Action == elementalv1.StaleSystemDiskWipe {
			wiped = append(wiped, disk.ID)
		}
	}
	if len(wiped) == 0 {
		return metav1.Condition{Type: elementalv1.StaleSystemDisksCondition, Status: metav1.ConditionTrue,
			Reason: elementalv1.NoneDetectedReason, Message: "no stale system disk was detected"}
	}
	return metav1.Condition{Type: elementalv1.StaleSystemDisksCondition, Status: metav1.ConditionTrue,
		Reason: elementalv1.WipedReason, Message: "the install wiped the stale system disks " + strings.Join(wiped, ", ")}
}

// withoutInstallRelease adjusts the configuration of a MsgGet that releases no
// install. In inventory mode only a released install-target may direct an
// install, so install.device names a disk that does not exist.
func withoutInstallRelease(registration *elementalv1.MachineRegistration) func(*elementalv1.Config) {
	if !inventoryMode(registration) {
		return nil
	}
	return func(config *elementalv1.Config) {
		config.Elemental.Install.Device = installTargetRequiresSelection
		config.Elemental.Install.DeviceSelector = nil
		config.Elemental.InstallTarget = nil
	}
}

// installReleasedTo adjusts the configuration to install to the released
// target and nothing else.
func installReleasedTo(target elementalv1.InstallTarget) func(*elementalv1.Config) {
	return func(config *elementalv1.Config) {
		config.Elemental.Install.Device = ""
		config.Elemental.Install.DeviceSelector = nil
		config.Elemental.InstallTarget = &target
	}
}

// handleInstallReport records the outcome a released host reports on a short
// session of its own, then ends the session.
func (i *InventoryServer) handleInstallReport(conn *websocket.Conn, protoVersion register.MessageType, inventory *elementalv1.MachineInventory, data []byte) error {
	if err := i.recordInstallReport(inventory, data); err != nil {
		if writeErr := writeError(conn, protoVersion, register.NewErrorMessage(err)); writeErr != nil {
			log.Errorf("Error reporting back error to client: %v", writeErr)
		}
		return err
	}
	if err := register.WriteMessage(conn, register.MsgReady, nil); err != nil {
		return fmt.Errorf("cannot acknowledge the install report: %w", err)
	}
	return nil
}

// recordInstallReport applies a MsgInstallReport payload to status.install. A
// report from another live boot than the one recorded is ignored: it is
// stale.
func (i *InventoryServer) recordInstallReport(inventory *elementalv1.MachineInventory, data []byte) error {
	if isNewInventory(inventory) || inventory.UID == "" {
		return errors.New("an install report needs a registered MachineInventory")
	}
	report, err := decodeInstallReport(data)
	if err != nil {
		return err
	}
	now := i.now()
	applied := false
	err = i.patchInstallStatus(inventory, func(status *elementalv1.MachineInventoryStatus) bool {
		install := status.Install
		applied = install != nil && install.BootID == report.BootID
		if !applied {
			return false
		}
		install.Phase = report.Phase
		if report.TargetDeviceID != "" {
			install.TargetDeviceID = report.TargetDeviceID
		}
		switch report.Phase {
		case elementalv1.InstallPhaseFailed:
			install.Message = report.Message
			if condition := reportedCondition(report); condition != nil {
				setInstallCondition(status, *condition, now)
			}
		case elementalv1.InstallPhaseInstalled:
			// The disk is installed and the host reboots into it; its
			// first registration finds nothing left to settle.
			install.Message = ""
			setInstallCondition(status, installedStaleSystemDisksCondition(install.StaleSystemDisks), now)
		}
		return true
	})
	if err != nil {
		return fmt.Errorf("cannot record the install report on MachineInventory %s/%s: %w", inventory.Namespace, inventory.Name, err)
	}
	if !applied {
		log.Warningf("Ignoring the install report of live boot %s for MachineInventory %s/%s: status.install belongs to another boot", report.BootID, inventory.Namespace, inventory.Name)
		return nil
	}
	switch report.Phase {
	case elementalv1.InstallPhaseFailed:
		message := fmt.Sprintf("the install in live boot %s failed", report.BootID)
		if report.Reason != "" {
			message += " (" + report.Reason + ")"
		}
		i.recordEvent(inventory, corev1.EventTypeWarning, EventReasonInstallFailed, message+": "+report.Message)
	case elementalv1.InstallPhaseInstalled:
		target := ""
		if report.TargetDeviceID != "" {
			target = " to " + report.TargetDeviceID
		}
		i.recordEvent(inventory, corev1.EventTypeNormal, EventReasonInstallCompleted, fmt.Sprintf("the install%s in live boot %s completed; the host reboots into the installed system", target, report.BootID))
	}
	log.Infof("MachineInventory %s/%s: live boot %s reports the install %s", inventory.Namespace, inventory.Name, report.BootID, report.Phase)
	return nil
}

// decodeInstallReport parses a MsgInstallReport payload and bounds it.
func decodeInstallReport(data []byte) (*register.InstallReport, error) {
	report := &register.InstallReport{}
	if err := json.Unmarshal(data, report); err != nil {
		return nil, fmt.Errorf("cannot decode install report: %w", err)
	}
	switch report.Phase {
	case elementalv1.InstallPhaseInstalling, elementalv1.InstallPhaseInstalled, elementalv1.InstallPhaseFailed:
	default:
		return nil, fmt.Errorf("install report has phase %q, expected %s, %s or %s", truncate(string(report.Phase), 64),
			elementalv1.InstallPhaseInstalling, elementalv1.InstallPhaseInstalled, elementalv1.InstallPhaseFailed)
	}
	report.BootID = strings.TrimSpace(report.BootID)
	report.TargetDeviceID = truncate(strings.TrimSpace(report.TargetDeviceID), maxInstallField)
	report.Reason = truncate(strings.TrimSpace(report.Reason), maxInstallField)
	report.Message = truncate(strings.TrimSpace(report.Message), maxInstallMessage)
	if report.Phase == elementalv1.InstallPhaseFailed && report.Message == "" {
		report.Message = "elemental-register reported no details"
	}
	return report, nil
}

// reportedCondition is the condition a failed install sets: its reason says
// whether the target disk or a stale system disk stopped the install.
func reportedCondition(report *register.InstallReport) *metav1.Condition {
	condition := &metav1.Condition{Status: metav1.ConditionFalse, Reason: report.Reason, Message: report.Message}
	switch report.Reason {
	case elementalv1.BlockedReason, elementalv1.WipeFailedReason:
		condition.Type = elementalv1.StaleSystemDisksCondition
	case elementalv1.TargetNotObservedReason, elementalv1.TargetIneligibleReason, elementalv1.TargetMismatchReason, targetAmbiguousReason:
		condition.Type = elementalv1.InstallTargetReadyCondition
	default:
		return nil
	}
	return condition
}

// liveBootTracker remembers the last two live boots that registered for each
// inventory. A host that reboots never comes back to an earlier boot, while
// two hosts presenting the same identity from the live ISO take turns. It
// lives in memory only: an operator restart forgets it.
type liveBootTracker struct {
	mu    sync.Mutex
	boots map[types.UID]*liveBoots
}

type liveBoots struct {
	current, previous string
	// changedAt is when current took over from previous, seenAt the last
	// registration and alternatedAt the last time previous came back.
	changedAt, seenAt, alternatedAt time.Time
}

// liveBootObservation is what a registration tells about the boots of an
// inventory.
type liveBootObservation struct {
	// alternating: a boot came back within liveBootAlternation, in this
	// registration (returned) or not long before.
	alternating, returned bool
	// other is the boot the registering one took over from.
	other string
}

// observe records a registration of bootID for the inventory.
func (t *liveBootTracker) observe(uid types.UID, bootID string, now time.Time) liveBootObservation {
	if uid == "" {
		return liveBootObservation{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, boots := range t.boots {
		if now.Sub(boots.seenAt) > liveBootMemory {
			delete(t.boots, key)
		}
	}
	if t.boots == nil {
		t.boots = map[types.UID]*liveBoots{}
	}
	boots := t.boots[uid]
	if boots == nil {
		t.boots[uid] = &liveBoots{current: bootID, changedAt: now, seenAt: now}
		return liveBootObservation{}
	}
	observed := liveBootObservation{}
	if bootID != boots.current {
		if bootID == boots.previous && now.Sub(boots.changedAt) < liveBootAlternation {
			boots.alternatedAt = now
			observed.returned = true
		}
		boots.previous, boots.current, boots.changedAt = boots.current, bootID, now
	}
	boots.seenAt = now
	observed.other = boots.previous
	observed.alternating = !boots.alternatedAt.IsZero() && now.Sub(boots.alternatedAt) < liveBootAlternation
	return observed
}

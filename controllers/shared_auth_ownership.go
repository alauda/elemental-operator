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

package controllers

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Who maintains the shared system-agent ServiceAccount, token Secret, Role
// and RoleBinding. On a disaster-recovery pair etcd-sync copies them from the
// primary Global to the standby, so the standby must only check them.
const (
	// SharedAuthOwnershipAuto is read-only while this cluster is the target of
	// etcd-sync, and the owner otherwise.
	SharedAuthOwnershipAuto = "auto"
	// SharedAuthOwnershipOwner always maintains the shared identity.
	SharedAuthOwnershipOwner = "owner"
	// SharedAuthOwnershipReadOnly never changes the shared identity.
	SharedAuthOwnershipReadOnly = "readonly"
)

const (
	// etcd-sync keeps this ConfigMap on the cluster it writes to, from before
	// the mirror starts until after it stops, including while it is paused.
	// The bare-metal provider, DCS, HCS and cluster-transformer use the same
	// object to tell whether their side is the standby.
	etcdSyncNamespace     = metav1.NamespacePublic
	etcdSyncConfigMapName = "etcd-sync"
	// etcdSyncProtectGap keeps a side read-only for a while after the
	// ConfigMap disappears, like the other components, so a side that just
	// stopped being a target does not race the last writes.
	etcdSyncProtectGap = time.Minute
	// etcdSyncRetryInterval is how soon a failed lookup is retried.
	etcdSyncRetryInterval = 10 * time.Second
)

// NormalizeSharedAuthOwnership validates an ownership value; empty is auto.
func NormalizeSharedAuthOwnership(ownership string) (string, error) {
	ownership = strings.ToLower(strings.TrimSpace(ownership))
	switch ownership {
	case "":
		return SharedAuthOwnershipAuto, nil
	case SharedAuthOwnershipAuto, SharedAuthOwnershipOwner, SharedAuthOwnershipReadOnly:
		return ownership, nil
	default:
		return "", fmt.Errorf("invalid shared system-agent auth ownership %q, valid values: %q, %q, %q",
			ownership, SharedAuthOwnershipAuto, SharedAuthOwnershipOwner, SharedAuthOwnershipReadOnly)
	}
}

// sharedAuthVerdict is the outcome of one ownership check.
type sharedAuthVerdict struct {
	readOnly bool
	// recheckAfter is set while the verdict is due to change without an
	// event: inside the protection gap, or after a failed lookup.
	recheckAfter time.Duration
	reason       string
}

// etcdSyncObserver tells whether this cluster is the target of etcd-sync, with
// the same protection gap as the other ACP components.
type etcdSyncObserver struct {
	reader client.Reader
	now    func() time.Time

	lock      sync.Mutex
	syncing   bool
	changedAt time.Time
	lastRO    *bool
}

func newEtcdSyncObserver(reader client.Reader) *etcdSyncObserver {
	o := &etcdSyncObserver{reader: reader, now: time.Now}
	// A ConfigMap that was never seen does not start a protection gap.
	o.changedAt = o.now().Add(-etcdSyncProtectGap)
	return o
}

func (o *etcdSyncObserver) observe(ctx context.Context) sharedAuthVerdict {
	cm := &corev1.ConfigMap{}
	err := o.reader.Get(ctx, client.ObjectKey{Namespace: etcdSyncNamespace, Name: etcdSyncConfigMapName}, cm)
	if err != nil && !apierrors.IsNotFound(err) {
		// Not knowing whether etcd-sync writes here, change nothing.
		return sharedAuthVerdict{
			readOnly:     true,
			recheckAfter: etcdSyncRetryInterval,
			reason:       fmt.Sprintf("cannot read %s/%s: %v", etcdSyncNamespace, etcdSyncConfigMapName, err),
		}
	}
	present := err == nil

	o.lock.Lock()
	defer o.lock.Unlock()
	now := o.now()
	if present != o.syncing {
		o.syncing = present
		o.changedAt = now
	}
	if o.syncing {
		return sharedAuthVerdict{readOnly: true, reason: fmt.Sprintf("%s/%s present", etcdSyncNamespace, etcdSyncConfigMapName)}
	}
	if elapsed := now.Sub(o.changedAt); elapsed < etcdSyncProtectGap {
		return sharedAuthVerdict{
			readOnly:     true,
			recheckAfter: etcdSyncProtectGap - elapsed,
			reason:       fmt.Sprintf("%s/%s absent for less than %s", etcdSyncNamespace, etcdSyncConfigMapName, etcdSyncProtectGap),
		}
	}
	return sharedAuthVerdict{reason: fmt.Sprintf("%s/%s absent", etcdSyncNamespace, etcdSyncConfigMapName)}
}

// transition records the verdict and returns the previous one when it
// differs, so a role change is logged once. The first verdict counts as a
// change from "unknown".
func (o *etcdSyncObserver) transition(readOnly bool) (string, bool) {
	o.lock.Lock()
	defer o.lock.Unlock()
	previous := "unknown"
	if o.lastRO != nil {
		if *o.lastRO == readOnly {
			return "", false
		}
		previous = sharedAuthRole(*o.lastRO)
	}
	o.lastRO = &readOnly
	return previous, true
}

func sharedAuthRole(readOnly bool) string {
	if readOnly {
		return "read-only"
	}
	return "owner"
}

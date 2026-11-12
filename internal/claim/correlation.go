/*
Copyright 2025.

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

package claim

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
)

// The two bounds a finished lifecycle Job is correlated to. A Job is the fact a recovery reads, and
// a fact about a slot is not a fact about the operation currently running on it: the two helpers
// below are the instants the current operation cannot predate.

// BootCommittedAt is the instant the boot of a claim became committed: the lastTransitionTime of its
// MachineCommitted condition, which the recorder stamps on the pending -> booting write - the same
// write that records that the attempt bought a machine, and the pass boundary the startup Job is
// created on the far side of (fsm.StartNode moves the claim, then creates the Job). A claim whose
// condition is missing - a phase landed on by a hand write - falls back to its creation timestamp,
// which is the oldest instant its boot can have started.
//
// It is the lower bound of the recovery of a finished startup Job: a Job created before it belongs to
// an earlier episode of the slot, and re-firing that Job's completion for the claim running now is
// one boot's evidence attributed to another.
func BootCommittedAt(c *infrav2alpha1.NodeClaim) time.Time {
	if c == nil {
		return time.Time{}
	}

	for _, cond := range c.Status.Conditions {
		if cond.Type != infrav2alpha1.ClaimConditionMachineCommitted || cond.Status != metav1.ConditionTrue {
			continue
		}

		if !cond.LastTransitionTime.IsZero() {
			return cond.LastTransitionTime.Time
		}
	}

	return c.CreationTimestamp.Time
}

// ScaleDownRequestedAt is the instant a scale-down named a claim: the deletionTimestamp
// NodeGroupDeleteNodes (or a hand delete, or an expired manual claim) lands on it. The zero time says
// no scale-down has named it, and the shutdown of a machine no claim asked away belongs to no
// scale-down whose Job a recovery may re-fire.
//
// It is the lower bound of the recovery of a finished shutdown Job: the shutdown the current
// scale-down created is the only shutdown this pass answers for, and a Job older than the request
// that started it is the record of a scale-down that already ended.
func ScaleDownRequestedAt(c *infrav2alpha1.NodeClaim) time.Time {
	if c == nil || c.DeletionTimestamp == nil {
		return time.Time{}
	}

	return c.DeletionTimestamp.Time
}

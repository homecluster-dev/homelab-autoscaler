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

package fsm

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// JobsInFlightOfSlot reports whether a startup Job and whether a shutdown Job of one slot exist and
// have not finished: the two `*JobInFlight` fields of phase.Input, answered in one place.
//
// It lives here because the labels it reads are the ones this package stamps on the Jobs it creates
// (lifecycleLabels: LabelNodeCR is the slot, LabelPhase the direction), and because **both** writers
// of the derived report need the answer. The transition path assembles the triple it feeds the state
// machine; the observation path assembles the same triple from the node object it was woken by. A
// phase has one formula (phase.Derive), so an input has one builder: two callers computing one field
// two ways is two chances for the report to answer two questions about one machine - and since the
// record of the report became a writer's job (phase.Record), a diverging pair does not only report
// twice, it appends two rows to one slot's ring and counts one boot twice.
//
// A list the API refuses is answered "nothing is in flight", and the caller's report is then derived
// from the claim and the observation alone: the answer is a fact about objects that exist, and a pass
// that cannot read them has no evidence to add. The next pass re-reads.
func JobsInFlightOfSlot(ctx context.Context, c client.Client, namespace, slot string) (bool, bool) {
	jobList := &batchv1.JobList{}
	selector := labels.SelectorFromSet(labels.Set{LabelNodeCR: slot})

	if err := c.List(ctx, jobList, &client.ListOptions{Namespace: namespace, LabelSelector: selector}); err != nil {
		log.FromContext(ctx).V(1).Info("Failed to list the lifecycle Jobs of the slot",
			"node", slot, "error", err.Error())

		return false, false
	}

	var startup, shutdown bool

	for i := range jobList.Items {
		job := &jobList.Items[i]
		if !job.DeletionTimestamp.IsZero() || job.Status.Succeeded > 0 || job.Status.Failed > 0 {
			continue
		}

		switch job.Labels[LabelPhase] {
		case PhaseStartup:
			startup = true
		case PhaseShutdown:
			shutdown = true
		}
	}

	return startup, shutdown
}

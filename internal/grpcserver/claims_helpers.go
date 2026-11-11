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

package grpcserver

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/claim"
)

// newClaim builds and creates the claim of one request, and returns it. The attribution - which RPC,
// which delta, which group, which nodes - is the point of the object, and it is stamped here, by the
// only writer of a claim's spec in the provider path.
func (s *HomeClusterProviderServer) newClaim(ctx context.Context, slot *infrav2alpha1.Node, group *infrav2alpha1.Group,
	req infrav2alpha1.ClaimRequest, requestedBy string,
) (*infrav2alpha1.NodeClaim, error) {
	nodeClaim, err := claim.New(slot, group, req, requestedBy, s.Scheme)
	if err != nil {
		return nil, err
	}

	if err := s.Client.Create(ctx, nodeClaim); err != nil {
		return nil, err
	}

	return nodeClaim, nil
}

// groupOf reads the Group CR of a node group id, and tolerates its absence: a group the provider
// cannot read has no floor and no maxNodeProvisionTime, and the slot lookups surface the real error.
func (s *HomeClusterProviderServer) groupOf(ctx context.Context, groupName string) *infrav2alpha1.Group {
	group := &infrav2alpha1.Group{}
	if err := s.Client.Get(ctx, client.ObjectKey{Name: groupName, Namespace: namespaceConfig.Get()}, group); err != nil {
		log.Log.WithName("grpc-server").V(1).Info("Group CR not readable", "group", groupName)

		return nil
	}

	return group
}


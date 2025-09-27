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

package groupstore

import (
	"fmt"
	"log"
	"sync"

	v1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/v1alpha1"
)

// Constants for health status values
const (
	statusUnknown  = "unknown"
	statusHealthy  = "healthy"
	statusOffline  = "offline"
	groupStatusKey = "_group" // Key for storing group-level health status
)

// GroupStore provides a thread-safe storage for Group resources using sync.Map
type GroupStore struct {
	store          sync.Map // Stores Group resources by name
	nodeStore      sync.Map // Stores Node resources by name
	healthcheckMap sync.Map // Stores map[string]map[string]string for group->node->status
	nodeToGroupMap sync.Map // Stores node-to-group mapping: nodeName -> groupName
}

// NewGroupStore creates a new GroupStore instance
func NewGroupStore() *GroupStore {
	return &GroupStore{
		store:          sync.Map{},
		nodeStore:      sync.Map{},
		healthcheckMap: sync.Map{},
		nodeToGroupMap: sync.Map{},
	}
}

// AddOrUpdate adds or updates a Group resource in the store
// The key is generated from the Group's metadata.Name
func (s *GroupStore) AddOrUpdate(group *v1alpha1.Group) error {
	if group == nil {
		return fmt.Errorf("group cannot be nil")
	}

	key := group.Name
	if key == "" {
		return fmt.Errorf("group name cannot be empty")
	}

	s.store.Store(key, group)
	log.Printf("GroupStore: Added/updated group %q", key)
	return nil
}

// AddOrUpdateNode adds or updates a Node resource in the store
// The key is generated from the Node's metadata.Name
func (s *GroupStore) AddOrUpdateNode(node *v1alpha1.Node) error {
	if node == nil {
		return fmt.Errorf("node cannot be nil")
	}

	key := node.Name
	if key == "" {
		return fmt.Errorf("node name cannot be empty")
	}

	s.nodeStore.Store(key, node)
	log.Printf("GroupStore: Added/updated node %q", key)
	return nil
}

// Get retrieves a Group resource by key (name)
func (s *GroupStore) Get(key string) (*v1alpha1.Group, error) {
	if key == "" {
		return nil, fmt.Errorf("key cannot be empty")
	}

	value, ok := s.store.Load(key)
	if !ok {
		return nil, fmt.Errorf("group %q not found", key)
	}

	group, ok := value.(*v1alpha1.Group)
	if !ok {
		return nil, fmt.Errorf("invalid type assertion for group %q", key)
	}

	return group, nil
}

// GetNode retrieves a Node resource by key (name)
func (s *GroupStore) GetNode(key string) (*v1alpha1.Node, error) {
	if key == "" {
		return nil, fmt.Errorf("key cannot be empty")
	}

	value, ok := s.nodeStore.Load(key)
	if !ok {
		return nil, fmt.Errorf("node %q not found", key)
	}

	node, ok := value.(*v1alpha1.Node)
	if !ok {
		return nil, fmt.Errorf("invalid type assertion for node %q", key)
	}

	return node, nil
}

// List returns all Group resources in the store
func (s *GroupStore) List() ([]*v1alpha1.Group, error) {
	var groups []*v1alpha1.Group

	s.store.Range(func(key, value interface{}) bool {
		group, ok := value.(*v1alpha1.Group)
		if !ok {
			log.Printf("GroupStore: Invalid type assertion for key %v", key)
			return true
		}
		groups = append(groups, group)
		return true
	})

	return groups, nil
}

// ListNode returns all Node resources in the store
func (s *GroupStore) ListNode() ([]*v1alpha1.Node, error) {
	var nodes []*v1alpha1.Node

	s.nodeStore.Range(func(key, value interface{}) bool {
		node, ok := value.(*v1alpha1.Node)
		if !ok {
			log.Printf("GroupStore: Invalid type assertion for key %v", key)
			return true
		}
		nodes = append(nodes, node)
		return true
	})

	return nodes, nil
}

// Remove removes a Group resource by key (name)
func (s *GroupStore) Remove(key string) error {
	if key == "" {
		return fmt.Errorf("key cannot be empty")
	}

	_, ok := s.store.Load(key)
	if !ok {
		return fmt.Errorf("group %q not found", key)
	}

	// Clean up node-to-group mappings for nodes in this group
	if healthData, exists := s.healthcheckMap.Load(key); exists {
		if nodeHealth, ok := healthData.(map[string]string); ok {
			// Remove all nodes from this group from the node-to-group mapping
			for nodeName := range nodeHealth {
				if nodeName != groupStatusKey { // Skip the group-level status key
					s.nodeToGroupMap.Delete(nodeName)
				}
			}
		}
	}

	s.store.Delete(key)
	s.healthcheckMap.Delete(key)
	log.Printf("GroupStore: Removed group %q", key)
	return nil
}

// RemoveNode removes a Node resource by key (name)
func (s *GroupStore) RemoveNode(key string) error {
	if key == "" {
		return fmt.Errorf("key cannot be empty")
	}

	_, ok := s.nodeStore.Load(key)
	if !ok {
		return fmt.Errorf("node %q not found", key)
	}

	s.nodeStore.Delete(key)
	log.Printf("GroupStore: Removed node %q", key)
	return nil
}

// Count returns the number of Group resources in the store
func (s *GroupStore) Count() int {
	count := 0
	s.store.Range(func(key, value interface{}) bool {
		count++
		return true
	})
	return count
}

// GetOrCreate retrieves a Group by key, or creates a new one if it doesn't exist
// This method is useful for controllers that need to ensure a Group exists
func (s *GroupStore) GetOrCreate(key string, creator func() *v1alpha1.Group) (*v1alpha1.Group, error) {
	if key == "" {
		return nil, fmt.Errorf("key cannot be empty")
	}

	value, ok := s.store.Load(key)
	if ok {
		group, ok := value.(*v1alpha1.Group)
		if !ok {
			return nil, fmt.Errorf("invalid type assertion for group %q", key)
		}
		return group, nil
	}

	// Create new group if provided
	if creator != nil {
		newGroup := creator()
		if newGroup == nil {
			return nil, fmt.Errorf("creator function returned nil group")
		}
		s.store.Store(key, newGroup)
		log.Printf("GroupStore: Created new group %q", key)
		return newGroup, nil
	}

	return nil, fmt.Errorf("group %q not found and no creator provided", key)
}

// Exists checks if a Group with the given key exists in the store
func (s *GroupStore) Exists(key string) bool {
	if key == "" {
		return false
	}

	_, ok := s.store.Load(key)
	return ok
}

// NodeExists checks if a Node with the given key exists in the store
func (s *GroupStore) NodeExists(key string) bool {
	if key == "" {
		return false
	}

	_, ok := s.nodeStore.Load(key)
	return ok
}

// SetHealthcheckStatus sets the healthcheck status for a group (backward compatibility)
// This function now sets the overall group health status based on all nodes
func (s *GroupStore) SetHealthcheckStatus(groupName string, status string) {
	if groupName == "" {
		return
	}

	// For backward compatibility, store group-level status
	// In the new structure, this represents the overall group health
	groupHealth := make(map[string]string)
	groupHealth[groupStatusKey] = status
	s.healthcheckMap.Store(groupName, groupHealth)
	log.Printf("GroupStore: Set healthcheck status for group %q to %q", groupName, status)
}

// UpdateGroupHealth updates the health status in the stored Group object
func (s *GroupStore) UpdateGroupHealth(groupName string, health string) error {
	if groupName == "" {
		return fmt.Errorf("group name cannot be empty")
	}

	value, ok := s.store.Load(groupName)
	if !ok {
		return fmt.Errorf("group %q not found", groupName)
	}

	group, ok := value.(*v1alpha1.Group)
	if !ok {
		return fmt.Errorf("invalid type assertion for group %q", groupName)
	}

	// Update the health status
	group.Status.Health = health
	s.store.Store(groupName, group)
	log.Printf("GroupStore: Updated health status for group %q to %q", groupName, health)
	return nil
}

// GetHealthcheckStatus retrieves the healthcheck status for a group (backward compatibility)
// Returns the overall group health status
func (s *GroupStore) GetHealthcheckStatus(groupName string) (string, bool) {
	if groupName == "" {
		log.Printf("GroupStore: GetHealthcheckStatus called with empty group name")
		return "", false
	}

	log.Printf("GroupStore: Getting healthcheck status for group %q", groupName)

	value, ok := s.healthcheckMap.Load(groupName)
	if !ok {
		log.Printf("GroupStore: No healthcheck status found for group %q", groupName)
		return "", false
	}

	// Handle both old (string) and new (map[string]string) formats for backward compatibility
	switch v := value.(type) {
	case string:
		// Old format - direct string status
		log.Printf("GroupStore: Found healthcheck status %q for group %q", v, groupName)
		return v, true
	case map[string]string:
		// New format - map of node statuses
		if groupStatus, exists := v[groupStatusKey]; exists {
			log.Printf("GroupStore: Found group healthcheck status %q for group %q", groupStatus, groupName)
			return groupStatus, true
		}
		// If no _group key exists, return empty string
		log.Printf("GroupStore: No group-level healthcheck status found for group %q", groupName)
		return "", false
	default:
		log.Printf("GroupStore: Invalid type assertion for healthcheck status of group %q", groupName)
		return "", false
	}
}

// SetNodeHealthcheckStatus sets the healthcheck status for a specific node within a group
func (s *GroupStore) SetNodeHealthcheckStatus(groupName, nodeName, status string) {
	if groupName == "" || nodeName == "" {
		return
	}

	// Update node-to-group mapping
	s.nodeToGroupMap.Store(nodeName, groupName)

	// Load existing health data or create new map
	var nodeHealth map[string]string
	value, exists := s.healthcheckMap.Load(groupName)
	if exists {
		if existingMap, ok := value.(map[string]string); ok {
			nodeHealth = existingMap
		} else {
			// Convert old format to new format
			nodeHealth = make(map[string]string)
			if oldStatus, ok := value.(string); ok {
				nodeHealth[groupStatusKey] = oldStatus
			}
		}
	} else {
		nodeHealth = make(map[string]string)
	}

	// Set the node status
	nodeHealth[nodeName] = status

	// Update the overall group health based on all nodes
	nodeHealth[groupStatusKey] = s.calculateGroupHealth(nodeHealth)

	s.healthcheckMap.Store(groupName, nodeHealth)
	log.Printf("GroupStore: Set healthcheck status for node %q in group %q to %q", nodeName, groupName, status)
}

// GetNodeHealthcheckStatus retrieves the healthcheck status for a specific node within a group
// Returns the status and a boolean indicating if the status was found
func (s *GroupStore) GetNodeHealthcheckStatus(groupName, nodeName string) (string, bool) {
	if groupName == "" || nodeName == "" {
		log.Printf("GroupStore: GetNodeHealthcheckStatus called with empty group or node name")
		return "", false
	}

	log.Printf("GroupStore: Getting healthcheck status for node %q in group %q", nodeName, groupName)

	value, ok := s.healthcheckMap.Load(groupName)
	if !ok {
		log.Printf("GroupStore: No healthcheck status found for group %q", groupName)
		return "", false
	}

	// Handle both old (string) and new (map[string]string) formats
	switch v := value.(type) {
	case map[string]string:
		if nodeStatus, exists := v[nodeName]; exists {
			log.Printf("GroupStore: Found healthcheck status %q for node %q in group %q", nodeStatus, nodeName, groupName)
			return nodeStatus, true
		}
		log.Printf("GroupStore: No healthcheck status found for node %q in group %q", nodeName, groupName)
		return "", false
	default:
		log.Printf("GroupStore: Invalid data structure for healthcheck status of group %q", groupName)
		return "", false
	}
}

// calculateGroupHealth calculates the overall group health based on individual node healths
func (s *GroupStore) calculateGroupHealth(nodeHealth map[string]string) string {
	if len(nodeHealth) == 0 {
		return statusUnknown
	}

	healthyCount := 0
	offlineCount := 0
	totalNodes := 0

	for node, status := range nodeHealth {
		if node == groupStatusKey {
			continue // Skip the group-level status
		}
		totalNodes++
		switch status {
		case statusHealthy:
			healthyCount++
		case statusOffline:
			offlineCount++
		}
	}

	if totalNodes == 0 {
		return statusUnknown
	}

	// If all nodes are healthy, group is healthy
	if healthyCount == totalNodes {
		return statusHealthy
	}

	// If any node is offline, group is offline
	if offlineCount > 0 {
		return statusOffline
	}

	// Otherwise, group is unknown
	return statusUnknown
}

// GetGroupForNode retrieves the group name that a node belongs to
func (s *GroupStore) GetGroupForNode(nodeName string) (string, bool) {
	if nodeName == "" {
		return "", false
	}

	value, ok := s.nodeToGroupMap.Load(nodeName)
	if !ok {
		return "", false
	}

	groupName, ok := value.(string)
	if !ok {
		log.Printf("GroupStore: Invalid type assertion for node-to-group mapping of node %q", nodeName)
		return "", false
	}

	return groupName, true
}

// GetNodesForGroup retrieves all node names that belong to a specific group
func (s *GroupStore) GetNodesForGroup(groupName string) []string {
	if groupName == "" {
		return []string{}
	}

	var nodes []string

	// Get the health data for the group
	value, ok := s.healthcheckMap.Load(groupName)
	if !ok {
		return nodes
	}

	nodeHealth, ok := value.(map[string]string)
	if !ok {
		log.Printf("GroupStore: Invalid type assertion for health data of group %q", groupName)
		return nodes
	}

	// Extract all node names (excluding the _group key)
	for nodeName := range nodeHealth {
		if nodeName != groupStatusKey {
			nodes = append(nodes, nodeName)
		}
	}

	return nodes
}

// GetAllNodeToGroupMappings returns a map of all node-to-group relationships
func (s *GroupStore) GetAllNodeToGroupMappings() map[string]string {
	mappings := make(map[string]string)

	s.nodeToGroupMap.Range(func(key, value interface{}) bool {
		nodeName, ok1 := key.(string)
		groupName, ok2 := value.(string)
		if ok1 && ok2 {
			mappings[nodeName] = groupName
		}
		return true
	})

	return mappings
}

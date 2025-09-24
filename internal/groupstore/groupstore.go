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

// GroupStore provides a thread-safe storage for Group resources using sync.Map
type GroupStore struct {
	store          sync.Map
	healthcheckMap sync.Map
}

// NewGroupStore creates a new GroupStore instance
func NewGroupStore() *GroupStore {
	return &GroupStore{
		store:          sync.Map{},
		healthcheckMap: sync.Map{},
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

// Remove removes a Group resource by key (name)
func (s *GroupStore) Remove(key string) error {
	if key == "" {
		return fmt.Errorf("key cannot be empty")
	}

	_, ok := s.store.Load(key)
	if !ok {
		return fmt.Errorf("group %q not found", key)
	}

	s.store.Delete(key)
	log.Printf("GroupStore: Removed group %q", key)
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

// SetHealthcheckStatus sets the healthcheck status for a group
func (s *GroupStore) SetHealthcheckStatus(groupName string, status string) {
	if groupName == "" {
		return
	}
	s.healthcheckMap.Store(groupName, status)
	log.Printf("GroupStore: Set healthcheck status for group %q to %q", groupName, status)
}

// GetHealthcheckStatus retrieves the healthcheck status for a group
// Returns the status and a boolean indicating if the status was found
func (s *GroupStore) GetHealthcheckStatus(groupName string) (string, bool) {
	if groupName == "" {
		return "", false
	}
	value, ok := s.healthcheckMap.Load(groupName)
	if !ok {
		return "", false
	}
	status, ok := value.(string)
	if !ok {
		log.Printf("GroupStore: Invalid type assertion for healthcheck status of group %q", groupName)
		return "", false
	}
	return status, true
}

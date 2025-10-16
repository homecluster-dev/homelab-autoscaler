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

package mocks

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MockClient implements the controller-runtime client.Client interface for testing
type MockClient struct {
	mu      sync.RWMutex
	objects map[string]runtime.Object // key format: "namespace/name" or "name" for cluster-scoped
	scheme  *runtime.Scheme
}

// NewMockClient creates a new mock client with the provided scheme
func NewMockClient(scheme *runtime.Scheme) *MockClient {
	return &MockClient{
		objects: make(map[string]runtime.Object),
		scheme:  scheme,
	}
}

// objectKey generates a consistent key for storing objects
func (m *MockClient) objectKey(obj runtime.Object) (string, error) {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return "", err
	}

	namespace := accessor.GetNamespace()
	name := accessor.GetName()

	if namespace == "" {
		return name, nil
	}
	return fmt.Sprintf("%s/%s", namespace, name), nil
}

// objectKeyFromNamespacedName generates a key from client.ObjectKey
func (m *MockClient) objectKeyFromNamespacedName(key client.ObjectKey) string {
	if key.Namespace == "" {
		return key.Name
	}
	return fmt.Sprintf("%s/%s", key.Namespace, key.Name)
}

// Get retrieves an object by key
func (m *MockClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	objKey := m.objectKeyFromNamespacedName(key)
	stored, exists := m.objects[objKey]
	if !exists {
		// Return appropriate NotFound error based on object type
		gvk := obj.GetObjectKind().GroupVersionKind()
		if gvk.Empty() {
			// Try to infer GVK from the object type
			gvks, _, err := m.scheme.ObjectKinds(obj)
			if err == nil && len(gvks) > 0 {
				gvk = gvks[0]
			}
		}

		return errors.NewNotFound(schema.GroupResource{
			Group:    gvk.Group,
			Resource: strings.ToLower(gvk.Kind) + "s",
		}, key.Name)
	}

	// Deep copy the stored object to avoid mutations
	storedCopy := stored.DeepCopyObject()

	// Use reflection to copy the stored object to the target object
	objValue := reflect.ValueOf(obj)
	if objValue.Kind() == reflect.Ptr {
		objValue = objValue.Elem()
	}

	storedValue := reflect.ValueOf(storedCopy)
	if storedValue.Kind() == reflect.Ptr {
		storedValue = storedValue.Elem()
	}

	objValue.Set(storedValue)
	return nil
}

// List retrieves a list of objects
func (m *MockClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	listOpts := &client.ListOptions{}
	for _, opt := range opts {
		opt.ApplyToList(listOpts)
	}

	// Get the item type from the list
	listValue := reflect.ValueOf(list)
	if listValue.Kind() == reflect.Ptr {
		listValue = listValue.Elem()
	}

	itemsField := listValue.FieldByName("Items")
	if !itemsField.IsValid() {
		return fmt.Errorf("list object does not have Items field")
	}

	itemType := itemsField.Type().Elem()
	var matchingObjects []runtime.Object

	// Filter objects by type and namespace
	for key, obj := range m.objects {
		objType := reflect.TypeOf(obj)
		if objType.Kind() == reflect.Ptr {
			objType = objType.Elem()
		}

		// Check if object type matches
		if objType != itemType {
			continue
		}

		// Check namespace filter
		if listOpts.Namespace != "" {
			parts := strings.Split(key, "/")
			if len(parts) != 2 || parts[0] != listOpts.Namespace {
				continue
			}
		}

		// Check label selector
		if listOpts.LabelSelector != nil {
			accessor, err := meta.Accessor(obj)
			if err != nil {
				continue
			}

			objLabels := labels.Set(accessor.GetLabels())
			if !listOpts.LabelSelector.Matches(objLabels) {
				continue
			}
		}

		matchingObjects = append(matchingObjects, obj.DeepCopyObject())
	}

	// Create slice of matching objects
	itemsSlice := reflect.MakeSlice(itemsField.Type(), len(matchingObjects), len(matchingObjects))
	for i, obj := range matchingObjects {
		objValue := reflect.ValueOf(obj)
		if objValue.Kind() == reflect.Ptr {
			objValue = objValue.Elem()
		}
		itemsSlice.Index(i).Set(objValue)
	}

	itemsField.Set(itemsSlice)
	return nil
}

// Create creates a new object
func (m *MockClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key, err := m.objectKey(obj)
	if err != nil {
		return err
	}

	if _, exists := m.objects[key]; exists {
		accessor, _ := meta.Accessor(obj)
		gvk := obj.GetObjectKind().GroupVersionKind()
		return errors.NewAlreadyExists(schema.GroupResource{
			Group:    gvk.Group,
			Resource: strings.ToLower(gvk.Kind) + "s",
		}, accessor.GetName())
	}

	// Set creation timestamp and UID if not set
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return err
	}

	creationTime := accessor.GetCreationTimestamp()
	if creationTime.IsZero() {
		now := metav1.Now()
		accessor.SetCreationTimestamp(now)
	}

	if accessor.GetUID() == "" {
		accessor.SetUID(types.UID(fmt.Sprintf("mock-uid-%s", key)))
	}

	if accessor.GetResourceVersion() == "" {
		accessor.SetResourceVersion("1")
	}

	m.objects[key] = obj.DeepCopyObject()
	return nil
}

// Update updates an existing object
func (m *MockClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key, err := m.objectKey(obj)
	if err != nil {
		return err
	}

	stored, exists := m.objects[key]
	if !exists {
		accessor, _ := meta.Accessor(obj)
		gvk := obj.GetObjectKind().GroupVersionKind()
		return errors.NewNotFound(schema.GroupResource{
			Group:    gvk.Group,
			Resource: strings.ToLower(gvk.Kind) + "s",
		}, accessor.GetName())
	}

	// Increment resource version
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return err
	}

	storedAccessor, err := meta.Accessor(stored)
	if err != nil {
		return err
	}

	// Copy metadata from stored object
	accessor.SetCreationTimestamp(storedAccessor.GetCreationTimestamp())
	accessor.SetUID(storedAccessor.GetUID())

	// Increment resource version
	currentVersion := storedAccessor.GetResourceVersion()
	if currentVersion == "" {
		currentVersion = "1"
	}
	accessor.SetResourceVersion(fmt.Sprintf("%s-updated", currentVersion))

	m.objects[key] = obj.DeepCopyObject()
	return nil
}

// Patch patches an existing object
func (m *MockClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	// For simplicity, treat patch as update
	return m.Update(ctx, obj, &client.UpdateOptions{})
}

// Delete deletes an object
func (m *MockClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key, err := m.objectKey(obj)
	if err != nil {
		return err
	}

	if _, exists := m.objects[key]; !exists {
		accessor, _ := meta.Accessor(obj)
		gvk := obj.GetObjectKind().GroupVersionKind()
		return errors.NewNotFound(schema.GroupResource{
			Group:    gvk.Group,
			Resource: strings.ToLower(gvk.Kind) + "s",
		}, accessor.GetName())
	}

	delete(m.objects, key)
	return nil
}

// DeleteAllOf deletes all objects matching the given options
func (m *MockClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	deleteOpts := &client.DeleteAllOfOptions{}
	for _, opt := range opts {
		opt.ApplyToDeleteAllOf(deleteOpts)
	}

	objType := reflect.TypeOf(obj)
	if objType.Kind() == reflect.Ptr {
		objType = objType.Elem()
	}

	var keysToDelete []string
	for key, stored := range m.objects {
		storedType := reflect.TypeOf(stored)
		if storedType.Kind() == reflect.Ptr {
			storedType = storedType.Elem()
		}

		if storedType != objType {
			continue
		}

		// Check namespace filter
		if deleteOpts.Namespace != "" {
			parts := strings.Split(key, "/")
			if len(parts) != 2 || parts[0] != deleteOpts.Namespace {
				continue
			}
		}

		// Check label selector
		if deleteOpts.LabelSelector != nil {
			accessor, err := meta.Accessor(stored)
			if err != nil {
				continue
			}

			objLabels := labels.Set(accessor.GetLabels())
			if !deleteOpts.LabelSelector.Matches(objLabels) {
				continue
			}
		}

		keysToDelete = append(keysToDelete, key)
	}

	for _, key := range keysToDelete {
		delete(m.objects, key)
	}

	return nil
}

// Status returns a status writer for the mock client
func (m *MockClient) Status() client.StatusWriter {
	return &mockStatusWriter{client: m}
}

// Scheme returns the scheme used by the mock client
func (m *MockClient) Scheme() *runtime.Scheme {
	return m.scheme
}

// RESTMapper returns a RESTMapper (not implemented for mock)
func (m *MockClient) RESTMapper() meta.RESTMapper {
	return nil
}

// GroupVersionKindFor returns the GroupVersionKind for the given object
func (m *MockClient) GroupVersionKindFor(obj runtime.Object) (schema.GroupVersionKind, error) {
	gvks, _, err := m.scheme.ObjectKinds(obj)
	if err != nil {
		return schema.GroupVersionKind{}, err
	}
	if len(gvks) == 0 {
		return schema.GroupVersionKind{}, fmt.Errorf("no GroupVersionKind found for object")
	}
	return gvks[0], nil
}

// IsObjectNamespaced returns true if the object is namespaced
func (m *MockClient) IsObjectNamespaced(obj runtime.Object) (bool, error) {
	gvk, err := m.GroupVersionKindFor(obj)
	if err != nil {
		return false, err
	}

	// Simple heuristic: core/v1 Nodes are cluster-scoped, everything else is namespaced
	if gvk.Group == "" && gvk.Version == "v1" && gvk.Kind == "Node" {
		return false, nil
	}
	return true, nil
}

// AddObject adds an object to the mock client's storage
func (m *MockClient) AddObject(obj runtime.Object) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key, err := m.objectKey(obj)
	if err != nil {
		return err
	}

	m.objects[key] = obj.DeepCopyObject()
	return nil
}

// RemoveObject removes an object from the mock client's storage
func (m *MockClient) RemoveObject(obj runtime.Object) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key, err := m.objectKey(obj)
	if err != nil {
		return err
	}

	delete(m.objects, key)
	return nil
}

// Clear removes all objects from the mock client's storage
func (m *MockClient) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.objects = make(map[string]runtime.Object)
}

// mockStatusWriter implements client.StatusWriter for the mock client
type mockStatusWriter struct {
	client *MockClient
}

// Update updates the status of an object
func (w *mockStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	return w.client.Update(ctx, obj)
}

// Patch patches the status of an object
func (w *mockStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	return w.client.Patch(ctx, obj, patch)
}

// Create creates the status of an object (not typically used)
func (w *mockStatusWriter) Create(ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	return w.client.Create(ctx, obj)
}

// Apply applies the given object to the mock client (server-side apply)
func (m *MockClient) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	// For simplicity in mocking, we don't fully implement server-side apply
	// This is a placeholder that would need proper implementation for real usage
	return fmt.Errorf("Apply method not fully implemented in mock client")
}

// SubResource returns a SubResourceClient for the given subresource
func (m *MockClient) SubResource(subresource string) client.SubResourceClient {
	return &mockSubResourceClient{client: m, subresource: subresource}
}

// mockSubResourceClient implements client.SubResourceClient for the mock client
type mockSubResourceClient struct {
	client      *MockClient
	subresource string
}

// Get retrieves a subresource
func (s *mockSubResourceClient) Get(ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceGetOption) error {
	// For simplicity, delegate to main client Get
	key := client.ObjectKeyFromObject(obj)
	return s.client.Get(ctx, key, obj)
}

// Create creates a subresource
func (s *mockSubResourceClient) Create(ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	// For simplicity, delegate to main client Create
	return s.client.Create(ctx, obj)
}

// Update updates a subresource
func (s *mockSubResourceClient) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	// For simplicity, delegate to main client Update
	return s.client.Update(ctx, obj)
}

// Patch patches a subresource
func (s *mockSubResourceClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	// For simplicity, delegate to main client Patch
	return s.client.Patch(ctx, obj, patch)
}

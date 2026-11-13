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

// Package v2alpha1 contains API Schema definitions for the infra.homecluster.dev v2alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=infra.homecluster.dev
package v2alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "infra.homecluster.dev", Version: "v2alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme. The *_types.go
	// files of this package call Register on it from their init.
	//
	// It is a local builder, not controller-runtime's: that one is deprecated as of
	// v0.25, because an api package should depend on the standard library, apimachinery and
	// other api packages only. This one is the same two methods, over apimachinery.
	SchemeBuilder = &typeBuilder{groupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// typeBuilder collects the types declared by this package and writes them into a scheme.
type typeBuilder struct {
	groupVersion schema.GroupVersion
	types        []runtime.Object
}

// Register records objects - a type and its list type, as the init of each *_types.go file
// passes them. It mutates the builder and returns it, so calls can chain.
func (b *typeBuilder) Register(objects ...runtime.Object) *typeBuilder {
	b.types = append(b.types, objects...)
	return b
}

// AddToScheme registers the recorded types with their group-version, and the metav1 kinds a
// scheme needs to list them.
func (b *typeBuilder) AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(b.groupVersion, b.types...)
	metav1.AddToGroupVersion(s, b.groupVersion)
	return nil
}

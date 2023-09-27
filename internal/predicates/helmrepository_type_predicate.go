/*
Copyright 2022 The Flux authors

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

package predicates

import (
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
)

// helmRepositoryTypeFilter filters events for a given HelmRepository type.
// It returns true if the event is for a HelmRepository of the given type.
func helmRepositoryTypeFilter(repositoryType string, o client.Object) bool {
	if o == nil {
		return false
	}

	// return true if the object is a HelmRepository
	// and the type is the same as the one we are looking for.
	hr, ok := o.(*sourcev1.HelmRepository)
	if !ok {
		return false
	}

	return hr.Spec.Type == repositoryType
}

// HelmRepositoryTypePredicate is a predicate that filters events for a given HelmRepository type.
type HelmRepositoryTypePredicate struct {
	RepositoryType string
	predicate.Funcs
}

// Create returns true if the Create event is for a HelmRepository of the given type.
func (h HelmRepositoryTypePredicate) Create(e event.CreateEvent) bool {
	return helmRepositoryTypeFilter(h.RepositoryType, e.Object)
}

// Update returns true if the Update event is for a HelmRepository of the given type.
func (h HelmRepositoryTypePredicate) Update(e event.UpdateEvent) bool {
	if e.ObjectNew == nil {
		return false
	}

	// check if the new object is a HelmRepository
	newObj, ok := e.ObjectNew.(*sourcev1.HelmRepository)
	if !ok {
		return false
	}

	isOfRepositoryType := newObj.Spec.Type == h.RepositoryType
	return isOfRepositoryType
}

// Delete returns true if the Delete event is for a HelmRepository of the given type.
func (h HelmRepositoryTypePredicate) Delete(e event.DeleteEvent) bool {
	return helmRepositoryTypeFilter(h.RepositoryType, e.Object)
}

// Generic returns true if the Generic event is for a HelmRepository of the given type.
func (h HelmRepositoryTypePredicate) Generic(e event.GenericEvent) bool {
	return helmRepositoryTypeFilter(h.RepositoryType, e.Object)
}

// HelmRepositoryOCIMigrationPredicate implements predicate functions to allow
// events for HelmRepository OCI that need migration to static object.
type HelmRepositoryOCIMigrationPredicate struct {
	predicate.Funcs
}

// Create allows events for objects that need migration to static object.
func (HelmRepositoryOCIMigrationPredicate) Create(e event.CreateEvent) bool {
	return HelmRepositoryOCIRequireMigration(e.Object)
}

// Update allows events for objects that need migration to static object.
func (HelmRepositoryOCIMigrationPredicate) Update(e event.UpdateEvent) bool {
	return HelmRepositoryOCIRequireMigration(e.ObjectNew)
}

// Delete allows events for objects that need migration to static object.
func (HelmRepositoryOCIMigrationPredicate) Delete(e event.DeleteEvent) bool {
	return HelmRepositoryOCIRequireMigration(e.Object)
}

// HelmRepositoryOCIRequireMigration returns if a given HelmRepository of type
// OCI requires migration to static object.
func HelmRepositoryOCIRequireMigration(o client.Object) bool {
	if o == nil {
		return false
	}

	hr, ok := o.(*sourcev1.HelmRepository)
	if !ok {
		return false
	}

	if hr.Spec.Type != sourcev1.HelmRepositoryTypeOCI {
		return false
	}

	if controllerutil.ContainsFinalizer(hr, sourcev1.SourceFinalizer) || !hasEmptyHelmRepositoryStatus(hr) {
		return true
	}

	return false
}

// hasEmptyHelmRepositoryStatus checks if the status of a HelmRepository is
// empty.
func hasEmptyHelmRepositoryStatus(obj *sourcev1.HelmRepository) bool {
	if obj.Status.ObservedGeneration == 0 &&
		obj.Status.Conditions == nil &&
		obj.Status.URL == "" &&
		obj.Status.Artifact == nil &&
		obj.Status.ReconcileRequestStatus.LastHandledReconcileAt == "" {
		return true
	}
	return false
}

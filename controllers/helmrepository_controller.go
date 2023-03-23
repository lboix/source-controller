/*
Copyright 2020 The Flux authors

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
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/docker/go-units"
	"github.com/opencontainers/go-digest"
	helmgetter "helm.sh/helm/v3/pkg/getter"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kuberecorder "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/ratelimiter"

	eventv1 "github.com/fluxcd/pkg/apis/event/v1beta1"
	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"
	helper "github.com/fluxcd/pkg/runtime/controller"
	"github.com/fluxcd/pkg/runtime/patch"
	"github.com/fluxcd/pkg/runtime/predicates"
	rreconcile "github.com/fluxcd/pkg/runtime/reconcile"

	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/fluxcd/source-controller/internal/cache"
	intdigest "github.com/fluxcd/source-controller/internal/digest"
	serror "github.com/fluxcd/source-controller/internal/error"
	"github.com/fluxcd/source-controller/internal/helm/getter"
	"github.com/fluxcd/source-controller/internal/helm/repository"
	"github.com/fluxcd/source-controller/internal/jitter"
	intpredicates "github.com/fluxcd/source-controller/internal/predicates"
	sreconcile "github.com/fluxcd/source-controller/internal/reconcile"
	"github.com/fluxcd/source-controller/internal/reconcile/summarize"
)

// helmRepositoryReadyCondition contains the information required to summarize a
// v1beta2.HelmRepository Ready Condition.
var helmRepositoryReadyCondition = summarize.Conditions{
	Target: meta.ReadyCondition,
	Owned: []string{
		sourcev1.StorageOperationFailedCondition,
		sourcev1.FetchFailedCondition,
		sourcev1.ArtifactOutdatedCondition,
		sourcev1.ArtifactInStorageCondition,
		meta.ReadyCondition,
		meta.ReconcilingCondition,
		meta.StalledCondition,
	},
	Summarize: []string{
		sourcev1.StorageOperationFailedCondition,
		sourcev1.FetchFailedCondition,
		sourcev1.ArtifactOutdatedCondition,
		sourcev1.ArtifactInStorageCondition,
		meta.StalledCondition,
		meta.ReconcilingCondition,
	},
	NegativePolarity: []string{
		sourcev1.StorageOperationFailedCondition,
		sourcev1.FetchFailedCondition,
		sourcev1.ArtifactOutdatedCondition,
		meta.StalledCondition,
		meta.ReconcilingCondition,
	},
}

// helmRepositoryFailConditions contains the conditions that represent a
// failure.
var helmRepositoryFailConditions = []string{
	sourcev1.FetchFailedCondition,
	sourcev1.StorageOperationFailedCondition,
}

// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmrepositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmrepositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmrepositories/finalizers,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// HelmRepositoryReconciler reconciles a v1beta2.HelmRepository object.
type HelmRepositoryReconciler struct {
	client.Client
	kuberecorder.EventRecorder
	helper.Metrics

	Getters        helmgetter.Providers
	Storage        *Storage
	ControllerName string

	Cache *cache.Cache
	TTL   time.Duration
	*cache.CacheRecorder

	patchOptions       []patch.Option
	requeueAfterJitter jitter.Duration
}

type HelmRepositoryReconcilerOptions struct {
	MaxConcurrentReconciles int
	RateLimiter             ratelimiter.RateLimiter
	RequeueAfterJitter      jitter.Duration
}

// helmRepositoryReconcileFunc is the function type for all the
// v1beta2.HelmRepository (sub)reconcile functions. The type implementations
// are grouped and executed serially to perform the complete reconcile of the
// object.
type helmRepositoryReconcileFunc func(ctx context.Context, sp *patch.SerialPatcher, obj *sourcev1.HelmRepository, artifact *sourcev1.Artifact, repo *repository.ChartRepository) (sreconcile.Result, error)

func (r *HelmRepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.SetupWithManagerAndOptions(mgr, HelmRepositoryReconcilerOptions{})
}

func (r *HelmRepositoryReconciler) SetupWithManagerAndOptions(mgr ctrl.Manager, opts HelmRepositoryReconcilerOptions) error {
	r.patchOptions = getPatchOptions(helmRepositoryReadyCondition.Owned, r.ControllerName)

	r.requeueAfterJitter = opts.RequeueAfterJitter
	if r.requeueAfterJitter == nil {
		r.requeueAfterJitter = jitter.NoJitter
	}

	recoverPanic := true
	return ctrl.NewControllerManagedBy(mgr).
		For(&sourcev1.HelmRepository{}).
		WithEventFilter(
			predicate.And(
				predicate.Or(
					intpredicates.HelmRepositoryTypePredicate{RepositoryType: sourcev1.HelmRepositoryTypeDefault},
					intpredicates.HelmRepositoryTypePredicate{RepositoryType: ""},
				),
				predicate.Or(predicate.GenerationChangedPredicate{}, predicates.ReconcileRequestedPredicate{}),
			),
		).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: opts.MaxConcurrentReconciles,
			RateLimiter:             opts.RateLimiter,
			RecoverPanic:            &recoverPanic,
		}).
		Complete(r)
}

func (r *HelmRepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	start := time.Now()
	log := ctrl.LoggerFrom(ctx)

	// Fetch the HelmRepository
	obj := &sourcev1.HelmRepository{}
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Record suspended status metric
	r.RecordSuspend(ctx, obj, obj.Spec.Suspend)

	// Initialize the patch helper with the current version of the object.
	serialPatcher := patch.NewSerialPatcher(obj, r.Client)

	// recResult stores the abstracted reconcile result.
	var recResult sreconcile.Result

	// Always attempt to patch the object after each reconciliation.
	// NOTE: The final runtime result and error are set in this block.
	defer func() {
		summarizeHelper := summarize.NewHelper(r.EventRecorder, serialPatcher)
		summarizeOpts := []summarize.Option{
			summarize.WithConditions(helmRepositoryReadyCondition),
			summarize.WithReconcileResult(recResult),
			summarize.WithReconcileError(retErr),
			summarize.WithIgnoreNotFound(),
			summarize.WithProcessors(
				summarize.RecordContextualError,
				summarize.RecordReconcileReq,
			),
			summarize.WithResultBuilder(sreconcile.AlwaysRequeueResultBuilder{
				RequeueAfter: r.requeueAfterJitter(obj.GetRequeueAfter()),
			}),
			summarize.WithPatchFieldOwner(r.ControllerName),
		}
		result, retErr = summarizeHelper.SummarizeAndPatch(ctx, obj, summarizeOpts...)

		// Always record readiness and duration metrics
		r.Metrics.RecordReadiness(ctx, obj)
		r.Metrics.RecordDuration(ctx, obj, start)
	}()

	// Add finalizer first if not exist to avoid the race condition
	// between init and delete
	if !controllerutil.ContainsFinalizer(obj, sourcev1.SourceFinalizer) {
		controllerutil.AddFinalizer(obj, sourcev1.SourceFinalizer)
		recResult = sreconcile.ResultRequeue
		return
	}

	// Examine if the object is under deletion
	// or if a type change has happened
	if !obj.ObjectMeta.DeletionTimestamp.IsZero() || (obj.Spec.Type != "" && obj.Spec.Type != sourcev1.HelmRepositoryTypeDefault) {
		recResult, retErr = r.reconcileDelete(ctx, obj)
		return
	}

	// Return if the object is suspended.
	if obj.Spec.Suspend {
		log.Info("reconciliation is suspended for this object")
		recResult, retErr = sreconcile.ResultEmpty, nil
		return
	}

	// Reconcile actual object
	reconcilers := []helmRepositoryReconcileFunc{
		r.reconcileStorage,
		r.reconcileSource,
		r.reconcileArtifact,
	}
	recResult, retErr = r.reconcile(ctx, serialPatcher, obj, reconcilers)
	return
}

// reconcile iterates through the helmRepositoryReconcileFunc tasks for the
// object. It returns early on the first call that returns
// reconcile.ResultRequeue, or produces an error.
func (r *HelmRepositoryReconciler) reconcile(ctx context.Context, sp *patch.SerialPatcher,
	obj *sourcev1.HelmRepository, reconcilers []helmRepositoryReconcileFunc) (sreconcile.Result, error) {
	oldObj := obj.DeepCopy()

	rreconcile.ProgressiveStatus(false, obj, meta.ProgressingReason, "reconciliation in progress")

	var reconcileAtVal string
	if v, ok := meta.ReconcileAnnotationValue(obj.GetAnnotations()); ok {
		reconcileAtVal = v
	}

	// Persist reconciling if generation differs or reconciliation is requested.
	switch {
	case obj.Generation != obj.Status.ObservedGeneration:
		rreconcile.ProgressiveStatus(false, obj, meta.ProgressingReason,
			"processing object: new generation %d -> %d", obj.Status.ObservedGeneration, obj.Generation)
		if err := sp.Patch(ctx, obj, r.patchOptions...); err != nil {
			return sreconcile.ResultEmpty, err
		}
	case reconcileAtVal != obj.Status.GetLastHandledReconcileRequest():
		if err := sp.Patch(ctx, obj, r.patchOptions...); err != nil {
			return sreconcile.ResultEmpty, err
		}
	}

	var chartRepo repository.ChartRepository
	var artifact sourcev1.Artifact

	// Run the sub-reconcilers and build the result of reconciliation.
	var res sreconcile.Result
	var resErr error
	for _, rec := range reconcilers {
		recResult, err := rec(ctx, sp, obj, &artifact, &chartRepo)
		// Exit immediately on ResultRequeue.
		if recResult == sreconcile.ResultRequeue {
			return sreconcile.ResultRequeue, nil
		}
		// If an error is received, prioritize the returned results because an
		// error also means immediate requeue.
		if err != nil {
			resErr = err
			res = recResult
			break
		}
		// Prioritize requeue request in the result for successful results.
		res = sreconcile.LowestRequeuingResult(res, recResult)
	}

	r.notify(ctx, oldObj, obj, &chartRepo, res, resErr)

	return res, resErr
}

// notify emits notification related to the reconciliation.
func (r *HelmRepositoryReconciler) notify(ctx context.Context, oldObj, newObj *sourcev1.HelmRepository, chartRepo *repository.ChartRepository, res sreconcile.Result, resErr error) {
	// Notify successful reconciliation for new artifact and recovery from any
	// failure.
	if resErr == nil && res == sreconcile.ResultSuccess && newObj.Status.Artifact != nil {
		annotations := map[string]string{
			fmt.Sprintf("%s/%s", sourcev1.GroupVersion.Group, eventv1.MetaRevisionKey): newObj.Status.Artifact.Revision,
			fmt.Sprintf("%s/%s", sourcev1.GroupVersion.Group, eventv1.MetaChecksumKey): newObj.Status.Artifact.Checksum,
		}
		if newObj.Status.Artifact.Digest != "" {
			annotations[sourcev1.GroupVersion.Group+"/"+eventv1.MetaDigestKey] = newObj.Status.Artifact.Digest
		}

		humanReadableSize := "unknown size"
		if size := newObj.Status.Artifact.Size; size != nil {
			humanReadableSize = fmt.Sprintf("size %s", units.HumanSize(float64(*size)))
		}

		var oldChecksum string
		if oldObj.GetArtifact() != nil {
			oldChecksum = oldObj.GetArtifact().Checksum
		}

		message := fmt.Sprintf("stored fetched index of %s from '%s'", humanReadableSize, chartRepo.URL)

		// Notify on new artifact and failure recovery.
		if oldChecksum != newObj.GetArtifact().Checksum {
			r.AnnotatedEventf(newObj, annotations, corev1.EventTypeNormal,
				"NewArtifact", message)
			ctrl.LoggerFrom(ctx).Info(message)
		} else {
			if sreconcile.FailureRecovery(oldObj, newObj, helmRepositoryFailConditions) {
				r.AnnotatedEventf(newObj, annotations, corev1.EventTypeNormal,
					meta.SucceededReason, message)
				ctrl.LoggerFrom(ctx).Info(message)
			}
		}
	}
}

// reconcileStorage ensures the current state of the storage matches the
// desired and previously observed state.
//
// The garbage collection is executed based on the flag configured settings and
// may remove files that are beyond their TTL or the maximum number of files
// to survive a collection cycle.
// If the Artifact in the Status of the object disappeared from the Storage,
// it is removed from the object.
// If the object does not have an Artifact in its Status, a Reconciling
// condition is added.
// The hostname of any URL in the Status of the object are updated, to ensure
// they match the Storage server hostname of current runtime.
func (r *HelmRepositoryReconciler) reconcileStorage(ctx context.Context, sp *patch.SerialPatcher,
	obj *sourcev1.HelmRepository, _ *sourcev1.Artifact, _ *repository.ChartRepository) (sreconcile.Result, error) {
	// Garbage collect previous advertised artifact(s) from storage
	_ = r.garbageCollect(ctx, obj)

	// Determine if the advertised artifact is still in storage
	var artifactMissing bool
	if artifact := obj.GetArtifact(); artifact != nil && !r.Storage.ArtifactExist(*artifact) {
		obj.Status.Artifact = nil
		obj.Status.URL = ""
		artifactMissing = true
		// Remove the condition as the artifact doesn't exist.
		conditions.Delete(obj, sourcev1.ArtifactInStorageCondition)
	}

	// Record that we do not have an artifact
	if obj.GetArtifact() == nil {
		msg := "building artifact"
		if artifactMissing {
			msg += ": disappeared from storage"
		}
		rreconcile.ProgressiveStatus(true, obj, meta.ProgressingReason, msg)
		conditions.Delete(obj, sourcev1.ArtifactInStorageCondition)
		if err := sp.Patch(ctx, obj, r.patchOptions...); err != nil {
			return sreconcile.ResultEmpty, err
		}
		return sreconcile.ResultSuccess, nil
	}

	// Always update URLs to ensure hostname is up-to-date
	// TODO(hidde): we may want to send out an event only if we notice the URL has changed
	r.Storage.SetArtifactURL(obj.GetArtifact())
	obj.Status.URL = r.Storage.SetHostname(obj.Status.URL)

	return sreconcile.ResultSuccess, nil
}

// reconcileSource attempts to fetch the Helm repository index using the
// specified configuration on the v1beta2.HelmRepository object.
//
// When the fetch fails, it records v1beta2.FetchFailedCondition=True and
// returns early.
// If successful and the index is valid, any previous
// v1beta2.FetchFailedCondition is removed, and the repository.ChartRepository
// pointer is set to the newly fetched index.
func (r *HelmRepositoryReconciler) reconcileSource(ctx context.Context, sp *patch.SerialPatcher,
	obj *sourcev1.HelmRepository, artifact *sourcev1.Artifact, chartRepo *repository.ChartRepository) (sreconcile.Result, error) {
	var tlsConfig *tls.Config

	// Configure Helm client to access repository
	clientOpts := []helmgetter.Option{
		helmgetter.WithTimeout(obj.Spec.Timeout.Duration),
		helmgetter.WithURL(obj.Spec.URL),
		helmgetter.WithPassCredentialsAll(obj.Spec.PassCredentials),
	}

	// Configure any authentication related options
	if obj.Spec.SecretRef != nil {
		// Attempt to retrieve secret
		name := types.NamespacedName{
			Namespace: obj.GetNamespace(),
			Name:      obj.Spec.SecretRef.Name,
		}
		var secret corev1.Secret
		if err := r.Client.Get(ctx, name, &secret); err != nil {
			e := &serror.Event{
				Err:    fmt.Errorf("failed to get secret '%s': %w", name.String(), err),
				Reason: sourcev1.AuthenticationFailedReason,
			}
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
			return sreconcile.ResultEmpty, e
		}

		// Construct actual options
		opts, err := getter.ClientOptionsFromSecret(secret)
		if err != nil {
			e := &serror.Event{
				Err:    fmt.Errorf("failed to configure Helm client with secret data: %w", err),
				Reason: sourcev1.AuthenticationFailedReason,
			}
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
			// Return err as the content of the secret may change.
			return sreconcile.ResultEmpty, e
		}
		clientOpts = append(clientOpts, opts...)

		tlsConfig, err = getter.TLSClientConfigFromSecret(secret, obj.Spec.URL)
		if err != nil {
			e := &serror.Event{
				Err:    fmt.Errorf("failed to create TLS client config with secret data: %w", err),
				Reason: sourcev1.AuthenticationFailedReason,
			}
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
			// Requeue as content of secret might change
			return sreconcile.ResultEmpty, e
		}
	}

	// Construct Helm chart repository with options and download index
	newChartRepo, err := repository.NewChartRepository(obj.Spec.URL, "", r.Getters, tlsConfig, clientOpts...)
	if err != nil {
		switch err.(type) {
		case *url.Error:
			e := &serror.Stalling{
				Err:    fmt.Errorf("invalid Helm repository URL: %w", err),
				Reason: sourcev1.URLInvalidReason,
			}
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
			return sreconcile.ResultEmpty, e
		default:
			e := &serror.Stalling{
				Err:    fmt.Errorf("failed to construct Helm client: %w", err),
				Reason: meta.FailedReason,
			}
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
			return sreconcile.ResultEmpty, e
		}
	}

	// Fetch the repository index from remote.
	if err := newChartRepo.CacheIndex(); err != nil {
		e := &serror.Event{
			Err:    fmt.Errorf("failed to fetch Helm repository index: %w", err),
			Reason: meta.FailedReason,
		}
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
		// Coin flip on transient or persistent error, return error and hope for the best
		return sreconcile.ResultEmpty, e
	}
	*chartRepo = *newChartRepo

	// Early comparison to current Artifact.
	if curArtifact := obj.GetArtifact(); curArtifact != nil {
		curDig := digest.Digest(curArtifact.Digest)
		if curDig == "" {
			curDig = digest.Digest(sourcev1.TransformLegacyRevision(curArtifact.Checksum))
		}
		if curDig.Validate() == nil {
			// Short-circuit based on the fetched index being an exact match to the
			// stored Artifact.
			if newDig := chartRepo.Digest(curDig.Algorithm()); newDig.Validate() == nil && (newDig == curDig) {
				*artifact = *curArtifact
				conditions.Delete(obj, sourcev1.FetchFailedCondition)
				return sreconcile.ResultSuccess, nil
			}
		}
	}

	// Load the cached repository index to ensure it passes validation.
	if err := chartRepo.LoadFromPath(); err != nil {
		e := &serror.Event{
			Err:    fmt.Errorf("failed to load Helm repository from index YAML: %w", err),
			Reason: sourcev1.IndexationFailedReason,
		}
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}
	// Delete any stale failure observation
	conditions.Delete(obj, sourcev1.FetchFailedCondition)

	// Check if index has changed compared to current Artifact revision.
	var changed bool
	if artifact := obj.Status.Artifact; artifact != nil {
		curRev := digest.Digest(sourcev1.TransformLegacyRevision(artifact.Revision))
		changed = curRev.Validate() != nil || curRev != chartRepo.Digest(curRev.Algorithm())
	}

	// Calculate revision.
	revision := chartRepo.Digest(intdigest.Canonical)
	if revision.Validate() != nil {
		e := &serror.Event{
			Err:    fmt.Errorf("failed to calculate revision: %w", err),
			Reason: sourcev1.IndexationFailedReason,
		}
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}

	// Mark observations about the revision on the object.
	if obj.Status.Artifact == nil || changed {
		message := fmt.Sprintf("new index revision '%s'", revision)
		if obj.GetArtifact() != nil {
			conditions.MarkTrue(obj, sourcev1.ArtifactOutdatedCondition, "NewRevision", message)
		}
		rreconcile.ProgressiveStatus(true, obj, meta.ProgressingReason, "building artifact: %s", message)
		if err := sp.Patch(ctx, obj, r.patchOptions...); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "failed to patch")
			return sreconcile.ResultEmpty, err
		}
	}

	// Create potential new artifact.
	*artifact = r.Storage.NewArtifactFor(obj.Kind,
		obj.ObjectMeta.GetObjectMeta(),
		revision.String(),
		fmt.Sprintf("index-%s.yaml", revision.Hex()),
	)

	return sreconcile.ResultSuccess, nil
}

// reconcileArtifact archives a new Artifact to the Storage, if the current
// (Status) data on the object does not match the given.
//
// The inspection of the given data to the object is differed, ensuring any
// stale observations like v1beta2.ArtifactOutdatedCondition are removed.
// If the given Artifact does not differ from the object's current, it returns
// early.
// On a successful archive, the Artifact in the Status of the object is set,
// and the symlink in the Storage is updated to its path.
func (r *HelmRepositoryReconciler) reconcileArtifact(ctx context.Context, sp *patch.SerialPatcher, obj *sourcev1.HelmRepository, artifact *sourcev1.Artifact, chartRepo *repository.ChartRepository) (sreconcile.Result, error) {
	// Set the ArtifactInStorageCondition if there's no drift.
	defer func() {
		if obj.GetArtifact().HasRevision(artifact.Revision) {
			conditions.Delete(obj, sourcev1.ArtifactOutdatedCondition)
			conditions.MarkTrue(obj, sourcev1.ArtifactInStorageCondition, meta.SucceededReason,
				"stored artifact: revision '%s'", artifact.Revision)
		}
		if err := chartRepo.Clear(); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "failed to remove temporary cached index file")
		}
	}()

	if obj.GetArtifact().HasRevision(artifact.Revision) && obj.GetArtifact().HasChecksum(artifact.Checksum) {
		// Extend TTL of the Index in the cache (if present).
		if r.Cache != nil {
			r.Cache.SetExpiration(artifact.Path, r.TTL)
		}

		r.eventLogf(ctx, obj, eventv1.EventTypeTrace, sourcev1.ArtifactUpToDateReason, "artifact up-to-date with remote revision: '%s'", artifact.Revision)
		return sreconcile.ResultSuccess, nil
	}

	// Create artifact dir
	if err := r.Storage.MkdirAll(*artifact); err != nil {
		e := &serror.Event{
			Err:    fmt.Errorf("failed to create artifact directory: %w", err),
			Reason: sourcev1.DirCreationFailedReason,
		}
		conditions.MarkTrue(obj, sourcev1.StorageOperationFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}

	// Acquire lock.
	unlock, err := r.Storage.Lock(*artifact)
	if err != nil {
		return sreconcile.ResultEmpty, &serror.Event{
			Err:    fmt.Errorf("failed to acquire lock for artifact: %w", err),
			Reason: meta.FailedReason,
		}
	}
	defer unlock()

	// Save artifact to storage.
	if err = r.Storage.CopyFromPath(artifact, chartRepo.Path); err != nil {
		e := &serror.Event{
			Err:    fmt.Errorf("unable to save artifact to storage: %w", err),
			Reason: sourcev1.ArchiveOperationFailedReason,
		}
		conditions.MarkTrue(obj, sourcev1.StorageOperationFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}

	// Record it on the object.
	obj.Status.Artifact = artifact.DeepCopy()

	// Cache the index if it was successfully retrieved.
	if r.Cache != nil && chartRepo.Index != nil {
		// The cache keys have to be safe in multi-tenancy environments, as
		// otherwise it could be used as a vector to bypass the repository's
		// authentication. Using the Artifact.Path is safe as the path is in
		// the format of: /<repository-name>/<chart-name>/<filename>.
		if err := r.Cache.Set(artifact.Path, chartRepo.Index, r.TTL); err != nil {
			r.eventLogf(ctx, obj, eventv1.EventTypeTrace, sourcev1.CacheOperationFailedReason, "failed to cache index: %s", err)
		}
	}

	// Update index symlink.
	indexURL, err := r.Storage.Symlink(*artifact, "index.yaml")
	if err != nil {
		r.eventLogf(ctx, obj, eventv1.EventTypeTrace, sourcev1.SymlinkUpdateFailedReason,
			"failed to update status URL symlink: %s", err)
	}
	if indexURL != "" {
		obj.Status.URL = indexURL
	}
	conditions.Delete(obj, sourcev1.StorageOperationFailedCondition)
	return sreconcile.ResultSuccess, nil
}

// reconcileDelete handles the deletion of the object.
// It first garbage collects all Artifacts for the object from the Storage.
// Removing the finalizer from the object if successful.
func (r *HelmRepositoryReconciler) reconcileDelete(ctx context.Context, obj *sourcev1.HelmRepository) (sreconcile.Result, error) {
	// Garbage collect the resource's artifacts
	if err := r.garbageCollect(ctx, obj); err != nil {
		// Return the error so we retry the failed garbage collection
		return sreconcile.ResultEmpty, err
	}

	// Remove our finalizer from the list if we are deleting the object
	if !obj.DeletionTimestamp.IsZero() {
		controllerutil.RemoveFinalizer(obj, sourcev1.SourceFinalizer)
	}

	// Stop reconciliation as the object is being deleted
	return sreconcile.ResultEmpty, nil
}

// garbageCollect performs a garbage collection for the given object.
//
// It removes all but the current Artifact from the Storage, unless:
// - the deletion timestamp on the object is set
// - the obj.Spec.Type has changed and artifacts are not supported by the new type
// Which will result in the removal of all Artifacts for the objects.
func (r *HelmRepositoryReconciler) garbageCollect(ctx context.Context, obj *sourcev1.HelmRepository) error {
	if !obj.DeletionTimestamp.IsZero() || (obj.Spec.Type != "" && obj.Spec.Type != sourcev1.HelmRepositoryTypeDefault) {
		if deleted, err := r.Storage.RemoveAll(r.Storage.NewArtifactFor(obj.Kind, obj.GetObjectMeta(), "", "*")); err != nil {
			return &serror.Event{
				Err:    fmt.Errorf("garbage collection for deleted resource failed: %w", err),
				Reason: "GarbageCollectionFailed",
			}
		} else if deleted != "" {
			r.eventLogf(ctx, obj, eventv1.EventTypeTrace, "GarbageCollectionSucceeded",
				"garbage collected artifacts for deleted resource")
		}
		// Clean status sub-resource
		obj.Status.Artifact = nil
		obj.Status.URL = ""
		// Remove any stale conditions.
		obj.Status.Conditions = nil
		return nil
	}
	if obj.GetArtifact() != nil {
		delFiles, err := r.Storage.GarbageCollect(ctx, *obj.GetArtifact(), time.Second*5)
		if err != nil {
			return &serror.Event{
				Err:    fmt.Errorf("garbage collection of artifacts failed: %w", err),
				Reason: "GarbageCollectionFailed",
			}
		}
		if len(delFiles) > 0 {
			r.eventLogf(ctx, obj, eventv1.EventTypeTrace, "GarbageCollectionSucceeded",
				fmt.Sprintf("garbage collected %d artifacts", len(delFiles)))
			return nil
		}
	}
	return nil
}

// eventLogf records events, and logs at the same time.
//
// This log is different from the debug log in the EventRecorder, in the sense
// that this is a simple log. While the debug log contains complete details
// about the event.
func (r *HelmRepositoryReconciler) eventLogf(ctx context.Context, obj runtime.Object, eventType string, reason string, messageFmt string, args ...interface{}) {
	msg := fmt.Sprintf(messageFmt, args...)
	// Log and emit event.
	if eventType == corev1.EventTypeWarning {
		ctrl.LoggerFrom(ctx).Error(errors.New(reason), msg)
	} else {
		ctrl.LoggerFrom(ctx).Info(msg)
	}
	r.Eventf(obj, eventType, reason, msg)
}

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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
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

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"
	helper "github.com/fluxcd/pkg/runtime/controller"
	"github.com/fluxcd/pkg/runtime/patch"
	"github.com/fluxcd/pkg/runtime/predicates"
	rreconcile "github.com/fluxcd/pkg/runtime/reconcile"

	eventv1 "github.com/fluxcd/pkg/apis/event/v1beta1"
	"github.com/fluxcd/pkg/sourceignore"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	bucketv1 "github.com/fluxcd/source-controller/api/v1beta2"
	intdigest "github.com/fluxcd/source-controller/internal/digest"
	serror "github.com/fluxcd/source-controller/internal/error"
	"github.com/fluxcd/source-controller/internal/index"
	sreconcile "github.com/fluxcd/source-controller/internal/reconcile"
	"github.com/fluxcd/source-controller/internal/reconcile/summarize"
	"github.com/fluxcd/source-controller/pkg/azure"
	"github.com/fluxcd/source-controller/pkg/gcp"
	"github.com/fluxcd/source-controller/pkg/minio"
)

// maxConcurrentBucketFetches is the upper bound on the goroutines used to
// fetch bucket objects. It's important to have a bound, to avoid
// using arbitrary amounts of memory; the actual number is chosen
// according to the queueing rule of thumb with some conservative
// parameters:
// s > Nr / T
// N (number of requestors, i.e., objects to fetch) = 10000
// r (service time -- fetch duration) = 0.01s (~ a megabyte file over 1Gb/s)
// T (total time available) = 1s
// -> s > 100
const maxConcurrentBucketFetches = 100

// bucketReadyCondition contains the information required to summarize a
// v1beta2.Bucket Ready Condition.
var bucketReadyCondition = summarize.Conditions{
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

// bucketFailConditions contains the conditions that represent a failure.
var bucketFailConditions = []string{
	sourcev1.FetchFailedCondition,
	sourcev1.StorageOperationFailedCondition,
}

// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=buckets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=buckets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=buckets/finalizers,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// BucketReconciler reconciles a v1beta2.Bucket object.
type BucketReconciler struct {
	client.Client
	kuberecorder.EventRecorder
	helper.Metrics

	Storage        *Storage
	ControllerName string

	patchOptions []patch.Option
}

type BucketReconcilerOptions struct {
	MaxConcurrentReconciles int
	RateLimiter             ratelimiter.RateLimiter
}

// BucketProvider is an interface for fetching objects from a storage provider
// bucket.
type BucketProvider interface {
	// BucketExists returns if an object storage bucket with the provided name
	// exists, or returns a (client) error.
	BucketExists(ctx context.Context, bucketName string) (bool, error)
	// FGetObject gets the object from the provided object storage bucket, and
	// writes it to targetPath.
	// It returns the etag of the successfully fetched file, or any error.
	FGetObject(ctx context.Context, bucketName, objectKey, targetPath string) (etag string, err error)
	// VisitObjects iterates over the items in the provided object storage
	// bucket, calling visit for every item.
	// If the underlying client or the visit callback returns an error,
	// it returns early.
	VisitObjects(ctx context.Context, bucketName string, visit func(key, etag string) error) error
	// ObjectIsNotFound returns true if the given error indicates an object
	// could not be found.
	ObjectIsNotFound(error) bool
	// Close closes the provider's client, if supported.
	Close(context.Context)
}

// bucketReconcileFunc is the function type for all the v1beta2.Bucket
// (sub)reconcile functions. The type implementations are grouped and
// executed serially to perform the complete reconcile of the object.
type bucketReconcileFunc func(ctx context.Context, sp *patch.SerialPatcher, obj *bucketv1.Bucket, index *index.Digester, dir string) (sreconcile.Result, error)

func (r *BucketReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.SetupWithManagerAndOptions(mgr, BucketReconcilerOptions{})
}

func (r *BucketReconciler) SetupWithManagerAndOptions(mgr ctrl.Manager, opts BucketReconcilerOptions) error {
	r.patchOptions = getPatchOptions(bucketReadyCondition.Owned, r.ControllerName)

	return ctrl.NewControllerManagedBy(mgr).
		For(&bucketv1.Bucket{}).
		WithEventFilter(predicate.Or(predicate.GenerationChangedPredicate{}, predicates.ReconcileRequestedPredicate{})).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: opts.MaxConcurrentReconciles,
			RateLimiter:             opts.RateLimiter,
		}).
		Complete(r)
}

func (r *BucketReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	start := time.Now()
	log := ctrl.LoggerFrom(ctx)

	// Fetch the Bucket
	obj := &bucketv1.Bucket{}
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Record suspended status metric
	r.RecordSuspend(ctx, obj, obj.Spec.Suspend)

	// Initialize the patch helper with the current version of the object.
	serialPatcher := patch.NewSerialPatcher(obj, r.Client)

	// recResult stores the abstracted reconcile result.
	var recResult sreconcile.Result

	// Always attempt to patch the object and status after each reconciliation
	// NOTE: The final runtime result and error are set in this block.
	defer func() {
		summarizeHelper := summarize.NewHelper(r.EventRecorder, serialPatcher)
		summarizeOpts := []summarize.Option{
			summarize.WithConditions(bucketReadyCondition),
			summarize.WithReconcileResult(recResult),
			summarize.WithReconcileError(retErr),
			summarize.WithIgnoreNotFound(),
			summarize.WithProcessors(
				summarize.RecordContextualError,
				summarize.RecordReconcileReq,
			),
			summarize.WithResultBuilder(sreconcile.AlwaysRequeueResultBuilder{RequeueAfter: obj.GetRequeueAfter()}),
			summarize.WithPatchFieldOwner(r.ControllerName),
		}
		result, retErr = summarizeHelper.SummarizeAndPatch(ctx, obj, summarizeOpts...)

		// Always record readiness and duration metrics
		r.Metrics.RecordReadiness(ctx, obj)
		r.Metrics.RecordDuration(ctx, obj, start)
	}()

	// Add finalizer first if not exist to avoid the race condition between init and delete
	if !controllerutil.ContainsFinalizer(obj, sourcev1.SourceFinalizer) {
		controllerutil.AddFinalizer(obj, sourcev1.SourceFinalizer)
		recResult = sreconcile.ResultRequeue
		return
	}

	// Examine if the object is under deletion
	if !obj.ObjectMeta.DeletionTimestamp.IsZero() {
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
	reconcilers := []bucketReconcileFunc{
		r.reconcileStorage,
		r.reconcileSource,
		r.reconcileArtifact,
	}
	recResult, retErr = r.reconcile(ctx, serialPatcher, obj, reconcilers)
	return
}

// reconcile iterates through the bucketReconcileFunc tasks for the
// object. It returns early on the first call that returns
// reconcile.ResultRequeue, or produces an error.
func (r *BucketReconciler) reconcile(ctx context.Context, sp *patch.SerialPatcher, obj *bucketv1.Bucket, reconcilers []bucketReconcileFunc) (sreconcile.Result, error) {
	oldObj := obj.DeepCopy()

	rreconcile.ProgressiveStatus(false, obj, meta.ProgressingReason, "reconciliation in progress")

	var recAtVal string
	if v, ok := meta.ReconcileAnnotationValue(obj.GetAnnotations()); ok {
		recAtVal = v
	}

	// Persist reconciling if generation differs or reconciliation is requested.
	switch {
	case obj.Generation != obj.Status.ObservedGeneration:
		rreconcile.ProgressiveStatus(false, obj, meta.ProgressingReason,
			"processing object: new generation %d -> %d", obj.Status.ObservedGeneration, obj.Generation)
		if err := sp.Patch(ctx, obj, r.patchOptions...); err != nil {
			return sreconcile.ResultEmpty, err
		}
	case recAtVal != obj.Status.GetLastHandledReconcileRequest():
		if err := sp.Patch(ctx, obj, r.patchOptions...); err != nil {
			return sreconcile.ResultEmpty, err
		}
	}

	// Create temp working dir
	tmpDir, err := os.MkdirTemp("", fmt.Sprintf("%s-%s-%s-", obj.Kind, obj.Namespace, obj.Name))
	if err != nil {
		e := &serror.Event{
			Err:    fmt.Errorf("failed to create temporary working directory: %w", err),
			Reason: sourcev1.DirCreationFailedReason,
		}
		conditions.MarkTrue(obj, sourcev1.StorageOperationFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}
	defer func() {
		if err = os.RemoveAll(tmpDir); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "failed to remove temporary working directory")
		}
	}()
	conditions.Delete(obj, sourcev1.StorageOperationFailedCondition)

	// Run the sub-reconcilers and build the result of reconciliation.
	var (
		res    sreconcile.Result
		resErr error
		index  = index.NewDigester()
	)

	for _, rec := range reconcilers {
		recResult, err := rec(ctx, sp, obj, index, tmpDir)
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
		// Prioritize requeue request in the result.
		res = sreconcile.LowestRequeuingResult(res, recResult)
	}

	r.notify(ctx, oldObj, obj, index, res, resErr)

	return res, resErr
}

// notify emits notification related to the reconciliation.
func (r *BucketReconciler) notify(ctx context.Context, oldObj, newObj *bucketv1.Bucket, index *index.Digester, res sreconcile.Result, resErr error) {
	// Notify successful reconciliation for new artifact and recovery from any
	// failure.
	if resErr == nil && res == sreconcile.ResultSuccess && newObj.Status.Artifact != nil {
		annotations := map[string]string{
			fmt.Sprintf("%s/%s", sourcev1.GroupVersion.Group, eventv1.MetaRevisionKey): newObj.Status.Artifact.Revision,
			fmt.Sprintf("%s/%s", sourcev1.GroupVersion.Group, eventv1.MetaDigestKey):   newObj.Status.Artifact.Digest,
		}

		message := fmt.Sprintf("stored artifact with %d fetched files from '%s' bucket", index.Len(), newObj.Spec.BucketName)

		// Notify on new artifact and failure recovery.
		if !oldObj.GetArtifact().HasDigest(newObj.GetArtifact().Digest) {
			r.AnnotatedEventf(newObj, annotations, corev1.EventTypeNormal,
				"NewArtifact", message)
			ctrl.LoggerFrom(ctx).Info(message)
		} else {
			if sreconcile.FailureRecovery(oldObj, newObj, bucketFailConditions) {
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
func (r *BucketReconciler) reconcileStorage(ctx context.Context, sp *patch.SerialPatcher, obj *bucketv1.Bucket, _ *index.Digester, _ string) (sreconcile.Result, error) {
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

// reconcileSource fetches the upstream bucket contents with the client for the
// given object's Provider, and returns the result.
// When a SecretRef is defined, it attempts to fetch the Secret before calling
// the provider. If this fails, it records v1beta2.FetchFailedCondition=True on
// the object and returns early.
func (r *BucketReconciler) reconcileSource(ctx context.Context, sp *patch.SerialPatcher, obj *bucketv1.Bucket, index *index.Digester, dir string) (sreconcile.Result, error) {
	secret, err := r.getBucketSecret(ctx, obj)
	if err != nil {
		e := &serror.Event{Err: err, Reason: sourcev1.AuthenticationFailedReason}
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Error())
		// Return error as the world as observed may change
		return sreconcile.ResultEmpty, e
	}

	// Construct provider client
	var provider BucketProvider
	switch obj.Spec.Provider {
	case bucketv1.GoogleBucketProvider:
		if err = gcp.ValidateSecret(secret); err != nil {
			e := &serror.Event{Err: err, Reason: sourcev1.AuthenticationFailedReason}
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Error())
			return sreconcile.ResultEmpty, e
		}
		if provider, err = gcp.NewClient(ctx, secret); err != nil {
			e := &serror.Event{Err: err, Reason: "ClientError"}
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Error())
			return sreconcile.ResultEmpty, e
		}
	case bucketv1.AzureBucketProvider:
		if err = azure.ValidateSecret(secret); err != nil {
			e := &serror.Event{Err: err, Reason: sourcev1.AuthenticationFailedReason}
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Error())
			return sreconcile.ResultEmpty, e
		}
		if provider, err = azure.NewClient(obj, secret); err != nil {
			e := &serror.Event{Err: err, Reason: "ClientError"}
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Error())
			return sreconcile.ResultEmpty, e
		}
	default:
		if err = minio.ValidateSecret(secret); err != nil {
			e := &serror.Event{Err: err, Reason: sourcev1.AuthenticationFailedReason}
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Error())
			return sreconcile.ResultEmpty, e
		}
		if provider, err = minio.NewClient(obj, secret); err != nil {
			e := &serror.Event{Err: err, Reason: "ClientError"}
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Error())
			return sreconcile.ResultEmpty, e
		}
	}

	// Fetch etag index
	if err = fetchEtagIndex(ctx, provider, obj, index, dir); err != nil {
		e := &serror.Event{Err: err, Reason: bucketv1.BucketOperationFailedReason}
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Error())
		return sreconcile.ResultEmpty, e
	}

	// Check if index has changed compared to current Artifact revision.
	var changed bool
	if artifact := obj.Status.Artifact; artifact != nil && artifact.Revision != "" {
		curRev := digest.Digest(artifact.Revision)
		changed = curRev.Validate() != nil || curRev != index.Digest(curRev.Algorithm())
	}

	// Fetch the bucket objects if required to.
	if artifact := obj.GetArtifact(); artifact == nil || changed {
		// Mark observations about the revision on the object
		defer func() {
			// As fetchIndexFiles can make last-minute modifications to the etag
			// index, we need to re-calculate the revision at the end
			revision := index.Digest(intdigest.Canonical)

			message := fmt.Sprintf("new upstream revision '%s'", revision)
			if obj.GetArtifact() != nil {
				conditions.MarkTrue(obj, sourcev1.ArtifactOutdatedCondition, "NewRevision", message)
			}
			rreconcile.ProgressiveStatus(true, obj, meta.ProgressingReason, "building artifact: %s", message)
			if err := sp.Patch(ctx, obj, r.patchOptions...); err != nil {
				ctrl.LoggerFrom(ctx).Error(err, "failed to patch")
				return
			}
		}()

		if err = fetchIndexFiles(ctx, provider, obj, index, dir); err != nil {
			e := &serror.Event{Err: err, Reason: bucketv1.BucketOperationFailedReason}
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Error())
			return sreconcile.ResultEmpty, e
		}
	}

	conditions.Delete(obj, sourcev1.FetchFailedCondition)
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
func (r *BucketReconciler) reconcileArtifact(ctx context.Context, sp *patch.SerialPatcher, obj *bucketv1.Bucket, index *index.Digester, dir string) (sreconcile.Result, error) {
	// Calculate revision
	revision := index.Digest(intdigest.Canonical)

	// Create artifact
	artifact := r.Storage.NewArtifactFor(obj.Kind, obj, revision.String(), fmt.Sprintf("%s.tar.gz", revision.Encoded()))

	// Set the ArtifactInStorageCondition if there's no drift.
	defer func() {
		if curArtifact := obj.GetArtifact(); curArtifact != nil && curArtifact.Revision != "" {
			curRev := digest.Digest(curArtifact.Revision)
			if curRev.Validate() == nil && index.Digest(curRev.Algorithm()) == curRev {
				conditions.Delete(obj, sourcev1.ArtifactOutdatedCondition)
				conditions.MarkTrue(obj, sourcev1.ArtifactInStorageCondition, meta.SucceededReason,
					"stored artifact: revision '%s'", artifact.Revision)
			}
		}
	}()

	// The artifact is up-to-date
	if curArtifact := obj.GetArtifact(); curArtifact != nil && curArtifact.Revision != "" {
		curRev := digest.Digest(curArtifact.Revision)
		if curRev.Validate() == nil && index.Digest(curRev.Algorithm()) == curRev {
			r.eventLogf(ctx, obj, eventv1.EventTypeTrace, sourcev1.ArtifactUpToDateReason, "artifact up-to-date with remote revision: '%s'", artifact.Revision)
			return sreconcile.ResultSuccess, nil
		}
	}

	// Ensure target path exists and is a directory
	if f, err := os.Stat(dir); err != nil {
		e := &serror.Event{
			Err:    fmt.Errorf("failed to stat source path: %w", err),
			Reason: sourcev1.StatOperationFailedReason,
		}
		conditions.MarkTrue(obj, sourcev1.StorageOperationFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	} else if !f.IsDir() {
		e := &serror.Event{
			Err:    fmt.Errorf("source path '%s' is not a directory", dir),
			Reason: sourcev1.InvalidPathReason,
		}
		conditions.MarkTrue(obj, sourcev1.StorageOperationFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}

	// Ensure artifact directory exists and acquire lock
	if err := r.Storage.MkdirAll(artifact); err != nil {
		e := &serror.Event{
			Err:    fmt.Errorf("failed to create artifact directory: %w", err),
			Reason: sourcev1.DirCreationFailedReason,
		}
		conditions.MarkTrue(obj, sourcev1.StorageOperationFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}
	unlock, err := r.Storage.Lock(artifact)
	if err != nil {
		return sreconcile.ResultEmpty, &serror.Event{
			Err:    fmt.Errorf("failed to acquire lock for artifact: %w", err),
			Reason: meta.FailedReason,
		}
	}
	defer unlock()

	// Archive directory to storage
	if err := r.Storage.Archive(&artifact, dir, nil); err != nil {
		e := &serror.Event{
			Err:    fmt.Errorf("unable to archive artifact to storage: %s", err),
			Reason: sourcev1.ArchiveOperationFailedReason,
		}
		conditions.MarkTrue(obj, sourcev1.StorageOperationFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}

	// Record it on the object
	obj.Status.Artifact = artifact.DeepCopy()
	obj.Status.ObservedIgnore = obj.Spec.Ignore

	// Update symlink on a "best effort" basis
	url, err := r.Storage.Symlink(artifact, "latest.tar.gz")
	if err != nil {
		r.eventLogf(ctx, obj, eventv1.EventTypeTrace, sourcev1.SymlinkUpdateFailedReason,
			"failed to update status URL symlink: %s", err)
	}
	if url != "" {
		obj.Status.URL = url
	}
	conditions.Delete(obj, sourcev1.StorageOperationFailedCondition)
	return sreconcile.ResultSuccess, nil
}

// reconcileDelete handles the deletion of the object.
// It first garbage collects all Artifacts for the object from the Storage.
// Removing the finalizer from the object if successful.
func (r *BucketReconciler) reconcileDelete(ctx context.Context, obj *bucketv1.Bucket) (sreconcile.Result, error) {
	// Garbage collect the resource's artifacts
	if err := r.garbageCollect(ctx, obj); err != nil {
		// Return the error so we retry the failed garbage collection
		return sreconcile.ResultEmpty, err
	}

	// Remove our finalizer from the list
	controllerutil.RemoveFinalizer(obj, sourcev1.SourceFinalizer)

	// Stop reconciliation as the object is being deleted
	return sreconcile.ResultEmpty, nil
}

// garbageCollect performs a garbage collection for the given object.
//
// It removes all but the current Artifact from the Storage, unless the
// deletion timestamp on the object is set. Which will result in the
// removal of all Artifacts for the objects.
func (r *BucketReconciler) garbageCollect(ctx context.Context, obj *bucketv1.Bucket) error {
	if !obj.DeletionTimestamp.IsZero() {
		if deleted, err := r.Storage.RemoveAll(r.Storage.NewArtifactFor(obj.Kind, obj.GetObjectMeta(), "", "*")); err != nil {
			return &serror.Event{
				Err:    fmt.Errorf("garbage collection for deleted resource failed: %s", err),
				Reason: "GarbageCollectionFailed",
			}
		} else if deleted != "" {
			r.eventLogf(ctx, obj, eventv1.EventTypeTrace, "GarbageCollectionSucceeded",
				"garbage collected artifacts for deleted resource")
		}
		obj.Status.Artifact = nil
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

// getBucketSecret attempts to fetch the Secret reference if specified on the
// obj. It returns any client error.
func (r *BucketReconciler) getBucketSecret(ctx context.Context, obj *bucketv1.Bucket) (*corev1.Secret, error) {
	if obj.Spec.SecretRef == nil {
		return nil, nil
	}
	secretName := types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      obj.Spec.SecretRef.Name,
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, secretName, secret); err != nil {
		return nil, fmt.Errorf("failed to get secret '%s': %w", secretName.String(), err)
	}
	return secret, nil
}

// eventLogf records events, and logs at the same time.
//
// This log is different from the debug log in the EventRecorder, in the sense
// that this is a simple log. While the debug log contains complete details
// about the event.
func (r *BucketReconciler) eventLogf(ctx context.Context, obj runtime.Object, eventType string, reason string, messageFmt string, args ...interface{}) {
	r.annotatedEventLogf(ctx, obj, nil, eventType, reason, messageFmt, args...)
}

// annotatedEventLogf records annotated events, and logs at the same time.
//
// This log is different from the debug log in the EventRecorder, in the sense
// that this is a simple log. While the debug log contains complete details
// about the event.
func (r *BucketReconciler) annotatedEventLogf(ctx context.Context,
	obj runtime.Object, annotations map[string]string, eventType string, reason string, messageFmt string, args ...interface{}) {
	msg := fmt.Sprintf(messageFmt, args...)
	// Log and emit event.
	if eventType == corev1.EventTypeWarning {
		ctrl.LoggerFrom(ctx).Error(errors.New(reason), msg)
	} else {
		ctrl.LoggerFrom(ctx).Info(msg)
	}
	r.AnnotatedEventf(obj, annotations, eventType, reason, msg)
}

// fetchEtagIndex fetches the current etagIndex for the in the obj specified
// bucket using the given provider, while filtering them using .sourceignore
// rules. After fetching an object, the etag value in the index is updated to
// the current value to ensure accuracy.
func fetchEtagIndex(ctx context.Context, provider BucketProvider, obj *bucketv1.Bucket, index *index.Digester, tempDir string) error {
	ctxTimeout, cancel := context.WithTimeout(ctx, obj.Spec.Timeout.Duration)
	defer cancel()

	// Confirm bucket exists
	exists, err := provider.BucketExists(ctxTimeout, obj.Spec.BucketName)
	if err != nil {
		return fmt.Errorf("failed to confirm existence of '%s' bucket: %w", obj.Spec.BucketName, err)
	}
	if !exists {
		err = fmt.Errorf("bucket '%s' not found", obj.Spec.BucketName)
		return err
	}

	// Look for file with ignore rules first
	path := filepath.Join(tempDir, sourceignore.IgnoreFile)
	if _, err := provider.FGetObject(ctxTimeout, obj.Spec.BucketName, sourceignore.IgnoreFile, path); err != nil {
		if !provider.ObjectIsNotFound(err) {
			return err
		}
	}
	ps, err := sourceignore.ReadIgnoreFile(path, nil)
	if err != nil {
		return err
	}
	// In-spec patterns take precedence
	if obj.Spec.Ignore != nil {
		ps = append(ps, sourceignore.ReadPatterns(strings.NewReader(*obj.Spec.Ignore), nil)...)
	}
	matcher := sourceignore.NewMatcher(ps)

	// Build up index
	err = provider.VisitObjects(ctxTimeout, obj.Spec.BucketName, func(key, etag string) error {
		if strings.HasSuffix(key, "/") || key == sourceignore.IgnoreFile {
			return nil
		}

		if matcher.Match(strings.Split(key, "/"), false) {
			return nil
		}

		index.Add(key, etag)
		return nil
	})
	if err != nil {
		return fmt.Errorf("indexation of objects from bucket '%s' failed: %w", obj.Spec.BucketName, err)
	}
	return nil
}

// fetchIndexFiles fetches the object files for the keys from the given etagIndex
// using the given provider, and stores them into tempDir. It downloads in
// parallel, but limited to the maxConcurrentBucketFetches.
// Given an index is provided, the bucket is assumed to exist.
func fetchIndexFiles(ctx context.Context, provider BucketProvider, obj *bucketv1.Bucket, index *index.Digester, tempDir string) error {
	ctxTimeout, cancel := context.WithTimeout(ctx, obj.Spec.Timeout.Duration)
	defer cancel()

	// Download in parallel, but bound the concurrency. According to
	// AWS and GCP docs, rate limits are either soft or don't exist:
	//  - https://cloud.google.com/storage/quotas
	//  - https://docs.aws.amazon.com/general/latest/gr/s3.html
	// .. so, the limiting factor is this process keeping a small footprint.
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		sem := semaphore.NewWeighted(maxConcurrentBucketFetches)
		for key, etag := range index.Index() {
			k := key
			t := etag
			if err := sem.Acquire(groupCtx, 1); err != nil {
				return err
			}
			group.Go(func() error {
				defer sem.Release(1)
				localPath := filepath.Join(tempDir, k)
				etag, err := provider.FGetObject(ctxTimeout, obj.Spec.BucketName, k, localPath)
				if err != nil {
					if provider.ObjectIsNotFound(err) {
						ctrl.LoggerFrom(ctx).Info(fmt.Sprintf("indexed object '%s' disappeared from '%s' bucket", k, obj.Spec.BucketName))
						index.Delete(k)
						return nil
					}
					return fmt.Errorf("failed to get '%s' object: %w", k, err)
				}
				if t != etag {
					index.Add(k, etag)
				}
				return nil
			})
		}
		return nil
	})
	if err := group.Wait(); err != nil {
		return fmt.Errorf("fetch from bucket '%s' failed: %w", obj.Spec.BucketName, err)
	}

	return nil
}

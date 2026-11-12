// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
)

const TrustProfileIndexFinalizer = "kognition.dev/trustprofile-index"

type TrustProfileIndexReconciler struct {
	client client.Client
}

func NewTrustProfileIndexReconciler(c client.Client) *TrustProfileIndexReconciler {
	return &TrustProfileIndexReconciler{client: c}
}

func TrustProfileIndexName(agentUID types.UID) string {
	digest := sha256.Sum256([]byte(agentUID))
	return "tpi-" + hex.EncodeToString(digest[:20])
}

func (r *TrustProfileIndexReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	profile := &k8s.TrustProfile{}
	if err := r.client.Get(ctx, req.NamespacedName, profile); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.deleteIndexesSelectingProfile(ctx, req.NamespacedName)
		}
		return ctrl.Result{}, err
	}
	if !profile.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.releaseDeletingProfile(ctx, profile)
	}

	agent := &k8s.Agent{}
	agentKey := client.ObjectKey{Namespace: profile.Namespace, Name: profile.Spec.AgentRef.Name}
	if err := r.client.Get(ctx, agentKey, agent); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.releaseInvalidAgentSelection(ctx, profile)
		}
		return ctrl.Result{}, fmt.Errorf("get TrustProfile Agent: %w", err)
	}
	if profile.Spec.AgentIncarnationUID != string(agent.UID) {
		if err := r.releaseInvalidAgentSelection(ctx, profile); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, fmt.Errorf("TrustProfile Agent incarnation does not match live Agent UID")
	}
	if profile.UID == "" {
		return ctrl.Result{}, fmt.Errorf("TrustProfile lacks stable UID")
	}

	if !controllerutil.ContainsFinalizer(profile, TrustProfileIndexFinalizer) {
		controllerutil.AddFinalizer(profile, TrustProfileIndexFinalizer)
		if err := r.client.Update(ctx, profile); err != nil {
			return ctrl.Result{}, fmt.Errorf("add TrustProfile index finalizer: %w", err)
		}
	}

	index := &k8s.TrustProfileIndex{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: profile.Namespace,
			Name:      TrustProfileIndexName(agent.UID),
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(agent, k8s.GroupVersion.WithKind("Agent")),
			},
		},
		Spec: k8s.TrustProfileIndexSpec{
			AgentUID:            string(agent.UID),
			SelectedProfileName: profile.Name,
			SelectedProfileUID:  string(profile.UID),
		},
	}
	if err := r.client.Create(ctx, index); err == nil {
		return ctrl.Result{}, nil
	} else if !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, fmt.Errorf("create TrustProfileIndex: %w", err)
	}

	winner := &k8s.TrustProfileIndex{}
	indexKey := client.ObjectKey{Namespace: index.Namespace, Name: index.Name}
	if err := r.client.Get(ctx, indexKey, winner); err != nil {
		return ctrl.Result{}, fmt.Errorf("get winning TrustProfileIndex: %w", err)
	}
	if winner.Spec.AgentUID != string(agent.UID) {
		return ctrl.Result{Requeue: true}, r.deleteInvalidIndex(ctx, winner)
	}
	selected, valid, err := r.validSelectedProfile(ctx, winner)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !valid {
		return ctrl.Result{Requeue: true}, r.deleteInvalidIndex(ctx, winner)
	}
	if winner.Spec.SelectedProfileUID == string(profile.UID) && winner.Spec.SelectedProfileName == profile.Name {
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(selected, TrustProfileIndexFinalizer) {
		controllerutil.AddFinalizer(selected, TrustProfileIndexFinalizer)
		if err := r.client.Update(ctx, selected); err != nil {
			return ctrl.Result{}, fmt.Errorf("add TrustProfile index finalizer to selected profile: %w", err)
		}
	}

	controllerutil.RemoveFinalizer(profile, TrustProfileIndexFinalizer)
	if err := r.client.Update(ctx, profile); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove TrustProfile index finalizer from losing candidate: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *TrustProfileIndexReconciler) deleteIndexesSelectingProfile(ctx context.Context, profileKey types.NamespacedName) error {
	indexes := &k8s.TrustProfileIndexList{}
	if err := r.client.List(ctx, indexes, client.InNamespace(profileKey.Namespace)); err != nil {
		return fmt.Errorf("list TrustProfileIndexes for missing profile: %w", err)
	}
	for i := range indexes.Items {
		index := &indexes.Items[i]
		if index.Spec.SelectedProfileName != profileKey.Name {
			continue
		}
		if err := r.client.Delete(ctx, index); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete TrustProfileIndex for missing profile: %w", err)
		}
	}
	return nil
}

func (r *TrustProfileIndexReconciler) releaseInvalidAgentSelection(ctx context.Context, profile *k8s.TrustProfile) error {
	index := &k8s.TrustProfileIndex{}
	indexKey := client.ObjectKey{Namespace: profile.Namespace, Name: TrustProfileIndexName(types.UID(profile.Spec.AgentIncarnationUID))}
	if err := r.client.Get(ctx, indexKey, index); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get TrustProfileIndex for invalid Agent: %w", err)
		}
	} else if err := r.client.Delete(ctx, index); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete TrustProfileIndex for invalid Agent: %w", err)
	}
	if controllerutil.ContainsFinalizer(profile, TrustProfileIndexFinalizer) {
		controllerutil.RemoveFinalizer(profile, TrustProfileIndexFinalizer)
		if err := r.client.Update(ctx, profile); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("remove TrustProfile index finalizer for invalid Agent: %w", err)
		}
	}
	return nil
}

func (r *TrustProfileIndexReconciler) validSelectedProfile(ctx context.Context, index *k8s.TrustProfileIndex) (*k8s.TrustProfile, bool, error) {
	selected := &k8s.TrustProfile{}
	selectedKey := client.ObjectKey{Namespace: index.Namespace, Name: index.Spec.SelectedProfileName}
	if err := r.client.Get(ctx, selectedKey, selected); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get selected TrustProfile: %w", err)
	}
	valid := selected.UID != "" &&
		string(selected.UID) == index.Spec.SelectedProfileUID &&
		selected.Spec.AgentIncarnationUID == index.Spec.AgentUID &&
		selected.DeletionTimestamp.IsZero()
	return selected, valid, nil
}

func (r *TrustProfileIndexReconciler) deleteInvalidIndex(ctx context.Context, index *k8s.TrustProfileIndex) error {
	if err := r.client.Delete(ctx, index); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete invalid TrustProfileIndex: %w", err)
	}
	return nil
}

func (r *TrustProfileIndexReconciler) releaseDeletingProfile(ctx context.Context, profile *k8s.TrustProfile) error {
	if !controllerutil.ContainsFinalizer(profile, TrustProfileIndexFinalizer) {
		return nil
	}
	indexKey := client.ObjectKey{Namespace: profile.Namespace, Name: TrustProfileIndexName(types.UID(profile.Spec.AgentIncarnationUID))}
	index := &k8s.TrustProfileIndex{}
	if err := r.client.Get(ctx, indexKey, index); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get TrustProfileIndex during profile deletion: %w", err)
		}
	} else if index.Spec.SelectedProfileUID == string(profile.UID) {
		replacement, err := r.replacementProfile(ctx, profile)
		if err != nil {
			return err
		}
		if replacement == nil {
			if err := r.client.Delete(ctx, index); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete TrustProfileIndex: %w", err)
			}
		} else {
			if !controllerutil.ContainsFinalizer(replacement, TrustProfileIndexFinalizer) {
				controllerutil.AddFinalizer(replacement, TrustProfileIndexFinalizer)
				if err := r.client.Update(ctx, replacement); err != nil {
					return fmt.Errorf("add TrustProfile index finalizer to replacement: %w", err)
				}
			}
			index.Spec.SelectedProfileName = replacement.Name
			index.Spec.SelectedProfileUID = string(replacement.UID)
			if err := r.client.Update(ctx, index); err != nil {
				return fmt.Errorf("transition TrustProfileIndex: %w", err)
			}
		}
	}

	controllerutil.RemoveFinalizer(profile, TrustProfileIndexFinalizer)
	if err := r.client.Update(ctx, profile); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("remove TrustProfile index finalizer: %w", err)
	}
	return nil
}

func (r *TrustProfileIndexReconciler) replacementProfile(ctx context.Context, deleting *k8s.TrustProfile) (*k8s.TrustProfile, error) {
	profiles := &k8s.TrustProfileList{}
	if err := r.client.List(ctx, profiles, client.InNamespace(deleting.Namespace)); err != nil {
		return nil, fmt.Errorf("list TrustProfiles during index transition: %w", err)
	}
	var replacement *k8s.TrustProfile
	for i := range profiles.Items {
		candidate := &profiles.Items[i]
		if candidate.UID == deleting.UID || !candidate.DeletionTimestamp.IsZero() || candidate.Spec.AgentIncarnationUID != deleting.Spec.AgentIncarnationUID {
			continue
		}
		if candidate.UID == "" {
			return nil, fmt.Errorf("replacement TrustProfile %q lacks stable UID", candidate.Name)
		}
		if replacement != nil {
			return nil, fmt.Errorf("multiple live TrustProfiles match deleting profile's Agent incarnation")
		}
		replacement = candidate
	}
	return replacement, nil
}

func (r *TrustProfileIndexReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("trustprofile-index").
		For(&k8s.TrustProfile{}).
		Watches(&k8s.TrustProfileIndex{}, handler.EnqueueRequestsFromMapFunc(r.indexToProfile)).
		Watches(&k8s.Agent{}, handler.EnqueueRequestsFromMapFunc(r.agentToProfiles)).
		Complete(r)
}

func (r *TrustProfileIndexReconciler) indexToProfile(_ context.Context, obj client.Object) []reconcile.Request {
	index, ok := obj.(*k8s.TrustProfileIndex)
	if !ok || index.Spec.SelectedProfileName == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: index.Namespace, Name: index.Spec.SelectedProfileName}}}
}

func (r *TrustProfileIndexReconciler) agentToProfiles(ctx context.Context, obj client.Object) []reconcile.Request {
	profiles := &k8s.TrustProfileList{}
	if err := r.client.List(ctx, profiles, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range profiles.Items {
		profile := &profiles.Items[i]
		if profile.Spec.AgentRef.Name == obj.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(profile)})
		}
	}
	return requests
}

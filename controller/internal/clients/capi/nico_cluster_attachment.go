// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

func ensureNicoClusterAttachment(ctx context.Context, deps shared.MachineCreateDeps, node db.Node, nodeSet db.NodeSet, cluster db.Cluster, infra *unstructured.Unstructured) error {
	provider, err := nodeSet.CAPIProvider()
	if err != nil {
		return err
	}
	attachment := provider.NicoCluster
	if attachment == nil {
		return nil
	}
	if _, _, err := attachment.SpireServer(); err != nil {
		return err
	}
	if !deps.Config.NicoControllersEnabled {
		return fmt.Errorf("NICo attachment requires NICo identity and provider-ID controllers enabled in the retained manager")
	}
	if err := validateAttachedNicoMachine(node, cluster, infra); err != nil {
		return err
	}
	if attachment.SiteID == uuid.Nil || attachment.IdentitySecretName == "" {
		return fmt.Errorf("NICo attachment requires an explicit site and identity Secret")
	}
	owner := &clusterv1.Cluster{}
	if err := deps.K8sClient.Get(ctx, client.ObjectKey{Namespace: deps.Namespace, Name: cluster.Name}, owner); err != nil {
		return fmt.Errorf("read retained CAPI cluster: %w", err)
	}
	if owner.UID == "" || !owner.DeletionTimestamp.IsZero() || (owner.Spec.Paused != nil && *owner.Spec.Paused) ||
		owner.Spec.InfrastructureRef.APIGroup != nicov1.GroupVersion.Group || owner.Spec.InfrastructureRef.Kind != "ProvisionedCluster" ||
		owner.Spec.InfrastructureRef.Name != cluster.Name || deps.Config.ManagedByLabelValue == "" ||
		owner.Labels[constants.LabelKeyNkeManagedBy] != deps.Config.ManagedByLabelValue {
		return fmt.Errorf("NICo attachment requires the unpaused ProvisionedCluster owned by this retained manager")
	}
	if _, paused := owner.Annotations[clusterv1.PausedAnnotation]; paused {
		return fmt.Errorf("retained CAPI cluster is paused")
	}
	if err := checkAttachedMachineIdentity(ctx, deps.K8sClient, deps.Namespace, node, cluster); err != nil {
		return err
	}

	// CAPNICo resolves NicoCluster using Cluster.spec.infrastructureRef.name.
	// A same-named companion retains the primary ProvisionedCluster reference and
	// original Machines. Never force-apply this object or adopt another owner.
	expected := &nicov1.NicoCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: owner.Spec.InfrastructureRef.Name, Namespace: deps.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(owner, clusterv1.GroupVersion.WithKind("Cluster"))},
			Labels: map[string]string{
				constants.LabelKeyClusterID:    cluster.ID.String(),
				constants.LabelKeyNkeManagedBy: deps.Config.ManagedByLabelValue,
			},
		},
		Spec: nicov1.NicoClusterSpec{
			SiteID:      attachment.SiteID.String(),
			IdentityRef: corev1.LocalObjectReference{Name: attachment.IdentitySecretName},
		},
	}
	current := &nicov1.NicoCluster{}
	key := client.ObjectKeyFromObject(expected)
	if err := deps.K8sClient.Get(ctx, key, current); apierrors.IsNotFound(err) {
		if err := deps.K8sClient.Create(ctx, expected); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create companion NicoCluster: %w", err)
		}
		if err := deps.K8sClient.Get(ctx, key, current); err != nil {
			return fmt.Errorf("read companion NicoCluster after create: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("read companion NicoCluster: %w", err)
	}
	ref := metav1.GetControllerOf(current)
	if ref == nil || ref.UID != owner.UID || ref.Name != owner.Name || ref.Kind != "Cluster" ||
		ref.APIVersion != clusterv1.GroupVersion.String() || current.Spec != expected.Spec ||
		current.Labels[constants.LabelKeyClusterID] != cluster.ID.String() ||
		current.Labels[constants.LabelKeyNkeManagedBy] != deps.Config.ManagedByLabelValue || !current.DeletionTimestamp.IsZero() {
		return fmt.Errorf("companion NicoCluster conflicts with retained cluster identity, owner or immutable site/credentials")
	}
	if !current.Status.Ready || current.Status.Initialization.Provisioned == nil || !*current.Status.Initialization.Provisioned {
		return fmt.Errorf("waiting for companion NicoCluster credentials and site readiness")
	}
	if _, paused := current.Annotations[clusterv1.PausedAnnotation]; paused {
		return fmt.Errorf("companion NicoCluster is paused")
	}
	return nil
}

func validateAttachedNicoMachine(node db.Node, cluster db.Cluster, infra *unstructured.Unstructured) error {
	if cluster.ProviderType != db.ProviderTypeCAPI || cluster.ID == uuid.Nil || node.ClusterID != cluster.ID || node.ID == uuid.Nil ||
		!node.IsManagedByControlPlaneController() || node.ControlPlaneConfigurationID == uuid.Nil || !node.SpiffeEnabled {
		return fmt.Errorf("NICo attachment requires a pinned SPIFFE-enabled control-plane node in the retained CAPI cluster")
	}
	if err := node.ValidateSecurityProfile(); err != nil {
		return err
	}
	if infra.GetAPIVersion() != nicov1.GroupVersion.String() || infra.GetKind() != "NicoMachine" {
		return fmt.Errorf("NICo attachment requires a NicoMachineTemplate")
	}
	var machine nicov1.NicoMachine
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(infra.Object, &machine); err != nil {
		return fmt.Errorf("decode attached NicoMachine: %w", err)
	}
	if machine.Spec.MachineID == "" || machine.Spec.InstanceTypeID != "" || machine.Spec.ProviderID != "" || node.FailureDomain != "" {
		return fmt.Errorf("NICo attachment requires an explicit unallocated machineID, no instanceTypeID/providerID or failure domain")
	}
	return nil
}

func checkAttachedMachineIdentity(ctx context.Context, k8s client.Client, namespace string, node db.Node, cluster db.Cluster) error {
	key := client.ObjectKey{Namespace: namespace, Name: node.Name}
	machine := &clusterv1.Machine{}
	if err := k8s.Get(ctx, key, machine); err == nil {
		if machine.Annotations[constants.MachineAnnotationKeyHostID] != node.ID.String() || machine.Spec.ClusterName != cluster.Name ||
			machine.Spec.InfrastructureRef.Kind != "NicoMachine" || machine.Spec.InfrastructureRef.APIGroup != nicov1.GroupVersion.Group ||
			machine.Spec.InfrastructureRef.Name != node.Name || !machine.DeletionTimestamp.IsZero() {
			return fmt.Errorf("existing CAPI Machine conflicts with attached node identity")
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("read existing CAPI Machine: %w", err)
	}
	nicoMachine := &nicov1.NicoMachine{}
	if err := k8s.Get(ctx, key, nicoMachine); err == nil {
		if nicoMachine.Spec.Labels[constants.LabelKeyNodeID] != node.ID.String() ||
			nicoMachine.Spec.Labels[constants.LabelKeyClusterID] != cluster.ID.String() || !nicoMachine.DeletionTimestamp.IsZero() {
			return fmt.Errorf("existing NicoMachine conflicts with attached node identity")
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("read existing NicoMachine: %w", err)
	}
	return nil
}

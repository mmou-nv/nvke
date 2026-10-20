// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/nodes"
)

// validateNicoClusterAttachmentUpdate limits the mixed-provider path to scale-out
// under the existing manager. It never imports or rewrites existing node records.
func (s *clusterService) validateNicoClusterAttachmentUpdate(ctx context.Context, tx db.Tx, cluster db.Cluster, spec db.ControlPlaneConfigSpec, previousDesiredCount int) error {
	if spec.Capi == nil || spec.Capi.NicoCluster == nil {
		return nil
	}
	attachment := spec.Capi.NicoCluster
	if err := s.providers.require(config.InfraProviderNico); err != nil {
		return newBadRequestErrorf("%s", err)
	}
	if attachment.SiteID == uuid.Nil || len(validation.IsDNS1123Subdomain(attachment.IdentitySecretName)) != 0 {
		return newBadRequestErrorf("nicoCluster requires a nonzero siteID and a valid identitySecretName")
	}
	if _, _, err := attachment.SpireServer(); err != nil {
		return newBadRequestErrorf("%s", err)
	}
	if cluster.ProviderType != db.ProviderTypeCAPI || cluster.Capi == nil || !attachmentManifestIs(cluster.Capi.InfraCluster, "ProvisionedCluster") {
		return newBadRequestErrorf("nicoCluster attachment requires an existing ProvisionedCluster")
	}
	if cluster.EnableCPNodeRecycling || cluster.ControlPlaneConfig.MaxSurge != 0 ||
		cluster.ControlPlaneConfig.DesiredCount < previousDesiredCount || cluster.ControlPlaneConfig.DesiredCount > previousDesiredCount+1 ||
		spec.KubernetesVersion != cluster.ControlPlaneConfig.KubernetesVersion {
		return newBadRequestErrorf("nicoCluster attachment supports scale-out one node at a time only, with recycling and surge disabled and no Kubernetes upgrade")
	}
	if cluster.Conditions.Provisioned.Status != db.ConditionStatusTrue || cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
		return newBadRequestErrorf("nicoCluster attachment requires a provisioned, non-deleting cluster")
	}
	if len(spec.Capi.InfraMachineTemplatePreferences) != 1 || !attachmentManifestIs(spec.Capi.InfraMachineTemplatePreferences[0], "NicoMachineTemplate") {
		return newBadRequestErrorf("nicoCluster attachment requires exactly one NicoMachineTemplate")
	}
	var template nicov1.NicoMachineTemplate
	if err := json.Unmarshal(spec.Capi.InfraMachineTemplatePreferences[0], &template); err != nil {
		return newBadRequestErrorf("invalid attached NicoMachineTemplate: %s", err)
	}
	machine := template.Spec.Template.Spec
	if machine.MachineID == "" || machine.InstanceTypeID != "" || machine.ProviderID != "" {
		return newBadRequestErrorf("nicoCluster attachment requires an explicit machineID and no instanceTypeID or providerID")
	}
	if machine.IpxeScript == "" || !machine.CloudInitInjectHostname || machine.AllowUnhealthyMachine || machine.VPCID == "" || len(machine.Interfaces) == 0 {
		return newBadRequestErrorf("nicoCluster attachment requires ipxeScript, cloudInitInjectHostname, vpcID and interfaces; allowUnhealthyMachine is not supported")
	}
	alreadyAttached := cluster.ControlPlaneConfig.Capi != nil && cluster.ControlPlaneConfig.Capi.NicoCluster != nil
	if alreadyAttached && cluster.ControlPlaneConfig.DesiredCount == previousDesiredCount {
		// Only future-node defaults change here. Existing nodes, including NICo
		// nodes still provisioning, resolve their immutable per-node snapshots.
		return nil
	}

	// Legacy nodes without a snapshot fall back to the current cluster template.
	// Refuse attachment rather than let a retry reprovision such a node through NICo.
	controlPlane := true
	existing, err := tx.ListNodes(ctx, db.NodeFilters{ClusterID: &cluster.ID, ControlPlane: &controlPlane})
	if err != nil {
		return fmt.Errorf("list control-plane nodes before NICo attachment: %w", err)
	}
	if len(existing) != previousDesiredCount || len(existing) == 0 {
		return newBadRequestErrorf("nicoCluster attachment requires converged control-plane capacity")
	}
	for _, node := range existing {
		if !node.IsManagedByControlPlaneController() || node.ControlPlaneConfigurationID == uuid.Nil ||
			nodes.IsDeleting(node) || nodes.IsDeleteRequested(node) || node.Conditions.KubernetesReady.Status != db.ConditionStatusTrue ||
			!node.SpiffeEnabled || node.ValidateSecurityProfile() != nil {
			return newBadRequestErrorf("node %s must be ready with a pinned configuration and valid SPIFFE security profile before NICo attachment", node.ID)
		}
		row, err := tx.GetControlPlaneConfiguration(ctx, node.ControlPlaneConfigurationID)
		if err != nil {
			return fmt.Errorf("read original node configuration: %w", err)
		}
		if row.ClusterID != cluster.ID || row.Config.Capi == nil || len(row.Config.Capi.InfraMachineTemplatePreferences) != 1 {
			return newBadRequestErrorf("node %s must remain pinned to its own infrastructure template", node.ID)
		}
		raw := row.Config.Capi.InfraMachineTemplatePreferences[0]
		if !attachmentManifestIs(raw, "ProvisionedMachineTemplate") && !(alreadyAttached && attachmentManifestIs(raw, "NicoMachineTemplate")) {
			return newBadRequestErrorf("node %s must remain pinned to its original infrastructure provider", node.ID)
		}
	}
	return nil
}

func attachmentManifestIs(raw json.RawMessage, kind string) bool {
	var meta metav1.TypeMeta
	return json.Unmarshal(raw, &meta) == nil && meta.APIVersion == nicov1.GroupVersion.String() && meta.Kind == kind
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	"github.com/dsx-ai-factory/nke/controller/api"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/nodes"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func nicoAttachmentAPIInput(t *testing.T) (db.Cluster, []db.Node, *api.UpdateClusterControlPlaneConfig) {
	t.Helper()
	cluster := db.Cluster{ID: uuid.New(), Name: "retained", ProviderType: db.ProviderTypeCAPI, KubernetesVersion: "1.34.2",
		ControlPlaneVIP: "192.0.2.10", EtcdEncrypted: true,
		Capi:               &db.CAPIClusterProvider{InfraCluster: json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1alpha1","kind":"ProvisionedCluster","spec":{}}`)},
		Conditions:         db.ClusterConditions{Provisioned: db.NewCondition(true, "ready")},
		ControlPlaneConfig: db.ControlPlaneConfig{ConfigurationID: uuid.New(), DesiredCount: 3},
	}
	cluster.ControlPlaneConfig.KubernetesVersion = cluster.KubernetesVersion
	cluster.ControlPlaneConfig.SetCapi(db.CAPIControlPlaneProvider{
		SpireAttestationMethod:          db.SpireAttestationMethodTPM,
		InfraMachineTemplatePreferences: []json.RawMessage{json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1alpha1","kind":"ProvisionedMachineTemplate","spec":{"template":{"spec":{}}}}`)},
	})
	existing := make([]db.Node, 3)
	for i := range existing {
		existing[i] = db.Node{ID: uuid.New(), ClusterID: cluster.ID, ControlPlane: true,
			ControlPlaneConfigurationID: cluster.ControlPlaneConfig.ConfigurationID,
			SpiffeEnabled:               true, SpireAttestationMethod: db.SpireAttestationMethodTPM,
			NodeAgentExternalMTLSEnabled: true, NodeAgentTLSJWTEnabled: true,
			Conditions: db.NodeConditions{KubernetesReady: db.NewCondition(true, "ready")},
		}
	}
	var template map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1alpha1","kind":"NicoMachineTemplate","spec":{"template":{"spec":{"machineID":"fourth-host","vpcID":"vpc-test","interfaces":[{"subnetID":"subnet-test"}],"ipxeScript":"#!ipxe\nchain https://boot.example.test/node.ipxe","cloudInitInjectHostname":true}}}}`), &template))
	req := &api.UpdateClusterControlPlaneConfig{DesiredCount: ptr.To(4), Infra: &api.InfraControlPlaneProviderUpdate{
		NicoCluster: &api.NicoClusterAttachment{SiteID: uuid.New(), IdentitySecretName: "nico-identity", SpireServerEndpoint: "192.0.2.11:30081"}, MachineTemplate: template,
	}}
	return cluster, existing, req
}

func TestNicoAttachmentPatchPreservesOriginalNodes(t *testing.T) {
	cluster, existing, req := nicoAttachmentAPIInput(t)
	originalCluster := cluster
	originalNodes, err := json.Marshal(existing)
	require.NoError(t, err)
	originalRow := db.ControlPlaneConfiguration{ID: cluster.ControlPlaneConfig.ConfigurationID, ClusterID: cluster.ID, Config: cluster.ControlPlaneConfig.ControlPlaneConfigSpec}
	newConfigID := uuid.New()
	tx := &testutil.MockTx{}
	tx.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
		return f.ClusterID != nil && *f.ClusterID == cluster.ID && f.ControlPlane != nil && *f.ControlPlane
	})).Return(existing, nil).Once()
	tx.On("GetControlPlaneConfiguration", mock.Anything, originalRow.ID).Return(originalRow, nil)
	tx.On("CreateControlPlaneConfiguration", mock.Anything, mock.MatchedBy(func(row *db.ControlPlaneConfiguration) bool {
		return row.ClusterID == cluster.ID && row.Config.Capi.NicoCluster.SiteID == req.Infra.NicoCluster.SiteID &&
			row.Config.Capi.NicoCluster.SpireServerEndpoint == req.Infra.NicoCluster.SpireServerEndpoint &&
			row.Config.Capi.SpireAttestationMethod == originalRow.Config.Capi.SpireAttestationMethod
	})).Return(db.ControlPlaneConfiguration{ID: newConfigID, ClusterID: cluster.ID}, nil).Once()
	svc := clusterService{useControlPlaneController: true}
	require.NoError(t, svc.applyClusterControlPlaneConfigUpdate(t.Context(), tx, &cluster, req))
	require.Equal(t, originalCluster.ID, cluster.ID)
	require.Equal(t, originalCluster.Capi, cluster.Capi)
	require.Equal(t, originalCluster.ProviderType, cluster.ProviderType)
	require.Equal(t, originalCluster.EtcdEncrypted, cluster.EtcdEncrypted)
	require.Equal(t, 4, cluster.ControlPlaneConfig.DesiredCount)
	require.Equal(t, newConfigID, cluster.ControlPlaneConfig.ConfigurationID)
	require.False(t, db.IsControlPlaneRolloutRequired(originalRow.Config, cluster.ControlPlaneConfig.ControlPlaneConfigSpec))
	response := apiInfraControlPlaneProviderFromDB(cluster.ControlPlaneConfig.Capi)
	require.Equal(t, req.Infra.NicoCluster, response.NicoCluster)

	// Resolve an original node AFTER the cluster points at the NICo snapshot.
	tx.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
	for _, node := range existing {
		set, _, err := nodes.GetNodeContext(t.Context(), tx, node)
		require.NoError(t, err)
		require.Nil(t, set.Capi.NicoCluster)
		require.Equal(t, originalRow.Config.Capi.InfraMachineTemplatePreferences[0], set.Capi.InfraMachineTemplate)
	}
	set, pool := nodes.ControlPlaneConfigNodeContext(cluster, db.Node{}, cluster.ControlPlaneConfig.ControlPlaneConfigSpec)
	require.Equal(t, cluster.ControlPlaneConfig.Capi.NicoCluster, set.Capi.NicoCluster)
	set.Capi.NicoCluster.IdentitySecretName = "mutated-copy"
	require.Equal(t, "nico-identity", pool.Capi.NicoCluster.IdentitySecretName)
	require.Equal(t, "nico-identity", cluster.ControlPlaneConfig.Capi.NicoCluster.IdentitySecretName)
	currentNodes, err := json.Marshal(existing)
	require.NoError(t, err)
	require.Equal(t, originalNodes, currentNodes)
	tx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	tx.AssertExpectations(t)
}

func TestNicoAttachmentPatchRejectsUnsafeExpansion(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*db.Cluster, *[]db.Node, *api.UpdateClusterControlPlaneConfig)
		want   string
	}{
		{"zero site", func(_ *db.Cluster, _ *[]db.Node, r *api.UpdateClusterControlPlaneConfig) {
			r.Infra.NicoCluster.SiteID = uuid.Nil
		}, "nonzero siteID"},
		{"missing identity", func(_ *db.Cluster, _ *[]db.Node, r *api.UpdateClusterControlPlaneConfig) {
			r.Infra.NicoCluster.IdentitySecretName = ""
		}, "identitySecretName"},
		{"invalid identity", func(_ *db.Cluster, _ *[]db.Node, r *api.UpdateClusterControlPlaneConfig) {
			r.Infra.NicoCluster.IdentitySecretName = "other/secret"
		}, "identitySecretName"},
		{"missing SPIRE endpoint", func(_ *db.Cluster, _ *[]db.Node, r *api.UpdateClusterControlPlaneConfig) {
			r.Infra.NicoCluster.SpireServerEndpoint = ""
		}, "spireServerEndpoint"},
		{"invalid SPIRE endpoint", func(_ *db.Cluster, _ *[]db.Node, r *api.UpdateClusterControlPlaneConfig) {
			r.Infra.NicoCluster.SpireServerEndpoint = "https://spire.example.test:30081"
		}, "spireServerEndpoint"},
		{"SPIRE endpoint mutation without scaling", func(c *db.Cluster, _ *[]db.Node, r *api.UpdateClusterControlPlaneConfig) {
			c.ControlPlaneConfig.Capi.NicoCluster = &db.NicoClusterAttachment{SiteID: r.Infra.NicoCluster.SiteID,
				IdentitySecretName: r.Infra.NicoCluster.IdentitySecretName, SpireServerEndpoint: "192.0.2.12:30081"}
			r.DesiredCount = ptr.To(c.ControlPlaneConfig.DesiredCount)
		}, "immutable"},
		{"recycling", func(c *db.Cluster, _ *[]db.Node, _ *api.UpdateClusterControlPlaneConfig) {
			c.EnableCPNodeRecycling = true
		}, "recycling"},
		{"surge", func(_ *db.Cluster, _ *[]db.Node, r *api.UpdateClusterControlPlaneConfig) { r.MaxSurge = ptr.To(1) }, "surge"},
		{"scale down", func(_ *db.Cluster, _ *[]db.Node, r *api.UpdateClusterControlPlaneConfig) { r.DesiredCount = ptr.To(2) }, "scale-out"},
		{"multiple new nodes", func(_ *db.Cluster, _ *[]db.Node, r *api.UpdateClusterControlPlaneConfig) { r.DesiredCount = ptr.To(5) }, "one node"},
		{"upgrade", func(c *db.Cluster, _ *[]db.Node, _ *api.UpdateClusterControlPlaneConfig) {
			c.KubernetesVersion = "1.35.0"
		}, "upgrade"},
		{"unprovisioned", func(c *db.Cluster, _ *[]db.Node, _ *api.UpdateClusterControlPlaneConfig) {
			c.Conditions.Provisioned = db.NewCondition(false, "pending")
		}, "provisioned"},
		{"capacity not converged", func(_ *db.Cluster, n *[]db.Node, _ *api.UpdateClusterControlPlaneConfig) { *n = (*n)[:2] }, "converged"},
		{"unpinned original", func(_ *db.Cluster, n *[]db.Node, _ *api.UpdateClusterControlPlaneConfig) {
			(*n)[0].ControlPlaneConfigurationID = uuid.Nil
		}, "pinned"},
		{"unready original", func(_ *db.Cluster, n *[]db.Node, _ *api.UpdateClusterControlPlaneConfig) {
			(*n)[0].Conditions.KubernetesReady = db.NewCondition(false, "not ready")
		}, "ready"},
		{"insecure original", func(_ *db.Cluster, n *[]db.Node, _ *api.UpdateClusterControlPlaneConfig) {
			(*n)[0].SpiffeEnabled = false
		}, "SPIFFE"},
		{"identity change", func(c *db.Cluster, _ *[]db.Node, r *api.UpdateClusterControlPlaneConfig) {
			c.ControlPlaneConfig.Capi.NicoCluster = &db.NicoClusterAttachment{SiteID: r.Infra.NicoCluster.SiteID, IdentitySecretName: "original-identity"}
		}, "immutable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cluster, existing, req := nicoAttachmentAPIInput(t)
			test.mutate(&cluster, &existing, req)
			tx := &testutil.MockTx{}
			tx.On("ListNodes", mock.Anything, mock.Anything).Return(existing, nil).Maybe()
			svc := clusterService{useControlPlaneController: true}
			err := svc.applyClusterControlPlaneConfigUpdate(t.Context(), tx, &cluster, req)
			require.ErrorContains(t, err, test.want)
			tx.AssertNotCalled(t, "CreateControlPlaneConfiguration", mock.Anything, mock.Anything)
		})
	}
}

func TestNicoAttachmentRejectsInitialCreate(t *testing.T) {
	_, _, req := nicoAttachmentAPIInput(t)
	err := validateCreateCAPIControlPlaneConfig(&api.CreateClusterControlPlaneConfig{DesiredCount: 3,
		Infra: &api.InfraControlPlaneProvider{NicoCluster: req.Infra.NicoCluster, MachineTemplate: req.Infra.MachineTemplate}})
	require.ErrorContains(t, err, "only supported through PATCH")
}

func TestNicoAttachmentUnchangedCountPreservesEveryNodeSnapshot(t *testing.T) {
	cluster, originals, initial := nicoAttachmentAPIInput(t)
	originalRow := db.ControlPlaneConfiguration{ID: cluster.ControlPlaneConfig.ConfigurationID, ClusterID: cluster.ID, Config: cluster.ControlPlaneConfig.ControlPlaneConfigSpec}
	current := db.ConfigSpecFromFull(cluster.ControlPlaneConfig)
	svc := clusterService{useControlPlaneController: true}
	require.NoError(t, svc.applyClusterLevelControlPlaneSpecUpdate(t.Context(), &cluster, initial, &current))
	cluster.ControlPlaneConfig.ControlPlaneConfigSpec = current
	cluster.ControlPlaneConfig.ConfigurationID = uuid.New()
	nicoRow := db.ControlPlaneConfiguration{ID: cluster.ControlPlaneConfig.ConfigurationID, ClusterID: cluster.ID, Config: current}
	fourth := originals[0]
	fourth.ID = uuid.New()
	fourth.ControlPlaneConfigurationID = nicoRow.ID
	fourth.Conditions.KubernetesReady = db.NewCondition(false, "still provisioning")
	existing := append(originals, fourth)
	before, err := json.Marshal(existing)
	require.NoError(t, err)
	_, _, changed := nicoAttachmentAPIInput(t)
	changed.Infra.NicoCluster = nil // omitted attachment preserves the immutable binding
	templateSpec := changed.Infra.MachineTemplate["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
	templateSpec["machineID"] = "future-fifth-host"
	changed.Infra.PreKubeadmCommands = []string{"echo future-node-only"}
	changed.Infra.BootstrapFiles = []api.CloudInitFile{{Path: "/etc/future-node-only", Content: "future configuration"}}
	newID := uuid.New()
	tx := &testutil.MockTx{}
	tx.On("GetControlPlaneConfiguration", mock.Anything, originalRow.ID).Return(originalRow, nil)
	tx.On("GetControlPlaneConfiguration", mock.Anything, nicoRow.ID).Return(nicoRow, nil)
	tx.On("CreateControlPlaneConfiguration", mock.Anything, mock.Anything).Return(db.ControlPlaneConfiguration{ID: newID, ClusterID: cluster.ID}, nil).Once()
	require.NoError(t, svc.applyClusterControlPlaneConfigUpdate(t.Context(), tx, &cluster, changed))
	require.Equal(t, 4, cluster.ControlPlaneConfig.DesiredCount)
	require.Equal(t, newID, cluster.ControlPlaneConfig.ConfigurationID)
	require.Equal(t, nicoRow.Config.Capi.NicoCluster, cluster.ControlPlaneConfig.Capi.NicoCluster)
	require.False(t, cluster.EnableCPNodeRecycling)
	tx.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
	for _, node := range existing {
		set, _, err := nodes.GetNodeContext(t.Context(), tx, node)
		require.NoError(t, err)
		expected := originalRow.Config
		if node.ID == fourth.ID {
			expected = nicoRow.Config
		}
		require.Equal(t, expected.Capi.InfraMachineTemplatePreferences[0], set.Capi.InfraMachineTemplate)
		require.Equal(t, expected.Capi.BootstrapFiles, set.Capi.BootstrapFiles)
		require.Equal(t, expected.Capi.PreKubeadmCommands, set.Capi.PreKubeadmCommands)
		require.Equal(t, expected.Capi.NicoCluster, set.Capi.NicoCluster)
		require.False(t, db.IsControlPlaneRolloutRequired(expected, cluster.ControlPlaneConfig.ControlPlaneConfigSpec))
	}
	after, err := json.Marshal(existing)
	require.NoError(t, err)
	require.Equal(t, before, after)
	tx.AssertNotCalled(t, "ListNodes", mock.Anything, mock.Anything)
	tx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	tx.AssertExpectations(t)
}

func TestNicoAttachmentUnchangedCountRejectsLifecycleChanges(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*db.Cluster, *api.UpdateClusterControlPlaneConfig)
	}{
		{"recycling", func(c *db.Cluster, _ *api.UpdateClusterControlPlaneConfig) { c.EnableCPNodeRecycling = true }},
		{"surge", func(_ *db.Cluster, r *api.UpdateClusterControlPlaneConfig) { r.MaxSurge = ptr.To(1) }},
		{"upgrade", func(c *db.Cluster, _ *api.UpdateClusterControlPlaneConfig) { c.KubernetesVersion = "1.35.0" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cluster, _, initial := nicoAttachmentAPIInput(t)
			spec := db.ConfigSpecFromFull(cluster.ControlPlaneConfig)
			svc := clusterService{useControlPlaneController: true}
			require.NoError(t, svc.applyClusterLevelControlPlaneSpecUpdate(t.Context(), &cluster, initial, &spec))
			cluster.ControlPlaneConfig.ControlPlaneConfigSpec = spec
			req := &api.UpdateClusterControlPlaneConfig{DesiredCount: ptr.To(4)}
			test.mutate(&cluster, req)
			tx := &testutil.MockTx{}
			require.ErrorContains(t, svc.applyClusterControlPlaneConfigUpdate(t.Context(), tx, &cluster, req), "scale-out one node at a time only")
			tx.AssertNotCalled(t, "CreateControlPlaneConfiguration", mock.Anything, mock.Anything)
		})
	}
}

func TestNicoAttachmentTopLevelRecyclingPatchRejected(t *testing.T) {
	cluster, _, initial := nicoAttachmentAPIInput(t)
	cluster.ControlPlaneConfig.Capi.NicoCluster = &db.NicoClusterAttachment{SiteID: initial.Infra.NicoCluster.SiteID, IdentitySecretName: initial.Infra.NicoCluster.IdentitySecretName}
	database := &testutil.MockDB{}
	tx := &testutil.MockTx{}
	database.MockTx = tx
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
	tx.On("GetClusterForUpdate", mock.Anything, cluster.ID).Return(cluster, nil).Once()
	svc := clusterService{db: database, useControlPlaneController: true}
	response, err := svc.UpdateCluster(t.Context(), api.UpdateClusterRequestObject{ClusterID: cluster.ID,
		Body: &api.UpdateClusterRequest{EnableCPNodeRecycling: ptr.To(true)}})
	require.NoError(t, err)
	require.IsType(t, api.UpdateCluster400JSONResponse{}, response)
	require.Contains(t, response.(api.UpdateCluster400JSONResponse).Message, "recycling is not supported")
	tx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
	tx.AssertNotCalled(t, "CreateControlPlaneConfiguration", mock.Anything, mock.Anything)
	database.AssertExpectations(t)
	tx.AssertExpectations(t)
}

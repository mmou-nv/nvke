// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestNicoAttachmentScaleThreeToFour(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New(), Name: "retained", ProviderType: db.ProviderTypeCAPI,
		ControlPlaneConfig: db.ControlPlaneConfig{ConfigurationID: uuid.New(), DesiredCount: 4}}
	cluster.ControlPlaneConfig.SetCapi(db.CAPIControlPlaneProvider{NicoCluster: &db.NicoClusterAttachment{SiteID: uuid.New(), IdentitySecretName: "nico-identity"}})
	originals := []db.Node{
		{ID: uuid.New(), Name: "original-3", ClusterID: cluster.ID, ControlPlane: true, ControlPlaneConfigurationID: uuid.New()},
		{ID: uuid.New(), Name: "original-1", ClusterID: cluster.ID, ControlPlane: true, ControlPlaneConfigurationID: uuid.New()},
		{ID: uuid.New(), Name: "original-2", ClusterID: cluster.ID, ControlPlane: true, ControlPlaneConfigurationID: uuid.New()},
	}
	before := append([]db.Node(nil), originals...)
	tx := &testutil.MockTx{}
	tx.On("ListClusterNodeNames", mock.Anything, cluster.ID).Return([]string{"original-3", "original-1", "original-2"}, nil).Once()
	tx.On("CreateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		return node.ClusterID == cluster.ID && node.ControlPlaneConfigurationID == cluster.ControlPlaneConfig.ConfigurationID &&
			node.IsManagedByControlPlaneController() && node.SpiffeEnabled && node.NodeAgentExternalMTLSEnabled && node.NodeAgentTLSJWTEnabled &&
			node.SpireAttestationMethod == db.SpireAttestationMethodDSXIMDS && node.ValidateSecurityProfile() == nil
	})).Return(db.Node{Name: "new-fourth"}, nil).Once()
	r := &reconciler{db: tx, logger: slog.Default(), cfg: config.Config{
		ProvisionControlPlaneWithControlPlaneController: true, SpiffeEnabled: true, NodeAgentExternalMTLSEnabled: true,
		NodeAgent: config.NodeAgentConfig{SpireAttestationMethod: config.SpireAttestationMethodDSXIMDS},
	}}
	r.cfg.NodeAgent.TLSJWT.Enabled = true
	require.NoError(t, r.reconcileControlPlaneScaleUp(t.Context(), cluster, &controlPlane{nodes: originals}, domainAllocator{}))
	require.Equal(t, before, originals)
	tx.AssertExpectations(t)
	tx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)

	// Automatic replacement is explicitly outside this scale-out contract.
	requeue, err := r.remediateMachines(t.Context(), cluster, nil)
	require.NoError(t, err)
	require.False(t, requeue)
}

func TestNicoAttachmentRejectsInsecureNodeCreation(t *testing.T) {
	cluster := db.Cluster{ControlPlaneConfig: db.ControlPlaneConfig{ConfigurationID: uuid.New()}}
	cluster.ControlPlaneConfig.SetCapi(db.CAPIControlPlaneProvider{NicoCluster: &db.NicoClusterAttachment{}})
	r := &reconciler{db: &testutil.MockTx{}, logger: slog.Default()}
	_, err := r.createPendingNode(t.Context(), cluster, nil, "")
	require.ErrorContains(t, err, "SPIFFE-enabled")
}

func TestNicoAttachmentScaleSequentialCountsPreservePriorNodes(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New(), Name: "retained", ProviderType: db.ProviderTypeCAPI}
	cluster.ControlPlaneConfig.SetCapi(db.CAPIControlPlaneProvider{NicoCluster: &db.NicoClusterAttachment{
		SiteID: uuid.New(), IdentitySecretName: "nico-identity",
	}})
	var prior []db.Node
	for i := range 3 {
		prior = append(prior, db.Node{ID: uuid.New(), Name: fmt.Sprintf("original-%d", i+1),
			ClusterID: cluster.ID, ControlPlane: true, ControlPlaneConfigurationID: uuid.New(),
			SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodTPM})
	}
	for target := 4; target <= 20; target++ {
		t.Run(fmt.Sprintf("%d-to-%d", target-1, target), func(t *testing.T) {
			cluster.ControlPlaneConfig.ConfigurationID = uuid.New()
			cluster.ControlPlaneConfig.DesiredCount = target
			before, err := json.Marshal(prior)
			require.NoError(t, err)
			var names []string
			for _, node := range prior {
				names = append(names, node.Name)
			}
			var pending db.Node
			tx := &testutil.MockTx{}
			tx.On("ListClusterNodeNames", mock.Anything, cluster.ID).Return(names, nil).Once()
			tx.On("CreateNode", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
				pending = *args.Get(1).(*db.Node)
				require.Equal(t, cluster.ID, pending.ClusterID)
				require.Equal(t, cluster.ControlPlaneConfig.ConfigurationID, pending.ControlPlaneConfigurationID)
				require.True(t, pending.IsManagedByControlPlaneController())
				require.True(t, pending.SpiffeEnabled)
				require.Equal(t, db.SpireAttestationMethodTPM, pending.SpireAttestationMethod)
				require.NoError(t, pending.ValidateSecurityProfile())
				require.NotContains(t, names, pending.Name)
			}).Return(db.Node{Name: fmt.Sprintf("created-%d", target)}, nil).Once()
			r := &reconciler{db: tx, logger: slog.Default(), cfg: config.Config{
				ProvisionControlPlaneWithControlPlaneController: true, SpiffeEnabled: true,
				NodeAgent: config.NodeAgentConfig{SpireAttestationMethod: config.SpireAttestationMethodTPM},
			}}
			require.NoError(t, r.reconcileControlPlaneScaleUp(t.Context(), cluster, &controlPlane{nodes: prior}, domainAllocator{}))
			after, err := json.Marshal(prior)
			require.NoError(t, err)
			require.Equal(t, before, after)
			tx.AssertExpectations(t)
			tx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
			tx.AssertNotCalled(t, "DeleteNode", mock.Anything, mock.Anything)

			// Carry the observed new binding into the next count, without replacing prior nodes.
			pending.ID = uuid.New()
			pending.SetNico(db.NicoNodeProvider{ForgeInstanceID: uuid.NewString(), ForgeMachineID: fmt.Sprintf("host-%d", target)})
			prior = append(prior, pending)
			require.NoError(t, r.reconcileControlPlaneScaleUp(t.Context(), cluster, &controlPlane{nodes: prior}, domainAllocator{}))
			tx.AssertNumberOfCalls(t, "CreateNode", 1)
		})
	}
	require.Len(t, prior, 20)
}

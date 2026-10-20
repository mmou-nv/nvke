// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

func TestNicoAttachmentWaitForPinnedHost(t *testing.T) {
	for _, test := range []struct {
		name, observedHost, instanceID    string
		wrongAnnotation, existingConflict bool
		wantError                         bool
	}{
		{name: "matching host", observedHost: "expected-host", instanceID: "new-instance"},
		{name: "wrong recovered host", observedHost: "different-host", instanceID: "new-instance", wantError: true},
		{name: "host not observed", instanceID: "new-instance", wantError: true},
		{name: "instance not observed", observedHost: "expected-host", wantError: true},
		{name: "wrong node annotation", observedHost: "expected-host", instanceID: "new-instance", wrongAnnotation: true, wantError: true},
		{name: "existing instance conflict", observedHost: "expected-host", instanceID: "new-instance", existingConflict: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := db.Node{ID: uuid.New(), Name: "worker-0", ClusterID: uuid.New(), ControlPlane: true,
				ControlPlaneConfigurationID: uuid.New(), SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodTPM,
				NodeAgentExternalMTLSEnabled: true, NodeAgentTLSJWTEnabled: true}
			if test.existingConflict {
				node.Nico = &db.NicoNodeProvider{ForgeInstanceID: "original-instance", ForgeMachineID: "expected-host"}
			}
			cluster := db.Cluster{ID: node.ClusterID, Name: "cluster-a", ProviderType: db.ProviderTypeCAPI}
			cluster.ControlPlaneConfig.SetCapi(db.CAPIControlPlaneProvider{
				NicoCluster:                     &db.NicoClusterAttachment{SiteID: uuid.New(), IdentitySecretName: "nico-identity"},
				InfraMachineTemplatePreferences: []json.RawMessage{json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1alpha1","kind":"NicoMachineTemplate","spec":{"template":{"spec":{"machineID":"expected-host"}}}}`)},
			})
			machine := testCAPIMachineWithNicoInfraRef(clusterv1.MachineAddresses{{Type: clusterv1.MachineInternalIP, Address: "192.0.2.14"}})
			machine.Annotations = map[string]string{constants.MachineAnnotationKeyHostID: node.ID.String()}
			if test.wrongAnnotation {
				machine.Annotations[constants.MachineAnnotationKeyHostID] = uuid.NewString()
			}
			infra := testNicoMachineForCAPIMachineReady(nicoMachineRotationParameters{MachineID: test.observedHost, InstanceID: test.instanceID})
			k8s := newCAPIMachineReadyClient(t, machine, infra)
			database, tx := newCAPIMachineReadyDB(t, node, cluster)
			database.On("GetControlPlaneConfiguration", mock.Anything, node.ControlPlaneConfigurationID).Return(db.ControlPlaneConfiguration{
				ID: node.ControlPlaneConfigurationID, ClusterID: cluster.ID, Config: cluster.ControlPlaneConfig.ControlPlaneConfigSpec}, nil)
			database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
			tx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil)
			var recorded []db.Node
			tx.On("UpdateNode", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
				recorded = append(recorded, *args.Get(1).(*db.Node))
			}).Return(node, nil)
			ip, err := executeWaitForCAPIMachineReadyActivity(t, &Activities{db: database, k8sClient: k8s}, WaitForCAPIMachineReadyInput{NodeID: node.ID})
			if test.wantError {
				require.Error(t, err)
				require.Empty(t, ip)
				for _, update := range recorded {
					require.Empty(t, update.IP)
				}
			} else {
				require.NoError(t, err)
				require.Equal(t, "192.0.2.14", ip)
				require.NotEmpty(t, recorded)
				last := recorded[len(recorded)-1]
				require.Equal(t, test.instanceID, last.Nico.ForgeInstanceID)
				require.Equal(t, test.observedHost, last.Nico.ForgeMachineID)
				require.True(t, last.SpiffeEnabled && last.NodeAgentExternalMTLSEnabled && last.NodeAgentTLSJWTEnabled)
			}
			database.AssertExpectations(t)
			tx.AssertExpectations(t)
		})
	}
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/api"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestNodeSecurityProfileExport(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		spiffe, external, jwt bool
		method                db.SpireAttestationMethod
	}{
		{"SPIFFE TPM", true, false, false, db.SpireAttestationMethodTPM},
		{"SPIFFE join token", true, false, false, db.SpireAttestationMethodJoinToken},
		{"SPIFFE DSX IMDS", true, false, false, db.SpireAttestationMethodDSXIMDS},
		{"external mTLS", false, true, false, db.SpireAttestationMethodTPM},
		{"SPIFFE TLSJWT", true, false, true, db.SpireAttestationMethodTPM},
		{"external TLSJWT", false, true, true, db.SpireAttestationMethodTPM},
		{"both transports TLSJWT", true, true, true, db.SpireAttestationMethodDSXIMDS},
		// Export must describe plaintext source state accurately, not silently
		// upgrade it. A future secure importer must reject this profile.
		{"explicit plaintext", false, false, false, db.SpireAttestationMethodTPM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := db.Node{
				ID: uuid.New(), ClusterID: uuid.New(), Name: "test-csc-cp", IP: "192.0.2.13", ControlPlane: true,
				SpiffeEnabled: tc.spiffe, NodeAgentExternalMTLSEnabled: tc.external,
				NodeAgentTLSJWTEnabled: tc.jwt, SpireAttestationMethod: tc.method,
			}
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
			database.On("ListNodes", mock.Anything, db.NodeFilters{ClusterID: &node.ClusterID}).Return([]db.Node{node}, nil).Once()
			database.On("GetCluster", mock.Anything, node.ClusterID).Return(db.Cluster{ID: node.ClusterID}, nil).Twice()
			service := NewNodeService(slog.New(slog.DiscardHandler), database, true)
			get, err := service.GetNode(context.Background(), api.GetNodeRequestObject{NodeID: node.ID})
			require.NoError(t, err)
			list, err := service.ListNodes(context.Background(), api.ListNodesRequestObject{Params: api.ListNodesParams{ClusterID: &node.ClusterID}})
			require.NoError(t, err)
			got := api.Node(get.(api.GetNode200JSONResponse))
			require.Equal(t, []api.Node{got}, list.(api.ListNodes200JSONResponse).Nodes)
			require.Equal(t, node.ID, got.ID)
			require.Equal(t, node.IP, got.IP)
			require.Equal(t, &api.NodeSecurityProfile{
				ClusterID: node.ClusterID, SpiffeEnabled: tc.spiffe,
				SpireAttestationMethod:       api.NodeSecurityProfileSpireAttestationMethod(tc.method),
				NodeAgentExternalMTLSEnabled: tc.external, NodeAgentTLSJWTEnabled: tc.jwt,
			}, got.SecurityProfile)

			encoded, err := json.Marshal(got)
			require.NoError(t, err)
			var wire struct {
				SecurityProfile map[string]interface{} `json:"securityProfile"`
			}
			require.NoError(t, json.Unmarshal(encoded, &wire))
			// Required booleans must survive JSON even when false. The profile
			// contains no key material, tokens, CA bundles or private identity state.
			require.Equal(t, map[string]interface{}{
				"clusterID": node.ClusterID.String(), "spiffeEnabled": tc.spiffe,
				"spireAttestationMethod":       string(tc.method),
				"nodeAgentExternalMTLSEnabled": tc.external, "nodeAgentTLSJWTEnabled": tc.jwt,
			}, wire.SecurityProfile)
			database.AssertExpectations(t)
		})
	}
}

func TestNodeSecurityProfileUnknownIsNotDefaulted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*db.Node)
	}{
		{"missing node ID", func(n *db.Node) { n.ID = uuid.Nil }},
		{"missing cluster ID", func(n *db.Node) { n.ClusterID = uuid.Nil }},
		{"missing attestor", func(n *db.Node) { n.SpireAttestationMethod = "" }},
		{"unknown attestor", func(n *db.Node) { n.SpireAttestationMethod = "unknown" }},
		{"join token without SPIFFE", func(n *db.Node) {
			n.SpiffeEnabled = false
			n.SpireAttestationMethod = db.SpireAttestationMethodJoinToken
		}},
		{"JWT without TLS", func(n *db.Node) { n.SpiffeEnabled = false; n.NodeAgentTLSJWTEnabled = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := db.Node{ID: uuid.New(), ClusterID: uuid.New(), SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodTPM}
			tc.change(&node)
			got := apiNodeFromDB(&node)
			require.Nil(t, got.SecurityProfile)
			encoded, err := json.Marshal(got)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "securityProfile")
		})
	}
	// The currently deployed API lacks this field entirely. Decoding such a
	// record must preserve absence, not synthesize an all-false profile.
	var legacy api.Node
	require.NoError(t, json.Unmarshal([]byte(`{"id":"11111111-1111-1111-1111-111111111111"}`), &legacy))
	require.Nil(t, legacy.SecurityProfile)
}

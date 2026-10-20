// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

func TestNicoAttachmentSnapshotRoundTrip(t *testing.T) {
	attachment := &db.NicoClusterAttachment{SiteID: uuid.New(), IdentitySecretName: "nico-identity", SpireServerEndpoint: "192.0.2.11:30081"}
	var spec db.ControlPlaneConfigSpec
	spec.SetCapi(db.CAPIControlPlaneProvider{NicoCluster: attachment})
	attachment.IdentitySecretName = "caller-mutation"
	require.Equal(t, "nico-identity", spec.Capi.NicoCluster.IdentitySecretName)
	raw, err := json.Marshal(spec)
	require.NoError(t, err)
	var stored db.ControlPlaneConfigSpec
	require.NoError(t, json.Unmarshal(raw, &stored))
	require.True(t, stored.Equal(spec))
	require.Equal(t, "192.0.2.11:30081", stored.Capi.NicoCluster.SpireServerEndpoint)
	stored.Capi.NicoCluster.SpireServerEndpoint = "192.0.2.12:30081"
	require.False(t, stored.Equal(spec))
	stored.Capi.NicoCluster.SpireServerEndpoint = spec.Capi.NicoCluster.SpireServerEndpoint
	provider, err := stored.CAPIProvider()
	require.NoError(t, err)
	provider.NicoCluster.SiteID = uuid.New()
	require.NotEqual(t, provider.NicoCluster.SiteID, stored.Capi.NicoCluster.SiteID)
	stored.Capi.NicoCluster = nil
	require.False(t, stored.Equal(spec))

	var pool db.NodePool
	pool.SetCapi(db.CAPINodePoolProvider{NicoCluster: spec.Capi.NicoCluster})
	copy, err := pool.CAPIProvider()
	require.NoError(t, err)
	require.True(t, pool.Capi.Equal(copy))
	copy.NicoCluster.SpireServerEndpoint = "192.0.2.12:30081"
	require.False(t, pool.Capi.Equal(copy))
	require.Equal(t, "192.0.2.11:30081", pool.Capi.NicoCluster.SpireServerEndpoint)
	copy.NicoCluster.IdentitySecretName = "changed"
	require.False(t, pool.Capi.Equal(copy))
	require.Equal(t, "nico-identity", pool.Capi.NicoCluster.IdentitySecretName)
}

func TestNicoAttachmentSpireServerEndpoint(t *testing.T) {
	for _, tc := range []struct {
		endpoint, host, port string
	}{
		{"192.0.2.11:30081", "192.0.2.11", "30081"},
		{"spire.example.test:8081", "spire.example.test", "8081"},
		{"[2001:db8::11]:30081", "2001:db8::11", "30081"},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			host, port, err := (db.NicoClusterAttachment{SpireServerEndpoint: tc.endpoint}).SpireServer()
			require.NoError(t, err)
			require.Equal(t, tc.host, host)
			require.Equal(t, tc.port, port)
		})
	}
	for _, endpoint := range []string{"", "spire.example.test", ":30081", "https://spire.example.test:30081",
		"spire.example.test:0", "spire.example.test:65536", "spire.example.test:grpc", "spire.example.test:-1",
		"localhost:30081", "127.0.0.1:30081", "0.0.0.0:30081", "224.0.0.1:30081", "[::1]:30081",
		"[::]:30081", "spire.example.test:30081/path", "spire.example.test\n:30081", `bad"host:30081`} {
		t.Run("reject "+endpoint, func(t *testing.T) {
			_, _, err := (db.NicoClusterAttachment{SpireServerEndpoint: endpoint}).SpireServer()
			require.ErrorContains(t, err, "spireServerEndpoint")
		})
	}
}

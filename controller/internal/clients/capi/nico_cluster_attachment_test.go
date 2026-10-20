// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/nodes"
)

// Record actual CreateMachine apply payloads, without pretending to test API
// server admission/SSA or CAPNICo reconciliation in a fake client.
type attachmentApplyRecorder struct {
	client.Client
	applied []client.Object
}

func (r *attachmentApplyRecorder) Patch(_ context.Context, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
	r.applied = append(r.applied, obj.DeepCopyObject().(client.Object))
	return nil
}

func attachmentFixture(t *testing.T) (db.Cluster, db.Node, db.NodeSet, db.NodePool, *clusterv1.Cluster, shared.MachineCreateDeps, *attachmentApplyRecorder) {
	t.Helper()
	cluster := db.Cluster{ID: uuid.New(), Name: "retained", ProviderType: db.ProviderTypeCAPI, KubernetesVersion: "1.34.2", EtcdEncrypted: true}
	cluster.ControlPlaneConfig.ConfigurationID = uuid.New()
	cluster.ControlPlaneConfig.SetCapi(db.CAPIControlPlaneProvider{
		NicoCluster:                     &db.NicoClusterAttachment{SiteID: uuid.New(), IdentitySecretName: "nico-identity", SpireServerEndpoint: "192.0.2.11:30081"},
		InfraMachineTemplatePreferences: []json.RawMessage{json.RawMessage(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1alpha1","kind":"NicoMachineTemplate","spec":{"template":{"spec":{"machineID":"target-host-4","vpcID":"vpc-test","interfaces":[{"subnetID":"subnet-test"}],"ipxeScript":"#!ipxe\nchain https://boot.example.test/node.ipxe","cloudInitInjectHostname":true}}}}`)},
	})
	node := db.Node{ID: uuid.New(), Name: "retained-cp-fourth", ClusterID: cluster.ID, ControlPlane: true,
		ControlPlaneConfigurationID: cluster.ControlPlaneConfig.ConfigurationID, SpiffeEnabled: true,
		SpireAttestationMethod: db.SpireAttestationMethodTPM, NodeAgentExternalMTLSEnabled: true, NodeAgentTLSJWTEnabled: true}
	set, pool := nodes.ControlPlaneConfigNodeContext(cluster, node, cluster.ControlPlaneConfig.ControlPlaneConfigSpec)
	owner := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: "nke-system", UID: types.UID("retained-capi-uid"),
		Labels: map[string]string{constants.LabelKeyNkeManagedBy: "retained-manager"}}, Spec: clusterv1.ClusterSpec{
		InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: nicov1.GroupVersion.Group, Kind: "ProvisionedCluster", Name: cluster.Name},
	}}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, nicov1.AddToScheme(scheme))
	recorder := &attachmentApplyRecorder{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner).WithStatusSubresource(&nicov1.NicoCluster{}).Build()}
	deps := shared.MachineCreateDeps{K8sClient: recorder, Namespace: owner.Namespace, FieldManagerName: "nke",
		Config: config.Config{ManagedByLabelValue: "retained-manager", NicoControllersEnabled: true, SpireServerAddress: "spire.example.test", SpireServerPort: "8081"},
		CreateSpireJoinToken: func(_ context.Context, actual db.Node) (string, string, error) {
			require.Equal(t, node, actual)
			return "", "", nil
		},
		NodeAgentBootstrapFiles: func(actual db.Node) ([]db.CloudInitFile, error) {
			require.Equal(t, node, actual)
			return []db.CloudInitFile{{Path: "/etc/nke/profile-test", Content: "external-mtls-and-tlsjwt"}}, nil
		},
		GetTrustDomainBundle: func(context.Context, config.Config) (string, string, error) {
			return "test.example", "test-bundle", nil
		},
	}
	return cluster, node, set, pool, owner, deps, recorder
}

func readyCompanion(t *testing.T, k8s client.Client, namespace, name string) *nicov1.NicoCluster {
	t.Helper()
	companion := &nicov1.NicoCluster{}
	require.NoError(t, k8s.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: name}, companion))
	companion.Status.Ready = true
	companion.Status.Initialization.Provisioned = ptr.To(true)
	require.NoError(t, k8s.Status().Update(t.Context(), companion))
	return companion
}

func TestNicoAttachmentCreateMachine(t *testing.T) {
	cluster, node, set, pool, owner, deps, recorder := attachmentFixture(t)
	// Include the original primary infrastructure and three Machines. All must
	// remain byte-for-byte unchanged through attachment and fourth-node creation.
	originals := []client.Object{owner.DeepCopy(), &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": nicov1.GroupVersion.String(), "kind": "ProvisionedCluster",
		"metadata": map[string]any{"name": cluster.Name, "namespace": deps.Namespace}, "spec": map[string]any{},
	}}}
	for _, name := range []string{"original-3", "original-1", "original-2"} {
		originals = append(originals, &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: deps.Namespace},
			Spec: clusterv1.MachineSpec{ClusterName: cluster.Name, InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: nicov1.GroupVersion.Group, Kind: "ProvisionedMachine", Name: name}}})
	}
	for _, original := range originals[1:] {
		require.NoError(t, recorder.Create(t.Context(), original))
	}
	for i, original := range originals {
		require.NoError(t, recorder.Get(t.Context(), client.ObjectKeyFromObject(original), original))
		originals[i] = original.DeepCopyObject().(client.Object)
	}
	err := New().CreateMachine(t.Context(), deps, node, set, pool, cluster)
	require.ErrorContains(t, err, "waiting for companion NicoCluster")
	require.Empty(t, recorder.applied)
	companion := readyCompanion(t, recorder, deps.Namespace, cluster.Name)
	require.Equal(t, owner.UID, metav1.GetControllerOf(companion).UID)
	require.Equal(t, cluster.ID.String(), companion.Labels[constants.LabelKeyClusterID])
	require.Equal(t, "nico-identity", companion.Spec.IdentityRef.Name)
	for range 2 {
		require.NoError(t, New().CreateMachine(t.Context(), deps, node, set, pool, cluster))
	}
	require.Len(t, recorder.applied, 6)
	infra := recorder.applied[0].(*unstructured.Unstructured)
	machineID, _, err := unstructured.NestedString(infra.Object, "spec", "machineID")
	require.NoError(t, err)
	require.Equal(t, "target-host-4", machineID)
	bootstrap := recorder.applied[1].(*corev1.Secret)
	require.Contains(t, string(bootstrap.Data["value"]), "external-mtls-and-tlsjwt")
	require.Contains(t, string(bootstrap.Data["value"]), "test-bundle")
	require.Contains(t, string(bootstrap.Data["value"]), `server_address = "192.0.2.11"`)
	require.Contains(t, string(bootstrap.Data["value"]), `server_port = "30081"`)
	require.NotContains(t, string(bootstrap.Data["value"]), "spire.example.test")
	machine := recorder.applied[2].(*clusterv1.Machine)
	require.Equal(t, "NicoMachine", machine.Spec.InfrastructureRef.Kind)
	require.Equal(t, node.ID.String(), machine.Annotations[constants.MachineAnnotationKeyHostID])
	require.Equal(t, cluster.Name, machine.Spec.ClusterName)
	for _, original := range originals {
		actual := original.DeepCopyObject().(client.Object)
		require.NoError(t, recorder.Get(t.Context(), client.ObjectKeyFromObject(original), actual))
		require.Equal(t, original, actual)
	}
	var companions nicov1.NicoClusterList
	require.NoError(t, recorder.List(t.Context(), &companions))
	require.Len(t, companions.Items, 1)
	require.True(t, cluster.EtcdEncrypted)
}

func TestNicoAttachmentMissingSpireEndpointMakesNoKubernetesChanges(t *testing.T) {
	cluster, node, set, pool, _, deps, recorder := attachmentFixture(t)
	set.Capi.NicoCluster.SpireServerEndpoint = ""
	require.ErrorContains(t, New().CreateMachine(t.Context(), deps, node, set, pool, cluster), "spireServerEndpoint")
	require.Empty(t, recorder.applied)
	var companions nicov1.NicoClusterList
	require.NoError(t, recorder.List(t.Context(), &companions))
	require.Empty(t, companions.Items)
}

func TestNicoAttachmentFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*nicov1.NicoCluster)
	}{
		{"foreign owner", func(c *nicov1.NicoCluster) { c.OwnerReferences[0].UID = "foreign" }},
		{"unowned companion", func(c *nicov1.NicoCluster) { c.OwnerReferences = nil }},
		{"different CSC", func(c *nicov1.NicoCluster) { c.Labels[constants.LabelKeyClusterID] = uuid.NewString() }},
		{"different manager", func(c *nicov1.NicoCluster) { c.Labels[constants.LabelKeyNkeManagedBy] = "other" }},
		{"different site", func(c *nicov1.NicoCluster) { c.Spec.SiteID = uuid.NewString() }},
		{"different identity", func(c *nicov1.NicoCluster) { c.Spec.IdentityRef.Name = "other" }},
		{"credential fallback", func(c *nicov1.NicoCluster) { c.Spec.IdentityRef.Name = "" }},
		{"not ready", func(c *nicov1.NicoCluster) { c.Status.Ready = false }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cluster, node, set, pool, _, deps, recorder := attachmentFixture(t)
			require.Error(t, New().CreateMachine(t.Context(), deps, node, set, pool, cluster))
			companion := readyCompanion(t, recorder, deps.Namespace, cluster.Name)
			test.mutate(companion)
			status := companion.Status
			require.NoError(t, recorder.Update(t.Context(), companion))
			companion.Status = status
			require.NoError(t, recorder.Status().Update(t.Context(), companion))
			require.Error(t, New().CreateMachine(t.Context(), deps, node, set, pool, cluster))
			require.Empty(t, recorder.applied)
		})
	}
	for _, test := range []struct {
		name   string
		mutate func(*db.Node, *clusterv1.Cluster)
	}{
		{"SPIFFE disabled", func(n *db.Node, _ *clusterv1.Cluster) { n.SpiffeEnabled = false }},
		{"missing attestor", func(n *db.Node, _ *clusterv1.Cluster) { n.SpireAttestationMethod = "" }},
		{"wrong node cluster", func(n *db.Node, _ *clusterv1.Cluster) { n.ClusterID = uuid.New() }},
		{"unpinned node", func(n *db.Node, _ *clusterv1.Cluster) { n.ControlPlaneConfigurationID = uuid.Nil }},
		{"paused owner", func(_ *db.Node, c *clusterv1.Cluster) { c.Spec.Paused = ptr.To(true) }},
		{"wrong manager", func(_ *db.Node, c *clusterv1.Cluster) { c.Labels[constants.LabelKeyNkeManagedBy] = "other" }},
		{"wrong primary provider", func(_ *db.Node, c *clusterv1.Cluster) { c.Spec.InfrastructureRef.Kind = "NicoCluster" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cluster, node, set, pool, owner, deps, recorder := attachmentFixture(t)
			test.mutate(&node, owner)
			require.NoError(t, recorder.Update(t.Context(), owner))
			require.Error(t, New().CreateMachine(t.Context(), deps, node, set, pool, cluster))
			require.Empty(t, recorder.applied)
			var companions nicov1.NicoClusterList
			require.NoError(t, recorder.List(t.Context(), &companions))
			require.Empty(t, companions.Items)
		})
	}
}

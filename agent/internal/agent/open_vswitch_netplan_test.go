// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyOpenVSwitchNetplanPlanPreservesBridgeMAC(t *testing.T) {
	const generate = "netplan generate"
	const pin = "ovs-vsctl --may-exist add-br breth0 -- set bridge breth0 other-config:hwaddr=02:11:22:33:44:55"
	const apply = "netplan apply"
	commandErr := errors.New("command failed")
	tests := map[string]struct {
		mac         string
		generateErr error
		pinErr      error
		applyErr    error
		wantErr     error
		wantCalls   []string
	}{
		"local MAC is pinned before adding the uplink": {
			mac: "02:11:22:33:44:55", wantCalls: []string{generate, pin, apply},
		},
		"global MAC keeps existing behavior": {
			mac: "00:11:22:33:44:55", wantCalls: []string{generate, apply},
		},
		"invalid netplan does not change OVS": {
			mac: "02:11:22:33:44:55", generateErr: commandErr, wantErr: commandErr,
			wantCalls: []string{generate},
		},
		"pin failure does not apply netplan": {
			mac: "02:11:22:33:44:55", pinErr: commandErr, wantErr: commandErr,
			wantCalls: []string{generate, pin},
		},
		"netplan apply failure is returned": {
			mac: "02:11:22:33:44:55", applyErr: commandErr, wantErr: commandErr,
			wantCalls: []string{generate, pin, apply},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(root, "etc/netplan"), 0o755))
			configPath := filepath.Join(root, "etc/netplan/50-test.yaml")
			require.NoError(t, os.WriteFile(configPath, []byte(`network:
  version: 2
  ethernets:
    uplink:
      match:
        name: "e*"
      dhcp4: true
`), 0o600))
			runner := newOpenVSwitchTestRunner(t, map[string][]openVSwitchCommandResult{
				generate: {{err: test.generateErr}},
				pin:      {{err: test.pinErr}},
				apply:    {{err: test.applyErr}},
			})
			server := &NodeAgentServer{Runner: runner, RootPath: root, openVSwitchSettleDelay: 1}
			plan, err := server.buildOpenVSwitchNetplanPlan(openVSwitchLink{
				Name: "eth0", Address: test.mac, MTU: 1500,
			}, "192.0.2.1", "breth0")
			require.NoError(t, err)

			err = server.applyOpenVSwitchNetplanPlan(context.Background(), plan)

			if test.wantErr != nil {
				require.ErrorIs(t, err, test.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, test.wantCalls, runner.calls)
			data, err := os.ReadFile(configPath)
			require.NoError(t, err)
			document, err := parseOpenVSwitchNetplanDocument(data)
			require.NoError(t, err)
			bridge := document.bridges["breth0"].(map[string]any)
			assert.NotContains(t, bridge, "macaddress", "OVS owns the bridge MAC, not netplan")
			assert.Equal(t, true, bridge["dhcp4"])
		})
	}
}

func TestParseOpenVSwitchNetplanDocumentCreatesOptionalSections(t *testing.T) {
	document, err := parseOpenVSwitchNetplanDocument([]byte(`network:
  version: 2
  renderer: networkd
`))

	require.NoError(t, err)
	document.ethernets["eth0"] = map[string]any{"dhcp4": true}
	document.bridges["breth0"] = map[string]any{"interfaces": []string{"eth0"}}

	network := document.root["network"].(map[string]any)
	assert.Equal(t, "networkd", network["renderer"])
	assert.Equal(t, document.ethernets, network["ethernets"])
	assert.Equal(t, document.bridges, network["bridges"])
}

func TestParseOpenVSwitchNetplanDocumentRejectsMalformedSections(t *testing.T) {
	testCases := []struct {
		name        string
		config      string
		errorString string
	}{
		{
			name:        "missing network",
			config:      "renderer: networkd\n",
			errorString: "network section is required",
		},
		{
			name:        "network is not a mapping",
			config:      "network: disabled\n",
			errorString: "network section must be a mapping",
		},
		{
			name:        "ethernets is not a mapping",
			config:      "network:\n  ethernets: []\n",
			errorString: "network.ethernets section must be a mapping",
		},
		{
			name:        "bridges is not a mapping",
			config:      "network:\n  bridges: disabled\n",
			errorString: "network.bridges section must be a mapping",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := parseOpenVSwitchNetplanDocument([]byte(testCase.config))

			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.errorString)
		})
	}
}

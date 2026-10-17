# API authentication in nke-controller-config: design

> **Status:** proposed. This covers phase 1, which is additive: nothing an operator or a released client relies on stops working. Phase 2, which retires the old path, is outlined at the end and designed separately.

## Problem

The controller reads every setting from `nke-controller-config` at startup except one: the NVKE API authentication configuration. That is still its own file, `auth.yaml` in the `nke-auth-config` ConfigMap, found through the `AUTH_CONFIG_PATH` environment variable and loaded by `internal/auth`. So the controller has two configuration files, two loaders and two ways to be misconfigured.

The authentication configuration was left out of the move to `nke-controller-config` deliberately, because folding it in raises two questions a rename does not answer:

- **How authentication is turned off.** Today an unset `AUTH_CONFIG_PATH` means off, a valid file means on, and a broken file stops the controller rather than letting it start unprotected. `nke-controller-config` is always mounted, so it can never be absent. The off switch has to become explicit.
- **Who owns the schema.** `auth.yaml` is a separately designed feature with its own types, strict loader and `apiVersion`/`kind` header. Folding it in must not leave a second copy of its rules.

## How it is used today

The chart has two modes, both selected by `apiAuth.configMap.create`:

| Mode | Who uses it | What happens |
|---|---|---|
| `create: true` | Every fleet site and the operator guide | The chart renders `apiAuth.config` into `nke-auth-config`, mounts it at `/etc/nke/auth` and sets `AUTH_CONFIG_PATH=/etc/nke/auth/auth.yaml`. |
| `create: false` | Tilt (`controller/deploy/k8s/nke-auth-dev.yaml`) and the CAPD and CAPNICo e2e deploy paths (`nvke-auth-dev` items) | The chart mounts a ConfigMap it does not own and sets `AUTH_CONFIG_PATH`. |

Every fleet site has the same shape: one issuer, `local-kubernetes`, the management cluster's own ServiceAccount issuer, with the projected ServiceAccount CA and token as its `caBundlePath` and `bearerTokenPath`, `serviceAccounts` admission and one audience. No site uses `apiAuth.configMap.extraFiles`.

Outside the controller, these read the configuration:

- `nvkectl` (`cli/internal/auth/source.go`) treats a missing `nke-auth-config` as "authentication off" and mints a ServiceAccount token for `audiences[0]` of its `auth.yaml` otherwise.
- The e2e suite's token helper (`controller/test/e2e/internal/authtoken`) reads the audiences from `nke-auth-config` the same way.
- The e2e suite's `detectAuthnEnforcement` (`controller/test/e2e/authn_test.go`) treats a missing `AUTH_CONFIG_PATH` on the controller Deployment as "authentication off", and reads `permissiveMode` from `nke-controller-config` otherwise.
- `hack/nvke-token.sh` and `hack/nvke-auth-demo.sh` read `AUTH_CONFIG_PATH` from the Deployment the same way.

## Goals

- The controller reads the authentication configuration from `nke-controller-config` when the chart renders it, through the same startup load as every other setting.
- `internal/auth` keeps the only definition of the schema and its rules.
- Turning authentication off is an explicit setting, and a configuration that is filled in but turned off is an error, not a silent "off".
- A configuration that is set but invalid still stops the controller at startup.
- No operator changes their chart values, and released `nvkectl` keeps working.

## Non-goals (phase 1)

- Retiring `AUTH_CONFIG_PATH` or the external-ConfigMap mode (`create: false`).
- Moving `nvkectl` off `nke-auth-config`.
- Moving `permissiveMode` under `apiAuth`. It governs the legacy file too, which is read while `apiAuth.enabled` is `false`, so it cannot live in a section that is turned off.
- An inline CA bundle. `caBundlePath` keeps working, and `extraFiles` keep landing in `/etc/nke/auth`.

## Design

### Schema

`nke-controller-config` gains an `apiAuth` section. Its `audiences` and `issuers` are `internal/auth`'s existing `Config` fields, unchanged; `enabled` is new.

```yaml
permissiveMode: false        # unchanged, top level
apiAuth:
  enabled: true
  audiences: [nke-site-example]
  issuers:
    - name: local-kubernetes
      issuer: https://kubernetes.default.svc.cluster.local
      jwks:
        uri: https://kubernetes.default.svc/openid/v1/jwks
        caBundlePath: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
        bearerTokenPath: /var/run/secrets/kubernetes.io/serviceaccount/token
      algorithms: [RS256]
      admit:
        serviceAccounts:
          - namespace: nke-clients
```

The embedded section has no `apiVersion` or `kind`, like every other section of the file. The standalone `auth.yaml` keeps requiring both.

### Startup behaviour

| `apiAuth.enabled` | `AUTH_CONFIG_PATH` | The controller |
|---|---|---|
| `false`, `audiences` and `issuers` empty | unset | Runs with authentication off, as today. |
| `false`, `audiences` and `issuers` empty | set | Loads the file at `AUTH_CONFIG_PATH`, as today. This is the external-ConfigMap mode. |
| `false`, `audiences` or `issuers` present | either | Refuses to start: the configuration is filled in but turned off. |
| `true` | unset | Validates `apiAuth` with `internal/auth`'s rules and enables authentication. A validation failure stops the controller. |
| `true` | set | Refuses to start and names both sources, rather than choosing one. |

Permissive mode comes from `permissiveMode` in every case, as today. The startup log line that reports the mode also names its source: `nke-controller-config` or the `AUTH_CONFIG_PATH` file.

### internal/auth

- One exported validation entry point runs everything `Load` checks today apart from the file header: `Config.Validate` plus the "every issuer needs `admit`" check, which today lives only in `Load`. `Load` calls it too, so both paths enforce identical rules.
- `Setup` takes either a path (the legacy file) or an already-validated `*Config`. Everything after loading, from key fetching to the middleware, is unchanged.

### controllerconfig

- `Config` gains `APIAuth`, holding `Enabled`, `Audiences` and `Issuers`. `Issuers` is a list of `auth.Issuer`, so the issuer schema stays defined once. The strict loader parses the section with the rest of the file.
- `validate` applies the table above: with `enabled: false`, `audiences` and `issuers` must be empty; with `enabled: true`, it calls `internal/auth`'s validation and prefixes errors with `apiAuth:`.
- `LoadStartupConfig`, which sees both the file and the environment, refuses `apiAuth.enabled` together with `AUTH_CONFIG_PATH`.
- `cmd/worker` passes the section to `auth.Setup` when it is enabled, and the path otherwise.
- `hack/local-controller-config.yaml` gains `apiAuth: {enabled: false, audiences: [], issuers: []}`.

### Chart

The values are unchanged. What the chart renders depends on the mode:

| Values | Rendered | The controller reads |
|---|---|---|
| `apiAuth.enabled: false` | `apiAuth: {enabled: false, audiences: [], issuers: []}`; no `AUTH_CONFIG_PATH` | nothing: authentication off |
| `enabled: true`, `configMap.create: true` | `apiAuth` with `enabled: true` and `apiAuth.config`'s `audiences` and `issuers`; no `AUTH_CONFIG_PATH`. `nke-auth-config` is still rendered from the same values and mounted at `/etc/nke/auth`. | `apiAuth` |
| `enabled: true`, `configMap.create: false` | `apiAuth: {enabled: false, …}`; the external ConfigMap mounted and `AUTH_CONFIG_PATH` set, as today | the file at `AUTH_CONFIG_PATH` |

`nke-auth-config` keeps being rendered in `create: true` mode for two reasons. `nvkectl` and the e2e token helper still find it there, and `extraFiles` still land in `/etc/nke/auth` for `caBundlePath`. Both copies come from the same `apiAuth.config` value, so they cannot disagree. The Reloader annotation already lists both ConfigMaps, and the render-time checks on `apiAuth.config` and `configMap.create` are unchanged. `AUTH_CONFIG_PATH` stays off the chart's list of retired variables, because the external-ConfigMap mode still sets it.

### Clients and tooling

`detectAuthnEnforcement` and both scripts treat authentication as on when either `AUTH_CONFIG_PATH` is set on the Deployment or `apiAuth.enabled` is `true` in `nke-controller-config`. Reading both keeps the e2e suite correct against controllers older than this change. `nvkectl` needs no change in phase 1.

## Compatibility and rollout

- **Fleet sites** keep their values. After the upgrade their controller reads `apiAuth` instead of `AUTH_CONFIG_PATH`, with the same issuers, audiences and mode.
- **Tilt and the CAPNICo e2e path** keep the external ConfigMap and the legacy path.
- **Rollback** is chart and image together, as for #1527. A controller older than this change rejects the unknown `apiAuth` key and does not start, so a mismatched pair fails closed rather than running unauthenticated.

## Testing

- `internal/auth`: the shared validation entry point, including the `admit` check, and `Setup` from an in-memory `Config`.
- `controllerconfig`: a table over `enabled` and the presence of `audiences` and `issuers`; the `AUTH_CONFIG_PATH` conflict in `LoadStartupConfig`; the drift tests extended to the new key.
- Chart: `chart-auth-assertions.sh` checks that `create: true` renders `apiAuth` and no `AUTH_CONFIG_PATH`, and that `create: false` is unchanged. `chart-controller-config-assertions.sh` checks that `apiAuth` renders in every mode.
- E2E: the CAPD e2e deploy path moves to `create: true`, so CI exercises the new path. The authentication document moves into its overlay values. Its `nvke-auth-dev` item keeps the ServiceAccounts and RBAC but drops its `nke-auth-config` ConfigMap, because the chart now renders that ConfigMap. Tilt and the CAPNICo e2e path exercise the legacy one. The authentication specs also run on a local CAPD cluster before the pull request.
- Docs: the operator guide (Secure the NVKE API), `controller/docs/configuring-api-authentication.md` and the `AUTH_CONFIG_PATH` row in `controller/AGENTS.md`.

## Phase 2, outline

Retire `AUTH_CONFIG_PATH` and the external-ConfigMap mode; move Tilt and the CAPNICo e2e path onto values; teach `nvkectl` to read `nke-controller-config` first; stop rendering `nke-auth-config` once released `nvkectl` no longer needs it; consider moving `permissiveMode` under `apiAuth` and an inline CA bundle. That is a breaking change, like #1527, and gets its own design.

## Alternatives considered

- **`auth.Load` reads the section out of `config.yaml`.** One loader, but `internal/auth` would have to know the controller configuration's layout, and the file would be parsed twice.
- **The chart renders `auth.yaml` from `nke-controller-config`'s values and the controller is left alone.** The controller would still read two files, which is the problem this solves.
- **Presence of issuers as the on switch.** Fewer keys, but an accidentally emptied list would turn authentication off instead of failing, against the fail-closed contract.
- **Move `permissiveMode` under `apiAuth` now.** Tidier, but the legacy file, read while `apiAuth.enabled` is `false`, would then take its mode from a section that is turned off. Moving it would also break the e2e suite's `SetPermissiveMode`, which writes the top-level key, on controllers older than this change.

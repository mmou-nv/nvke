# Incremental NICo Attachment POC

This branch extracts the retained-manager scale-out implementation onto its
upstream base, `6ffc2e5cae6739215b5cccf71f2692dcfa8da1c4`. It is an experimental
code reference, not a production-ready change rebased onto current main.

- Keep the existing, managed `ProvisionedCluster` and its node snapshots.
- Attach a companion `NicoCluster` for new, explicitly selected hosts.
- Send the next host template and desired controller count in one update.
- Require converged, healthy existing nodes before increasing the count by one.
- Preserve each node's infrastructure and security settings across retries.
- Reject recycling, surge, scale-down, and Kubernetes upgrades on this path.

The attachment supplies the new hosts' SPIRE endpoint and optional FMDS endpoint
without changing the retained nodes' configuration. It does not import an
unmanaged cluster or transfer ownership to a replacement permanent manager.

Simulator routing, image packaging, credentials, and operational recovery scripts
are deliberately excluded. The extracted source is not a verified build-source
mapping for a previously deployed controller image.

Before proposing this upstream: review the ownership model, integrate permanent
identity, rebase onto current main, and qualify physical-host scale-out and
recovery. Do not treat decreasing the desired count as a tested rollback.

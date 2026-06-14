# Operator

The Elemental operator extends Rancher introducing OS provisioning and management capabilities.

Machines booting from an Elemental live ISO register to the Elemental Operator, get provisioned
with the OS and a k8s distro, forming a new Kubernetes cluster immediately available in Rancher.

See the [Elemental docs](https://elemental.docs.rancher.com) for more information.

## Installation

The Elemental operator should be installed on a K8s cluster running Rancher Multi Cluster
Management server.

A step by step [guide](https://elemental.docs.rancher.com/quickstart-ui) is available in the [official documentation](https://elemental.docs.rancher.com).

## Alauda branch configuration notes

This branch expects the Elemental HTTP endpoint to be configured explicitly instead of deriving it from Rancher settings.

- `--server-url` is required. It must be the externally reachable `http` or `https` URL for the Elemental operator HTTP server, without a trailing path. The operator uses it to populate `MachineRegistration.status.registrationURL`, SeedImage download URLs, and the system-agent Kubernetes URL.
- `--http-bind-addr` controls where the Elemental HTTP server listens. The default is `:8082`.
- `--ca-cert-file` points to an optional PEM CA bundle. When set, the CA data is embedded in machine registration configs and SeedImage registration configs.
- `--agent-tls-mode` controls the generated system-agent TLS mode. Valid values are `strict` and `system-store`; the default is `strict`.
- `--system-agent-cluster-name` selects the platform Kubernetes cluster path used for system-agent registration. The generated URL is `<server-url>/kubernetes/<cluster-name>`, and the default cluster name is `local`.
- `--seedimage-image-pull-secrets` accepts a comma-separated list of image pull secret names. The operator trims empty entries, removes duplicates, and propagates the resulting secrets to SeedImage builder Pods.

Machine inventories with no network configurator, or with `spec.network.configurator: none`, are marked with `NetworkConfigReady=True` directly because no IPAM reconciliation is required. Inventories that use another configurator keep waiting for the normal network reconciliation flow.

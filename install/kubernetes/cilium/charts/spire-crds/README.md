# spire-crds

ClusterStaticEntry CRD for the Cilium SPIRE integration

## Source Code

* <https://github.com/spiffe/spire-controller-manager>

## Requirements

Kubernetes: `>= 1.21.0-0`

Helm installs this CRD only on initial installation. Existing CRDs are not
adopted or upgraded, and uninstall retains the CRD and its instances.

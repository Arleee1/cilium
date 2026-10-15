.. only:: not (epub or latex or html)

    WARNING: You are looking at unreleased Cilium documentation.
    Please use the official rendered version released here:
    https://docs.cilium.io

.. _encryption_ztunnel:

*************************************
Ztunnel Transparent Encryption (Beta)
*************************************

.. include:: /beta.rst

This guide explains how to configure Cilium to use ztunnel for transparent
encryption and mutual TLS (mTLS) authentication between Cilium-managed endpoints.
ztunnel is a purpose-built per-node proxy that provides transparent Layer 4 mTLS
encryption and authentication for pod-to-pod communication.

When ztunnel is enabled in Cilium, the agent running on each cluster node
establishes a control plane connection with the local ztunnel proxy. Cilium
enrolls pods into the mesh on a per-namespace basis, allowing fine-grained
control over which workloads participate in mTLS encryption. Enrolled pods have
their traffic transparently redirected to the ztunnel proxy using iptables rules
configured in their network namespace, where the traffic is encrypted and
authenticated using mutual TLS before being sent to the destination.


Generating secrets for authentication
=====================================

Cilium's ztunnel integration requires a set of private keys and accompanying
to certificates be present via Kubernetes secrets. This follows the same pattern
as IPsec key injections.

These keys can be generated with the following bash script prior to deploying
Cilium.

.. literalinclude:: ../../../examples/kubernetes-ztunnel/generate-secrets.sh
   :language: bash

The 'bootstrap' keys are used to secure the connection between ztunnel and
Cilium's xDS and certificate server implementation.

The 'ca' keys are used as the root certificate for creating in-memory and
ephemeral client certificates on ztunnel's request.

Enable ztunnel in Cilium
========================

Before you install Cilium with ztunnel enabled, ensure that:

* The necessary Kubernetes secrets are available.
* Cluster Mesh is not enabled (ztunnel is currently not compatible with Cluster Mesh).

.. tabs::

    .. group-tab:: Cilium CLI

       If you are deploying Cilium with the Cilium CLI, pass the following
       options:

       .. parsed-literal::

          cilium install --version |CHART_VERSION| \\
             --set encryption.enabled=true \\
             --set encryption.type=ztunnel

    .. group-tab:: Helm

       If you are deploying Cilium with Helm by following
       :ref:`k8s_install_helm`, pass the following options:

       .. parsed-literal::

           helm install cilium |CHART_RELEASE| \\
             --namespace kube-system \\
             --set encryption.enabled=true \\
             --set encryption.type=ztunnel


.. _ztunnel_spire_registration:

SPIRE registration backends
===========================

When ztunnel uses an external SPIRE CA (``ztunnel-ca-type=spire``), the operator
registers every ServiceAccount in each enrolled namespace. An account is
registered even if it has no Pods. Deleting a ServiceAccount removes its
registration even if its Pods remain; deleting Pods alone does not remove the
account's registration. Namespace enrollment remains the policy boundary, with
no per-account opt-in. The default internal CA does not use SPIRE registration.

The default SPIRE registration backend calls the Entry API directly. The optional
CRD backend persists one ``spire.spiffe.io/v1alpha1`` ``ClusterStaticEntry`` per
account, which each SPIRE controller-manager reconciles into its own server.
For ``shop/checkout``, both backends use:

.. code-block:: yaml

   spiffeID: spiffe://<trust-domain>/ns/shop/sa/checkout
   parentID: spiffe://<trust-domain>/ztunnel
   selectors:
     - k8s:ns:shop
     - k8s:sa:checkout

The trust domain comes from ``mesh-auth-spiffe-trust-domain``. This is the
ServiceAccount-based, static-parent model, not Pod/node-agent registration.

The SPIRE client package keeps shared interfaces, entry configuration, and
backend selection in ``client.go``. The direct Entry API implementation is in
``entry_client.go``, and the Kubernetes CRD implementation is in
``crd_client.go``.

To select the CRD backend, retain the historical operator flag
``--enable-ztunnel-spiffeid-crd=true``. Despite its name, it now creates
``ClusterStaticEntry`` resources, not namespace-wide ``ClusterSPIFFEID`` rules.
The flag requires ``enable-ztunnel`` and ``ztunnel-ca-type=spire``; it does not
implicitly switch ztunnel from the internal CA to SPIRE.
For Helm installations using an external SPIRE deployment, the relevant values
are:

.. code-block:: yaml

   encryption:
     enabled: true
     type: ztunnel
     ztunnel:
       ca:
         type: spire
   authentication:
     mutual:
       spire:
         enabled: true
         install:
           enabled: false
   operator:
     extraArgs:
       - --enable-ztunnel-spiffeid-crd=true

Install the ClusterStaticEntry CRD and enable its reconciliation on every
controller-manager before enabling this backend. The operator flag does not
install these components. Each server must also have the bootstrap entries and
delegated identity configuration needed for the ``/ztunnel`` parent. The Helm
chart grants the operator ``get``, ``list``, ``create``, ``update``, and ``delete``
on ``clusterstaticentries`` when ztunnel uses the SPIRE CA and
``authentication.mutual.spire.enabled`` is enabled; custom RBAC must provide the
same permissions.

The sole ownership marker is
``app.kubernetes.io/managed-by=cilium-operator-ztunnel`` on
``ClusterStaticEntry.metadata.labels``. Resources with a missing or different
value are not adopted. Do not put this feature-specific label on bootstrap or
foreign resources. ``spec.className`` selects consuming SPIRE
controller-managers; it does not identify the resource's producer.

The ``spire.cilium.io/service-account`` annotation stores the validated
``namespace/serviceaccount`` key used to associate registrations with accounts
and scope namespace cleanup. Missing or malformed identity data causes an
error, not deletion; account-specific updates and deletions also reject a
mismatched identity. Names remain deterministic and contain a hash of the
complete account key, but names and additional labels do not establish
ownership. Separate purpose and namespace labels are no longer written or
required. Namespaced owner references are not valid for these cluster-scoped
objects.

The operator prunes owned orphan registrations after both inventories
initialize and every minute thereafter. Failed account deletions also requeue
enrolled namespaces for cleanup; namespace deletion does not depend on accounts
still being present. Updates retain resourceVersion checks, and deletions retain
UID and resourceVersion preconditions.

A successful CRD operation means Kubernetes accepted the desired change.
``Initialized`` means backend initialization, not convergence of all SPIRE
replicas. A shared CR status cannot demonstrate that every independent server
has reconciled the change. Registration deletion does not revoke already-issued
certificates or terminate established connections.

.. _ztunnel_spire_crd_migration:

Migrating earlier CRD deployments
----------------------------------

This change does not automatically migrate or delete earlier namespace rules
or adopt ClusterStaticEntry resources with the old broad
``app.kubernetes.io/managed-by=cilium-operator`` value. Before relying on
ServiceAccount deletion semantics:

#. Prepare ClusterStaticEntry reconciliation, RBAC, the configured trust domain,
   and ``/ztunnel`` parenting on every server. Stop old operator writers so a
   mixed-version rollout cannot recreate namespace-wide rules.
#. Inspect existing per-account ClusterStaticEntry resources individually.
   Explicitly verify each intended registration's ServiceAccount annotation,
   SPIFFE ID, parent, and selectors before manually migrating its managed-by
   value to ``cilium-operator-ztunnel``. Preserve the valid account annotation.
   Alternatively, plan verified removal and recreation of obsolete entries,
   accounting for an issuance gap. An old-label entry at the generated name
   blocks that account's upsert until it is explicitly migrated or removed.
   Never bulk-relabel resources using the old broad value: bootstrap and foreign
   resources must remain untouched.
#. Upgrade the operator using the same CRD flag. Confirm that each current
   enrolled ServiceAccount has an owned ClusterStaticEntry, including accounts
   without Pods. Verify the generated registrations and delegated certificate
   issuance against each SPIRE replica separately.
#. Inspect and remove only verified legacy operator-created ClusterSPIFFEID
   namespace rules after the replacement registrations work. Also inspect any
   superseded account entries from older ClusterStaticEntry PoCs, which are not
   automatically adopted. Do not bulk-delete by the old broad managed-by value
   or by label presence alone. Never remove bootstrap resources such as
   ``ztunnel`` and ``spire-agent-node-alias``.
#. Verify that the old rules and their generated registrations have disappeared
   from every server. Test that deleting one ServiceAccount removes only its
   registration while its Pods remain, and that deleting a Pod alone leaves its
   account registered.

Until legacy rules and registrations are removed, they can continue authorizing
Pods after the new ServiceAccount CR has been deleted. Migration is not complete
at Kubernetes write acceptance or a shared status update, and a zero-downtime
migration is not guaranteed. Switching backends or rolling back also requires
explicitly reviewing and cleaning up the previous backend's registrations.

Enrolling Namespaces
====================

After enabling ztunnel in Cilium, you need to explicitly enroll namespaces to
enable mTLS encryption for their workloads. This is done by applying a label
to the namespace:

.. code-block:: shell-session

    kubectl label namespace <namespace-name> io.cilium/mtls-enabled=true

To verify that a namespace is enrolled:

.. code-block:: shell-session

    kubectl get namespace <namespace-name> --show-labels

When a namespace is enrolled:

* All existing pods in the namespace (except ztunnel pods themselves) are enrolled
* Iptables rules are configured in each pod's network namespace for traffic redirection
* Pod metadata is sent to the ztunnel proxy via the ZDS protocol
* Future pods created in the namespace are automatically enrolled

To disenroll a namespace:

.. code-block:: shell-session

    kubectl label namespace <namespace-name> io.cilium/mtls-enabled-

This will:

#. Disenroll all pods in the namespace from ztunnel
#. Remove the iptables rules from each pod's network namespace
#. Notify ztunnel to stop processing traffic for those workloads

Validate the Setup
==================

#. Check that ztunnel has been enabled:

   .. code-block:: shell-session

      kubectl -n kube-system describe cm cilium-config | grep enable-ztunnel -A2

   You should see output indicating that ztunnel encryption is enabled.

#. Check which namespaces are enrolled:

   .. code-block:: shell-session

      kubectl get namespaces -l io.cilium/mtls-enabled=true

   This shows all namespaces labeled for ztunnel enrollment.

   To verify that these namespaces are actually enrolled in the StateDB table:

   .. code-block:: shell-session

      kubectl exec -n kube-system ds/cilium -- cilium-dbg statedb dump | jq '.["mtls-enrolled-namespaces"]'

   The results of this query should show which namespaces have been successfully
   processed by the enrollment reconciler.

#. Run a ``bash`` shell in one of the Cilium pods hosting a mtls-enrolled pod with
   ``kubectl -n kube-system exec -ti pod/<cilium-pod-hosting-mtls-pod> -- bash``
   and execute the following commands:

   Install tcpdump

   .. code-block:: shell-session

       $ apt-get update
       $ apt-get -y install tcpdump

   Check that traffic is encrypted. In the example below, this can be verified
   by the fact that packets will have a destination port of 15008 (HBONE).
   In the example below, ``eth0`` is the interface used for pod-to-pod
   communication. Replace this interface with e.g. ``cilium_vxlan`` if
   tunneling is enabled.

   .. code-block:: shell-session

       tcpdump -i eth0 port 15008
       tcpdump: verbose output suppressed, use -v or -vv for full protocol decode
       listening on eth0, link-type EN10MB (Ethernet), snapshot length 262144 bytes
       13:00:06.982499 IP 10.244.1.95.15008 > 10.244.2.3.33446: ...
       13:00:06.982536 IP 10.244.2.3.33446 > 10.244.1.95.15008: ...
       13:00:06.982675 IP 10.244.2.3.33446 > 10.244.1.95.15008: ...


Limitations
===========

* Traffic between workloads is only supported when both the source and
  destination endpoints are enrolled in ztunnel. Communication between an
  enrolled workload and a non-enrolled workload is not supported.

* The ztunnel integration currently only supports enrollment via namespace
  labels. Pod-level enrollment is not supported.

* Only TCP traffic is currently supported for mTLS encryption. UDP and other
  protocols are not redirected to ztunnel.

* The integration requires iptables support in the kernel and cannot be used
  with environments that do not support iptables (such as some minimal container
  runtimes).

* Ztunnel interferes with Cilium network policy as traffic is encrypted before
  it leaves the pod, meaning L4 policies won't work except for directly
  targeting the ztunnel HBONE port (15008).

Known Issues
============

* Cluster Mesh is not currently supported when ztunnel is enabled. Attempting
  to enable both will result in a validation error.

* Pods without a network namespace path (such as host-networked pods) cannot
  be enrolled in ztunnel and will be skipped during enrollment.

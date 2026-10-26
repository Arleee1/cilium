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

.. _encryption_ztunnel_spire_crds:

Registering SPIRE entries with Kubernetes resources
===================================================

When ztunnel obtains workload certificates from the SPIRE installation of the
chart (``encryption.ztunnel.ca.type=spire``,
``authentication.mutual.spire.enabled=true`` and
``authentication.mutual.spire.install.enabled=true``), the Cilium operator
registers a SPIRE entry for each service account of the enrolled namespaces
through the SPIRE Entry API, and the ``cilium-init`` container of the SPIRE
server registers the SPIRE agents, the Cilium agent and the Cilium operator.

SPIRE agents authorize ``spiffe://<trust domain>/ztunnel`` to use the
Delegated Identity API in both registration modes whenever ztunnel uses SPIRE.
Authorization does not create the delegate's registration entry: legacy
installations still need that entry, while CRD mode supplies it automatically.
The agents restart when their startup-loaded configuration changes.

With ``encryption.ztunnel.spire.useCRD=true``, a spire-controller-manager
sidecar of the SPIRE server registers these identities from Kubernetes
resources instead:

* A ClusterSPIFFEID gives each pod of the enrolled namespaces the SPIFFE ID
  ``spiffe://<trust domain>/ns/<namespace>/sa/<service account>``. The
  operator stops writing entries through the SPIRE Entry API.
* ClusterStaticEntries register the SPIRE agents and the ``cilium-agent``,
  ``cilium-operator`` and ``ztunnel`` identities. They replace the
  ``cilium-init`` container.
* The Cilium agents advertise the SPIFFE trust domain to ztunnel. This
  configuration is also read only at startup, so enabling or disabling
  ``useCRD``, or changing ``authentication.mutual.spire.trustDomain`` in
  CRD mode, restarts the Cilium Pods even with ``rollOutCiliumPods=false``.
  Unrelated ConfigMap changes still follow ``rollOutCiliumPods``.

The elected Cilium operator initializes four ClusterStaticEntries and one
ClusterSPIFFEID when its leader lifecycle starts. Helm configures the operator
and its permissions but does not render these resource instances. The operator
keeps the single namespace-selector-based workload resource rather than creating
one resource per namespace or service account. Each server's sidecar
independently translates the same resources into its own datastore.

Initialization uses the Kubernetes API directly, without waiting for a SPIRE
connection or an operator SVID. It retries transient API errors and missing CRDs
with exponential backoff, allowing up to six attempts per resource and at most
two minutes for the entire initialization. Permanent errors, such as missing
permissions or another resource owner, fail initialization immediately. If
initialization cannot complete, operator startup fails and logs the cause
before shutting down. Unchanged resources are not written. Updates preserve
the sidecars' status and finalizers and retry conflicts with status updates.

After successful initialization, the operator does not poll, watch or
periodically reconcile these resource instances. Accidental deletion or manual
drift is not repaired until the next operator or leader startup. The SPIRE
controller-manager sidecars continue to reconcile namespace and workload
changes at runtime.

Resource names use ``cilium-spire-<Cilium namespace>-<Helm release>-`` followed
by ``spire-agent``, ``cilium-agent``, ``cilium-operator``, ``ztunnel`` or
``ztunnel-workloads``. The operator manages their specs, ``commonLabels`` and
``authentication.mutual.spire.annotations``. It adds the reserved label
``app.kubernetes.io/managed-by=cilium-operator`` and refuses to overwrite
resources owned by Helm or another writer. Do not set Helm release annotations
on these resources. Changes to the trust domain, class, service accounts or
resource metadata restart the operator even with ``operator.rollOutPods=false``.

Stopping the operator, disabling CRD mode or uninstalling the chart does not
delete these operator-created resources. This avoids revoking registrations
during an operator restart or leadership handoff. Remove them explicitly only
after the associated identities are no longer needed. Existing Helm-owned
registration resources are not automatically adopted.

The sidecar only manages entries whose IDs start with ``cilium.``: it creates,
updates and deletes them to match the ClusterSPIFFEIDs and ClusterStaticEntries
of the class ``encryption.ztunnel.spire.className``, and leaves all other
entries alone. CRD mode uses the fixed ``spire-server-crd`` StatefulSet,
``spire-server-crd`` server ConfigMap, ``spire-server-crd-controller-manager``
controller ConfigMap and ``spire-server-crd-bundle`` bootstrap bundle. Its
``spire-data-spire-server-crd-<ordinal>`` PVCs are independent of legacy storage.
The client-facing Service remains ``spire-server``; clients do not need a new
hostname. In CRD mode, the SPIRE server Pods restart when either the server
or controller-manager configuration changes. This includes changes to this
class and to ``authentication.mutual.spire.install.agent.serviceAccount.name``,
which the server must reload before it can attest agents using the new account.

Once the fixed ``spire-server-crd`` layout is established, ordinary follow-up
upgrades preserve its StatefulSet and PVC identities. Configuration changes can
roll the Cilium and SPIRE server Pods; with one server, certificate issuance can
pause during its restart. An older experimental CRD layout that used
``spire-server`` for the StatefulSet has different storage identities and is
not a Pod-only upgrade or a supported legacy-registration cutover.

The ClusterSPIFFEID selects every namespace labeled
``io.cilium/mtls-enabled=true``, just as the operator does without ``useCRD``.
The chart explicitly clears the controller's built-in namespace exclusions
with ``ignoreNamespaces: []``. Namespace names do not affect enrollment,
including ``kube-system``, ``kube-public``, ``spire-system`` and names that
contain those strings.

The sidecar reports its progress in the status of the resources.
``status.stats`` of a ClusterSPIFFEID counts the selected pods, the entries to
set and the entries that failed. ``status.rendered`` and ``status.masked`` of a
ClusterStaticEntry show whether its entry rendered and whether another resource
declares the same entry. ``status.set`` is only ``true`` after the
reconciliation that created the entry and returns to ``false`` at the next one,
and it never becomes ``true`` for an entry that the sidecar adopted, so it does
not show whether the entry exists: check the entries in the SPIRE server
instead.

Prerequisites
-------------

CRD registration requires ``operator.enabled=true``. If you manage the
operator's RBAC yourself, grant it ``get``, ``create`` and ``update`` on
``clusterspiffeids`` and ``clusterstaticentries`` in ``spire.spiffe.io``.
It does not need permission to write their status.

Neither the chart nor the operator installs the ClusterSPIFFEID and
ClusterStaticEntry custom resource definitions (CRDs).
Before you install Cilium with ``useCRD`` or
enable it on an existing installation, install the CRDs of the
spire-controller-manager version that the chart deploys, v0.6.2 by default:

.. code-block:: shell-session

   kubectl apply --server-side \
     -f https://raw.githubusercontent.com/spiffe/spire-controller-manager/v0.6.2/config/crd/bases/spire.spiffe.io_clusterspiffeids.yaml \
     -f https://raw.githubusercontent.com/spiffe/spire-controller-manager/v0.6.2/config/crd/bases/spire.spiffe.io_clusterstaticentries.yaml

If the cluster does not serve both resources, the installation or upgrade fails
and prints the command that installs the missing CRDs.
``encryption.ztunnel.spire.trustCRDsExist=true`` skips this check, for example
when you render the chart without access to the cluster. It does not install
the CRDs. The operator cannot create the registrations until both exist. If
they remain unavailable beyond its bounded startup retries, initialization
fails even when the Helm check is skipped.

Enabling useCRD on an existing installation
-------------------------------------------

Do not enable ``useCRD`` with a single ordinary Helm upgrade on a running
legacy installation. The replacement servers have new databases and signing
authorities, and Helm does not order trust distribution, writer shutdown,
Service routing and client reconnection.

A safe cutover first establishes a shared upstream root and proves its
distribution and certificate renewal. It then stages the fresh CRD servers,
checks registrations in every datastore, stops the legacy operator writer,
switches the stable Service and deliberately reconnects clients. Retire the
legacy server only after new issuance and workload traffic are accepted.
Retain its PVC unless its deletion is separately authorized. Arbitrary legacy
registrations are not copied into the new databases.

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

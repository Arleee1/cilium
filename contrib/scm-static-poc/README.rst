Historical operator-produced static registrations
==================================================

This restoration starts at ``ee72a5dce26a2f1acb96aa8cca8e53d7a31db33e``,
the preserved ``backup/spire-ha-pre-rebase-20261026`` implementation. The
operator watches enrolled Namespaces and ServiceAccounts and writes one
``ClusterStaticEntry`` per namespace/SA pair, including unused SAs. It does
not enumerate Pods to produce those identities.

The only runtime compatibility additions are optional CRD-backend flags:

.. code-block:: text

   --enable-ztunnel-spiffeid-crd=true
   --mesh-auth-spiffe-trust-domain=spiffe.cilium
   --mesh-auth-spire-crd-parent-id=spiffe://spiffe.cilium/ns/cilium-spire/sa/spire-agent
   --mesh-auth-spire-crd-class=cilium-spire

Unconfigured behavior remains the historical ``/ztunnel`` parent and no class
on new entries. The direct Entry API backend is unchanged. Ownership and
namespace-scoped pruning remain enforced by the original producer label,
namespace label and full namespace/SA identity annotation.

Do not install this branch's entire historical Helm chart over the running POC.
Keep the current agent, ztunnel, SPIRE server/agent and standalone SCM images;
keep roots, Secrets, PVCs, bootstrap declarations and durable image delivery.
The archived source already includes the same ztunnel SPIFFE trust-domain/xDS
support as the retained running agent source. No trust-domain backport or
dataplane downgrade is required.

Build only the generic operator with ``Dockerfile.operator``. It uses pinned
builder/runtime images and the established local build recipe, avoiding live
APK/gops installer downloads. Label the image with its exact clean source commit
and save its full index, amd64 manifest and config-digest chain in an offline
archive. Do not publish it.

Preparation without cluster writes
----------------------------------

``prepare_restore.py`` reads saved current Deployment, ClusterRole, ConfigMap
and StatefulSet JSON documents and writes narrowly scoped JSON patches. It
does not contact Kubernetes or change kubeconfig. Every patch tests the object's
UID, resourceVersion and the exact fields it replaces. Regenerate from a fresh,
guarded read if the objects change; do not remove concurrency preconditions.

.. code-block:: bash

   python3 contrib/scm-static-poc/prepare_restore.py \
     --baseline "$PRIVATE_BASELINE" --output "$PRIVATE_PATCHES" \
     --operator-image "localhost/scm-sa-poc/operator-generic:static-$SHORT_COMMIT"

The producer patch removes newer Set/startup-bootstrap flags, enables the old
CRD producer, adds the parent/class overrides, and sets
``--skip-crd-creation=true``. This prevents the restored operator's general
Cilium CRD installer from replacing newer live schemas. Only its SPIRE RBAC
changes: ``ClusterStaticEntry`` get/list/create/update/delete, without Set
mutation permission. Other operator controllers and their existing permissions
remain unchanged.

The separate Static-only patches preserve the SCM class, trust domain and
entry ownership prefix; disable Set and Pod reconciliation; retain Static
reconciliation; remove the now-unused generic input allowlist/read permissions;
and update only the SCM configuration checksum on the existing StatefulSet.
They must not be applied until the handoff gates below pass.

These are persistent resource overlays, not a rollback of the Helm release.
The saved Helm values still describe the preceding Set mode. Do not run a
future Helm upgrade or rollback with those stale values while the restored
producer is active: integrate the same narrow transformations into the next
release or deliberately execute the documented reverse handoff first.

Proposed guarded rollout
------------------------

No preparation command authorizes live mutation. Wait for explicit rollout
authorization and the exact SCM adoption/rollback evidence.

1. Recheck the dedicated API endpoint and kube-system UID. Save a fresh private
   Linux rollback checkpoint, including full registrations on both stores,
   Set and Static declarations, roots/PVC identities and the touched resources.
2. Retain all current and rollback image references/archives, add the new
   operator archive to the existing private TLS NFS cache, and verify exact
   target digests on all three nodes.
3. Inventory the recorded stopped proof resources. If any remain, withdraw only
   those matching saved UIDs/specs while Sets are still enabled. Preserve the
   baseline Set, real ServiceAccounts and unrelated resources.
4. Apply the narrow producer RBAC and operator Deployment overlays with SCM
   Set+Static still enabled. Wait for six correctly owned, classed and parented
   CSEs. Compare all ten full registrations, IDs and revisions on both stores.
5. After equivalent CSE adoption is established, withdraw the primary Set using
   UID/resourceVersion preconditions. Recheck registration equality and require
   zero owned-prefix ``identityset.`` entries on both stores.
6. Only then apply Static-only SCM configuration/RBAC and roll the existing
   sidecars. Keep the four bootstrap CSEs and the same prefix at every stage.
7. Exercise bounded Namespace/SA lifecycle, unused-SA production and restart
   recovery after Static-only is established, not during the overlap. Verify
   actual SVID issuance, cross-node authenticated metrics and unenrolled denial.
   Restore baseline demo workloads and remove only temporary proof resources.

The ``identityset.`` gate is essential: SCM 89e371c ignores those entry IDs
when Set reconciliation is disabled, even with Static reconciliation enabled.
The six preserved historical ``serviceaccount.`` entry IDs are not renamed.
New lifecycle identities must be created only after Static-only is active so
the generic Set cannot win their initial creation.

Proposed reverse handoff
-----------------------

Retain the same SCM version, prefix, root and four bootstrap declarations.
Enable Set+Static and its narrow input permissions, restore the saved equivalent
primary Set, then verify both stores. Stop the historical CSE producer before
removing its six owned workload CSEs so it cannot recreate them. Never remove
the four bootstrap CSEs. Verify that the Set preserves the existing entries,
then restore the prior operator Deployment/RBAC and the intended Set-mode
configuration. Do not delete registrations or edit SPIRE databases directly.
Any deliberate identity, parent or selector change is outside this handoff:
the reconciler may delete obsolete entries before replacement creation.

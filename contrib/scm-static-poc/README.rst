Operator-produced static registrations
======================================

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
keep roots, Secrets, PVCs and durable image delivery. Keep all four bootstrap
declarations during restoration; minimizing them is a separate guarded phase.
The archived source already includes the same ztunnel SPIFFE trust-domain/xDS
support as the retained running agent source. No trust-domain backport or
dataplane downgrade is required.

Build only the generic operator with ``Dockerfile.operator``. It uses pinned
builder/runtime images and the established local build recipe, avoiding live
APK/gops installer downloads. Label the image with its exact clean source commit
and save its full index, amd64 manifest and config-digest chain in an offline
archive. Do not publish it.

Minimal bootstrap contract
--------------------------

The explicit Helm setting ``encryption.ztunnel.spire.bootstrapProfile=ztunnel``
requires ``encryption.ztunnel.spire.useCRD=true`` and selects exactly the
SPIRE agent node alias plus ztunnel. It does not replace the per-ServiceAccount
producer. Each enrolled Namespace's ``default``, ``app`` and ``unused``
ServiceAccounts still get independent literal CSEs with exactly
``k8s:ns:<namespace>`` and ``k8s:sa:<serviceaccount>`` selectors. Namespace/SA
lifecycle, producer ownership, names and guarded pruning are unchanged.
There is no Pod enumeration, Set enrollment or new SCM API.

``bootstrapProfile=legacy`` is the default and retains the Cilium-agent and
operator identities as well. Use it for other consumers of those identities.
In particular, the direct Entry API backend needs the operator's Workload API
identity and cannot use the ztunnel-only profile. The chart has no separate
existing setting that proves other consumers are absent, so ``useCRD`` alone
does not select the reduced bootstrap set. Socket mounts and shared
delegated/admin authorization remain unchanged.

The ztunnel profile derives its workload parent from the SPIRE agent alias.
``encryption.ztunnel.spire.parentID`` may explicitly repeat that alias, but a
different parent is rejected. In the legacy profile, the empty value retains
the historical ``/ztunnel`` parent.
``encryption.ztunnel.spire.className`` drives both the operator's existing
CRD-class flag and chart bootstrap/SCM configuration. A nonempty class makes
SCM watch that class only; an empty class retains classless defaults.
External SPIRE must implement the equivalent bootstrap/controller contract
itself; ``install.enabled=false`` does not install any SPIRE resources.

The full prefix key is
``authentication.mutual.spire.install.server.controllerManager.entryIDPrefix``.
Its default is ``cilium``. The POC must retain ``cilium-scm-sa-poc-1108`` and
must not introduce ``entryIDPrefixCleanup``.

For the POC, the desired values are ``bootstrapProfile=ztunnel``,
``parentID=spiffe://spiffe.cilium/ns/cilium-spire/sa/spire-agent`` and
``className=cilium-spire``. Both SCM sidecars retain Static reconciliation,
with Set and Pod reconciliation disabled. Existing binaries suffice; this
change does not require rebuilding or replacing any image.

The standard chart retains its historical resource names and cluster-qualified
PSAT selectors. The deployed POC instead has a ``...-spire-agent`` alias name,
two PSAT selectors, ``spire-server-crd`` resources and operator-created
bootstrap ownership. Those exact existing names, UIDs and specs must be
preserved through the compatibility migration below. Rendering the ordinary
chart is not evidence that it can safely upgrade this release.

Offline minimal-profile preparation
-----------------------------------

``prepare_minimal.py`` consumes a protected, restored four-bootstrap snapshot
and a read-only release capture. It never contacts Kubernetes, invokes Helm,
changes kubeconfig or stages images. Run it with Python 3 and PyYAML:

.. code-block:: bash

   python3 -B contrib/scm-static-poc/prepare_minimal.py \
     --baseline "$STABLE_SNAPSHOT" --release-capture "$PRIVATE_CAPTURE" \
     --output "$PRIVATE_PREPARATION"

The output must be outside the checkout and must not already exist. Files are
created with mode 0600 under private directories. **Keep the output private:
the compatibility charts preserve the existing Helm-owned Secrets and may
contain private keys. Never commit or publish them.**

The protected snapshot is the ``current.json`` shape from the bootstrap
minimality capture. It contains complete CSE specs/UIDs, all registrations
from both stores, component identities and protected root/storage/configuration
state. The preparation rejects temporary class exclusions, extra declarations,
missing workload entries, nonliteral selectors, wrong ownership, stale
``identityset.`` entries and mismatched controller scope.

The release capture contains:

* ``release-history.json`` from ``helm history -o json``.
* ``release-values.json`` from ``helm get values --revision 6 -o json``.
* ``release-manifest.yaml`` from ``helm get manifest --revision 6``.
* ``release-live-objects.json``: a JSON array containing a fresh full read of
  every object in that manifest, including its UID and resourceVersion.
* ``capture.json`` with the guarded endpoint, cluster UID, context, release
  ``cilium``, namespace ``kube-system``, revision ``6``, ``hooks: false`` and
  ``read_only: true``. Confirm ``helm get hooks --revision 6`` is empty.

Use only ``$HOME/.kube/scm-sa-poc-1108.config`` and context
``scm-sa-poc-1108`` for those reads. First verify the endpoint is
``https://aks-eermov-rg-eermovick-scm-9b8218-45zmt2zq.hcp.austriaeast.azmk8s.io:443``
and the kube-system UID is ``3d5ff43e-10de-4bfc-bb47-8c96859a42f0``.
Do not change the default kubeconfig or current context. For example, after
the endpoint/UID checks:

.. code-block:: bash

   umask 077
   K=(kubectl --kubeconfig "$HOME/.kube/scm-sa-poc-1108.config" --context scm-sa-poc-1108)
   H=(helm --kubeconfig "$HOME/.kube/scm-sa-poc-1108.config" --kube-context scm-sa-poc-1108)
   "${H[@]}" history cilium -n kube-system -o json > "$PRIVATE_CAPTURE/release-history.json"
   "${H[@]}" get values cilium -n kube-system --revision 6 -o json > "$PRIVATE_CAPTURE/release-values.json"
   "${H[@]}" get manifest cilium -n kube-system --revision 6 > "$PRIVATE_CAPTURE/release-manifest.yaml"
   "${K[@]}" get -f "$PRIVATE_CAPTURE/release-manifest.yaml" -o json |
     python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)["items"]))' \
       > "$PRIVATE_CAPTURE/release-live-objects.json"

Capture history again afterward and require the same deployed revision.
Do not reconstruct missing resources from revision 6. The generator requires
identical old/live object inventories, rejects registration Set/SA instances,
CRDs, PVCs and PVs in the release, and requires restored producer arguments,
Static-only RBAC and configuration. It compares all protected controller
specs, ConfigMaps and roles against the stable snapshot.

Prepared artifacts and offline checks
-------------------------------------

``minimal-release`` is a **frozen POC compatibility chart**, not a general
Cilium chart or an upgrade of SPIRE schemas. It contains the 41 existing
release resources with their complete normalized current content, plus the
two retained bootstrap CSEs with explicit Helm ownership. A complete target
comparison rejects any unrelated metadata/spec, image, RBAC, root or storage
change. The chart accepts only its recorded values and an upgrade of
``cilium`` in ``kube-system``; stale Set values and accidental installs fail.
Its values record the restored operator image and literal static mode, with
obsolete Set inputs removed.

``recovery-release`` contains the same unchanged existing resources and all
four original bootstrap specs. It supports a compensating forward transaction
from the actual restored baseline instead of replaying the old Set release.

``adopt/`` contains exact-name UID/resourceVersion/spec-tested JSON patches for
the two retained CSEs. ``remove/`` contains exact REST DELETE requests for only
the Cilium-agent and operator CSEs, including UID/resourceVersion preconditions
and expected specs. ``adopt-if-present-for-recovery/`` contains equivalent
ownership patches for the optional CSEs if recovery needs them while they still
exist. ``preflight.json`` records resource UIDs and normalized hashes.
``expected-registrations.json`` preserves all eight expected full registration
records on each independent store, not merely counts.

Render privately without a cluster and verify Helm's client-side merge:

.. code-block:: bash

   umask 077
   KUBECONFIG=/dev/null helm template cilium "$PRIVATE_PREPARATION/minimal-release" \
     -n kube-system --is-upgrade > "$PRIVATE_PREPARATION/minimal-release.rendered.yaml"
   GOMAXPROCS=4 go run ./contrib/scm-static-poc/verify_release.go \
     --previous "$PRIVATE_CAPTURE/release-manifest.yaml" \
     --live "$PRIVATE_CAPTURE/release-live-objects.json" \
     --target "$PRIVATE_PREPARATION/minimal-release.rendered.yaml" \
     --snapshot "$STABLE_SNAPSHOT"

The verifier uses Kubernetes strategic three-way merge for built-in resources
and Helm's client-side JSON merge behavior for existing CSEs. It requires zero
predicted changes to existing resources, no implicit deletions, and exact
bootstrap names/specs/ownership. Render ``recovery-release`` in the same way
and run the verifier with ``--recovery``. For recovery after a partial or
successful upgrade, use the actual latest recorded manifest and freshly read
release resources, not the revision-6 input. The retained bootstrap entries
must remain unchanged.

The verified strategy is explicitly ``--server-side=false`` with the installed
Helm 4 CLI. Do not silently use its automatic/server-side apply mode or force
conflict resolution. Offline merge verification cannot establish admission,
fresh attestation, runtime authentication or successful Helm adoption.

Separately gated live migration
------------------------------

Preparation is not live authorization. The live test worker owns all cluster
writes until coordinator handoff. Fresh SPIRE-agent attestation and temporary
pool/resource cleanup are required before rollout. The earlier alias+ztunnel
proof covered new delegates, fresh issuance, cross-node mTLS and natural
renewal, but did not by itself establish fresh node attestation.

1. Recheck the dedicated target, all protected state and original workloads,
   four complete bootstrap specs/UIDs, six workload CSEs, both stores and
   deployed release revision 6. Capture a fresh private checkpoint. Require
   the complete normalized release inventory to match ``preflight.json``.
   Regenerate stale resourceVersion guards from that approved checkpoint;
   investigate semantic or UID drift instead of weakening preconditions.
2. Apply only the two ``adopt/`` JSON patches using ``--type=json`` and exact
   resource names. Preserve their specs, UIDs and all workload ownership.
   Recheck those objects before Helm adoption. This transfers ownership to
   the existing ``cilium`` release; it is not permission to claim any other
   resource.
3. Perform the explicitly authorized forward transaction with the frozen
   ``minimal-release`` chart, ``--reset-values --server-side=false
   --skip-crds --no-hooks`` and the dedicated kubeconfig/context. Do not use
   ``--install``, ``--reuse-values``, ``--take-ownership``, replacement,
   force-conflict flags, ``--atomic`` or ``--rollback-on-failure``. Preserve
   existing workloads, images, sockets, Secrets, roots and PVCs. Check the
   stored chart, values and manifest against the prepared target.
4. Re-read each optional CSE and require the saved UID and complete spec.
   Submit only its prepared DELETE body with the freshly confirmed
   resourceVersion. If Kubernetes reports a conflict, stop and recheck;
   do not retry an unguarded name-only deletion. The required alias and
   ztunnel declarations must never be withdrawn.
5. Require exactly two bootstrap plus six workload CSEs, class ``cilium-spire``,
   unchanged workload names/owners/UIDs/specs and all eight expected full
   registrations on each store. Prove fresh issuance, authenticated cross-node
   traffic, renewal and unenrolled denial, and compare roots/storage and
   unaffected component state. Only then report minimal live adoption and
   Helm alignment.

If a transaction fails, **never roll back to revision 6** and never edit release
Secrets. That revision still describes Set mode and the old operator.
Use the prepared recovery chart as an explicit compensating forward
transaction after rechecking the latest actual release and resources.
If the optional CSEs still exist, regenerate their recovery ownership patches
from fresh reads before adopting them. If they were deleted, the recovery
chart recreates the exact names/specs with new UIDs; native bootstrap entry IDs
may also change. Retained bootstrap/workload UIDs and specs, the restored
producer, SCM class/prefix, roots, images and storage must not change.
Verify four bootstraps and ten registrations on each store. An interrupted,
pending Helm operation is a blocker for operator review, not authorization
to rewrite Helm Secrets or replay stale history.

This compatibility chart deliberately freezes the POC's existing footprint.
Future general-chart adoption requires a separate complete naming, ownership,
PSAT-selector, schema and runtime configuration review. Do not treat this
one migration as proof that an ordinary historical-chart upgrade is safe.

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

Historical restoration rollout
-------------------------------

This sequence documents the earlier Set-to-Static restoration, not the new
minimal-profile migration. Do not rerun it or re-enable Sets merely to reduce
the bootstrap count. No preparation command authorizes live mutation.

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

Historical reverse handoff
--------------------------

This is a deliberate return to the older Set producer, not recovery for the
minimal-profile migration. It requires its own separate authorization.

Retain the same SCM version, prefix, root and four bootstrap declarations.
Enable Set+Static and its narrow input permissions, restore the saved equivalent
primary Set, then verify both stores. Stop the historical CSE producer before
removing its six owned workload CSEs so it cannot recreate them. Never remove
the four bootstrap CSEs. Verify that the Set preserves the existing entries,
then restore the prior operator Deployment/RBAC and the intended Set-mode
configuration. Do not delete registrations or edit SPIRE databases directly.
Any deliberate identity, parent or selector change is outside this handoff:
the reconciler may delete obsolete entries before replacement creation.

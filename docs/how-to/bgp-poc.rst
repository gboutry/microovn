Attach BGP to externally managed OVN topology (PoC)
==================================================

``microovn enable bgp-poc`` reads the ``microovn-bgp-poc`` external ID on
``Open_vSwitch`` as one versioned JSON snapshot. It is mutually exclusive
with the existing ``bgp`` service. The topology owner must create the logical
router and external switch, connect their ports, pin the router to the local
chassis, configure dynamic routing, and arrange physical bridge mappings.

Example configuration::

    {
      "version": 1,
      "router_id": "192.0.2.10",
      "local_asn": 65101,
      "advertise_table": 42,
      "learn_table": 10,
      "uplinks": [{
        "bridge": "br-bgp",
        "router": "existing-router",
        "router_port": "existing-router-port",
        "switch": "existing-external-switch",
        "peers": [{"address": "fe80::1", "asn": 65002}]
      }]
    }

Each uplink can have multiple peers; additional uplinks are separate entries.
Peer link-local addresses are scoped to each attachment's interface. This PoC
supports IPv4 host-route advertisements over IPv6 link-local eBGP, imports only
the default route, and exports only /32 routes originating from the designated
advertisement kernel table. It does not provide IPv6 NLRI, BFD sessions, ECMP
configuration, per-peer policy selection, or distinct VRFs per uplink yet.
Unsupported JSON fields fail validation.

Exported host routes must have a kernel metric. BIRD copies that metric into
BGP MED so that OVN's route preference is preserved; routes without a metric
are rejected. No subnet allowlist is imposed. Fabric peers must compare MED
across neighboring ASNs when chassis use different ASNs (for BIRD, configure
``med metric on`` on the receiving BGP sessions). This preserves preference;
it does not itself enable ECMP or validate forwarding through a remote chassis.

The command checks router/chassis and router/port/switch relationships before
mutation. It adds a veth pair, an OVN switch port, routing-protocol redirect
options on the existing router port, and a route-learning port mapping. BIRD's
host-side veth shares the existing router port MAC and uses OVN's existing VRF.
No logical router is created and no custom OpenFlow command is issued.

The existing router's ``dynamic-routing-vrf-id`` must match ``learn_table``.
Its external port must already have ``dynamic-routing-port-name``. The external
topology owner is responsible for populating the advertisement table with the
correct chassis-local routes.

``microovn disable bgp-poc`` stops BIRD and removes the journaled attachment
resources while preserving external routers, router ports and switches. It
restores the previous BIRD file and prior per-uplink route mapping. Cleanup
refuses conflicting logical-port ownership. Do not run simultaneous external
writers for these attachment resources or mapping entries.

Without Neutron compatibility this is an explicit apply operation, not an
event-monitoring service. Repeating
``enable bgp-poc`` reapplies the same snapshot. Disable before changing the
snapshot. Startup activation attempts to reapply enabled configuration, but
full reboot ordering and automatic recovery after topology replacement are
not production guarantees. Reapply after the topology owner has restored the
prerequisites. A failed apply retains a journal for retry or explicit cleanup.

Only the redirect options and newly created attachment ports belong to this
mode. The topology owner's reconciler must preserve those options. Generic
mode contains no OpenStack resource names or Neutron-specific database keys.

Neutron topology compatibility
------------------------------

Add an optional ``neutron`` object to the same snapshot to replace the local
BGP extension while retaining Neutron's existing main/chassis routers and
two-localnet interconnect topology::

    "neutron": {
      "interconnect_bridge": "br-interconnect",
      "provider_network": "physnet1",
      "provider_switch": "existing-provider-switch",
      "interconnect_switch": "existing-interconnect-switch",
      "interconnect_router_port": "existing-main-router-interconnect-port"
    }

The provider and interconnect switch names and router port are explicit; no
Neutron naming convention is used to identify physical patch ports. The
interconnect bridge must already exist and contain only the two OVN patch
ports for these switches. MicroOVN validates their localnet metadata, network,
ofports and router MAC before installing the two interconnect forwarding rules.
It deletes or replaces only its own reserved flow cookie, never all bridge
flows. Conflicting priority-10 rules are rejected.

For migration, stop and disable the BGP extension first, then remove its old
custom bridge flows. Retain normal switching on the physical BGP bridges.
MicroOVN uses native BGP redirect there; it retains the interconnect bridge
and installs its MAC-rewrite/patch forwarding rules. Neutron's server-side
topology reconciler must remain running. No Neutron code or logical topology
change is required.

Compatibility mode also preserves/maintains the required bridge mappings and
publishes the configured uplink bridge list as ``neutron-bgp-bridges`` on the
local ``Chassis_Private`` row. Publication is repeated when the controller
recreates that row, allowing the unchanged Neutron reconciler to discover
the chassis's peer topology. Existing mapping entries using either ``:`` or
``=`` are preserved. Prior mappings and publication are journaled for cleanup.

In this mode the daemon retries reconciliation every two seconds, serializing
it with explicit apply and stopping the worker before disable. It reattaches
the veth to an OVN-recreated VRF, restores redirect options, and updates its
interconnect rules when patch ofports change. Failed initial apply retains its
journal and the worker retries while the daemon remains running. This polling
worker is experimental; cold-boot prerequisite ordering and automatic desired
snapshot updates still need deployment integration. Disable before changing
the snapshot. No simultaneous external writers are supported for the same
mapping keys, redirect options or reserved flow cookie.

The IPv4 host-route/default-only and IPv6 link-local peering limits above still
apply. This mode does not replace other ``ovn-agent`` extensions or Neutron's
server-side BGP service. Route eligibility and topology policy remain Neutron
and OVN responsibilities.

Reattaching a veth does not guarantee fast BGP convergence. After VRF
replacement a peer can retain the previous TCP session until its hold timer
expires and then apply its retry delay. This PoC does not configure BFD;
expect convergence to depend on the peers' failure-detection timers.

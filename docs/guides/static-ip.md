---
audience: operator
type: guide
---

# A fixed IP address for the cluster

This guide reserves the one public address a cluster answers on, before
Gentian OS is installed on it. It is the alternative to the tunnel of
[cloudflare.md](cloudflare.md): follow one of the two, not both.

It is written for someone who knows basic IT and has a Kubernetes cluster at
a cloud provider, with `kubectl` working on their machine.

## 1. Tunnel or fixed address

| | Tunnel | Fixed address |
|---|---|---|
| The cluster needs | no public address and no open port | one public address, and a load balancer that answers on it |
| DNS | at Cloudflare | at any provider the platform supports ([cluster-requirements.md §4](cluster-requirements.md#4-the-dns-provider)), or kept by hand |
| Carries | HTTP and HTTPS | any protocol: the platform's own mail server needs this mode |
| Fits | a machine at home or in an office | a cluster at a cloud provider |

With a fixed address, the installer creates one `LoadBalancer` Service for the
edge, the cloud's load balancer answers on your address and passes traffic to
it, and every name of the platform points at that address.

**Reserve the address before you install.** The edge takes its address when
its Service is created and keeps it. An address the cloud picks by itself can
be lost when the cluster is rebuilt, and every DNS record with it.

## 2. OpenStack

On OpenStack a public address is a **floating IP**, and the load balancer is
**Octavia**. This section holds for every cluster whose machines run on
OpenStack, including a managed Kubernetes built on it and Infomaniak Public
Cloud.

### 2.1 Reserve a floating IP

In the dashboard (Horizon), signed in to the project the cluster runs in:

1. Open **Network → Floating IPs** and choose **Allocate IP To Project**.
2. **Pool:** the external network, the one the cluster's load balancers use.
   Most projects offer one.
3. Choose **Allocate IP**. The list shows the new address with nothing in the
   column **Mapped Fixed IP Address**. Leave it that way: the installer
   attaches it.

Or with the `openstack` command-line tool:

```bash
openstack network list --external                 # the name of the external network
openstack floating ip create --description "gentian edge" <external network>
openstack floating ip list                        # the new address, with no port
```

Note the address, for example `203.0.113.10`.

The address has to be in the same project as the cluster, and unused. If the
allocation is refused, the project's quota for floating IPs is used up; your
provider raises it.

### 2.2 What to answer in the installer

When step 4 of [GETTING-STARTED](../../GETTING-STARTED.md#4-install) asks
what the cluster is:

| Question | Answer |
|---|---|
| `networkMode` | `static-ip` |
| `NODE_IP` (the address DNS resolves to) | the floating IP, as an address, not a name |
| `platform` | leave it empty: the installer detects `openstack` from the cluster's nodes |

The installer gives the edge's Service that address and two settings for
Octavia: the floating IP is kept when the Service is deleted, so that a
reinstall takes the same address again, and the load balancer checks which
nodes answer before it sends traffic to them
([cluster-requirements.md §3](cluster-requirements.md#3-the-platform)).

### 2.3 DNS

The platform's names have to point at the address.

- **With a DNS provider and its credential** (step 3 of
  [GETTING-STARTED](../../GETTING-STARTED.md#3-have-the-credentials-ready)),
  the cluster publishes every record itself and the install waits for them.
- **Without one**, create the records yourself before the install:
  `<domain>`, `*.<domain>`, `*.platform.<domain>` and, on a multi-tenancy
  cluster, `*.<tenant>.<domain>` for each tenant, all as `A` records with the
  floating IP ([cluster-requirements.md §4.2](cluster-requirements.md#42-records-kept-by-hand)).

### 2.4 Check it

After the install:

```bash
kubectl get svc -n kernel-edge
```

One Service has the type `LoadBalancer`, and its `EXTERNAL-IP` is your
floating IP. In OpenStack, the floating IP is now mapped to the load
balancer:

```bash
openstack floating ip show 203.0.113.10          # port_id is set
openstack loadbalancer list                      # one balancer, ACTIVE and ONLINE
```

From any machine, `curl -sI https://platform.<domain>/` answers.

If `EXTERNAL-IP` stays `<pending>`, `kubectl describe svc -n kernel-edge
<name>` shows the reason at the end. The usual ones: the address is in
another project, it is already mapped to something else, or it belongs to a
different external network than the one the cluster's load balancers use.

### 2.5 Giving the address back

The floating IP stays reserved after the cluster is uninstalled, and your
provider may go on charging for it. Release it when you no longer need it:

```bash
openstack floating ip delete 203.0.113.10
```

## 3. Mail on a fixed address

A cluster that runs the platform's own mail server needs more from the
provider than the address: its reverse DNS, an address to send from, and
outbound port 25.
[mail.md §9c](../concepts/mail.md#9c-installing-a-cluster-with-its-own-mail-what-is-yours-to-do)
has the list. Skip it if the cluster relays its mail.

## 4. Next

The repository that holds the cluster's definition:
[github-deployments.md](github-deployments.md).

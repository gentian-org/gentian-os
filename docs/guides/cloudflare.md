---
audience: operator
type: guide
---

# A domain, a tunnel and a token from Cloudflare

This guide sets up the three things a cluster needs from Cloudflare before
Gentian OS is installed on it:

- **A domain** whose DNS Cloudflare runs. People reach the platform under it.
- **A tunnel.** A small program in the cluster, `cloudflared`, opens a
  connection out to Cloudflare, and visitors' traffic comes in over it. The
  cluster needs no public address and no open port on your router.
- **An API token.** With it the platform creates its own DNS records and tells
  the tunnel which name goes where, so you never edit either by hand.

A cluster at a cloud provider can take a fixed IP address instead of the
tunnel: [static-ip.md](static-ip.md).

It is written for someone who knows basic IT and has not used Kubernetes
before. You need the cluster from [microk8s.md](microk8s.md), with `kubectl`
working on your machine.

## 1. A Cloudflare account and a domain

1. Create an account at [dash.cloudflare.com](https://dash.cloudflare.com).
2. Add a domain to it, in one of two ways:
   - **Buy one at Cloudflare:** **Domain Registration → Register Domains**.
     Its DNS is at Cloudflare from the start.
   - **Bring one you own:** **Add a domain**, enter it, then change its
     nameservers at the registrar you bought it from to the two Cloudflare
     shows. It is ready when Cloudflare reports the domain as **Active**,
     often within an hour and at most a day.
3. Use the domain itself as the platform's domain, for example `example.com`,
   when the installer asks for it. Use it for nothing else: the platform
   publishes many names under it.

### 1.1 Certificates for names two levels deep

Cloudflare's free certificate covers the domain and names one level below
it, such as `desktop.example.com`. A name two levels below, such as
`admin.platform.example.com`, gets a certificate error unless the domain has
**Total TLS**. How much of the platform that affects depends on the tenancy
mode you choose in step 4 of
[GETTING-STARTED](../../GETTING-STARTED.md#4-install):

| Tenancy mode | Names two levels deep | Without Total TLS |
|---|---|---|
| `single` | The platform administrator's console, `admin.platform.<domain>`, and the Operations Console beside it | Everything people use works: the desktop, the tenant's administration console, the App Store and every app. The platform administrator signs in at `platform.<domain>`, and those two consoles show a certificate error |
| `multi` | Every tenant's desktop and apps, `<app>.<tenant>.<domain>` | No tenant is reachable |

This holds when the platform's domain is the domain itself, `example.com`.
With a subdomain, `gentian.example.com`, every name is one level deeper and
all of them need Total TLS.

To turn it on, open **SSL/TLS → Edge Certificates → Total TLS** for the
domain. Total TLS needs **Advanced Certificate Manager**, a paid add-on that
the same page offers. With it on, Cloudflare issues a certificate for every
name the platform publishes, a few minutes after the name appears.

## 2. Create the tunnel

1. In the Cloudflare dashboard, open **Zero Trust**, then **Networks →
   Tunnels**, and choose **Create a tunnel**.
2. Choose **Cloudflared** as the connector type and give the tunnel a name,
   for example the cluster's name.
3. The next page shows commands to install the connector. You do not run any
   of them. Each one ends in `--token` and a long string starting with `eyJ`:
   copy that string. It is the **tunnel token**.
4. Leave the page that asks for public hostnames or routes empty and finish.
   The platform writes the tunnel's routes itself.

The tunnel shows as **Inactive** until `cloudflared` runs in the next step.

## 3. Run `cloudflared` in the cluster

Store the tunnel token in the cluster. The installer reads it from exactly
this Secret, `cf-tunnel` in the namespace `default`:

```bash
kubectl create secret generic cf-tunnel -n default \
    --from-literal=token='<the tunnel token>'
```

Then start `cloudflared` with it. This creates two copies, so the tunnel stays
up while one restarts:

```bash
kubectl apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cloudflared
  namespace: default
spec:
  replicas: 2
  selector:
    matchLabels:
      app: cloudflared
  template:
    metadata:
      labels:
        app: cloudflared
    spec:
      securityContext:
        runAsNonRoot: true
      containers:
        - name: cloudflared
          image: cloudflare/cloudflared:2026.10.7
          args: ["tunnel", "--no-autoupdate", "--metrics", "0.0.0.0:2000", "run"]
          env:
            - name: TUNNEL_TOKEN
              valueFrom:
                secretKeyRef:
                  name: cf-tunnel
                  key: token
          livenessProbe:
            httpGet:
              path: /ready
              port: 2000
            initialDelaySeconds: 10
            periodSeconds: 10
            failureThreshold: 1
EOF
```

Check it:

```bash
kubectl get pods -n default -l app=cloudflared
kubectl logs -n default -l app=cloudflared --tail=20
```

Both pods are `Running`, the log says `Registered tunnel connection`, and the
tunnel in the dashboard turns **Healthy**. Nobody can reach anything through
it yet: the platform adds the routes during the install.

## 4. Create the API token

1. In the Cloudflare dashboard, open your profile (top right) → **My Profile →
   API Tokens**, choose **Create Token**, then **Create Custom Token**.
2. Name it, for example `gentian-<cluster name>`.
3. Under **Permissions**, add these three rows:

   | | | |
   |---|---|---|
   | Account | Cloudflare One Connector: cloudflared | Edit |
   | Zone | Zone | Read |
   | Zone | DNS | Edit |

   Older accounts show **Argo Tunnel** instead of **Cloudflare One Connector:
   cloudflared**; it is the same permission.
4. Under **Account Resources**, include your account. Under **Zone
   Resources**, include **Specific zone** and your domain.
5. Choose **Continue to summary**, then **Create Token**, and copy the token.
   Cloudflare shows it once; keep it in a password manager.

The installer checks every permission when you enter the token. It asks for
it twice, once as the DNS provider credential and once as the Cloudflare
Tunnel API token (step 3 of
[GETTING-STARTED](../../GETTING-STARTED.md#3-have-the-credentials-ready)):
enter the same token both times. [routing.md §3a](../concepts/routing.md)
says what each permission is used for.

## 5. What to answer in the installer

When step 4 of [GETTING-STARTED](../../GETTING-STARTED.md#4-install) asks
what the cluster is, these answers match this guide:

| Question | Answer |
|---|---|
| The domain | your domain, for example `example.com` |
| `networkMode` | `tunnel` |
| The certificate issuer | `acme-dns01` |
| The DNS provider | `cloudflare` |

## 6. Next

Last, the repository that holds the cluster's definition:
[github-deployments.md](github-deployments.md).

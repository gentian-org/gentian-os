---
audience: operator
type: guide
---

# Setting up a cluster with MicroK8s

This guide turns one computer into a Kubernetes cluster that Gentian OS can be
installed on, and sets that computer up with the tools the installer runs. It
is written for someone who knows basic IT and has not used Kubernetes before.

There are two options. Pick the one that matches your machine:

- [§2 An Ubuntu machine](#2-option-a-an-ubuntu-machine): a PC or server
  running Ubuntu.
- [§3 A Mac mini](#3-option-b-a-mac-mini): MicroK8s runs in an Ubuntu virtual
  machine on the Mac.

Both end in the same place: `kubectl get nodes` shows one node that is
`Ready`, and [§4](#4-the-tools-the-installer-runs) has installed the tools.

A few words used below:

| Word | Meaning |
|---|---|
| Kubernetes | Software that runs programs in containers and keeps them running |
| Cluster | One or more machines that Kubernetes runs on, managed as one |
| Node | One machine in the cluster. This guide's cluster has one |
| Pod | One running program (one or more containers) in the cluster |
| MicroK8s | A small Kubernetes distribution from Canonical that installs with one command |
| `kubectl` | The command-line tool that talks to the cluster |

## 1. What the computer needs

Gentian OS runs about 100 pods before any app is installed, and reserves 4 to
6 CPU cores and 9 to 16 GiB of memory for them
([cluster-requirements.md §2](cluster-requirements.md#2-size)). That sets the
minimum:

| | Ubuntu machine | Mac mini |
|---|---|---|
| Processor | 64-bit, x86_64 or arm64, at least 8 cores | Apple silicon, at least 10 cores |
| Memory | at least 32 GB | at least 32 GB |
| Disk | at least 200 GB, SSD | at least 200 GB free, internal SSD |
| System | Ubuntu 24.04 LTS, Server or Desktop, freshly installed | a current macOS |

Use a machine that does nothing else, and connect it to the network by cable.
It has to stay on: the cluster is down whenever it sleeps.

A home network is enough. With the Cloudflare tunnel
([cloudflare.md](cloudflare.md)) the cluster connects out to Cloudflare, so
nothing on your router has to change and the machine needs no public address.

## 2. Option A: an Ubuntu machine

Run every command in a terminal on the Ubuntu machine, as a user that can use
`sudo`.

### 2.1 Install MicroK8s

```bash
sudo snap install microk8s --classic --channel=1.35/stable
sudo usermod -a -G microk8s "$USER"
mkdir -p ~/.kube && chmod 0700 ~/.kube
```

Log out and log in again, so that your user belongs to the group `microk8s`.
Then wait for MicroK8s to start and switch on the three add-ons Gentian OS
needs: cluster DNS, access control (RBAC), and storage on the machine's disk.

```bash
microk8s status --wait-ready
microk8s enable dns rbac hostpath-storage
```

Any channel from `1.33/stable` to `1.36/stable` works; these are the
Kubernetes versions the platform is tested on.

### 2.2 Let the node run enough pods

A MicroK8s node runs at most 110 pods. Gentian OS needs more, so raise the
limit to 220:

```bash
echo '--max-pods=220' | sudo tee -a /var/snap/microk8s/current/args/kubelet
sudo snap restart microk8s
microk8s status --wait-ready
```

The installer does this itself when it runs on this same machine. Doing it now
does no harm, and it is needed when the installer runs somewhere else.

### 2.3 Encrypt Secrets at rest

Kubernetes keeps passwords and keys as Secrets in its database. Unless it is
told otherwise, it writes them to disk unencrypted. These commands create an
encryption key and tell Kubernetes to encrypt every Secret with it:

```bash
KEY="$(head -c 32 /dev/urandom | base64)"
sudo tee /var/snap/microk8s/current/args/encryption-config.yaml >/dev/null <<EOF
apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources:
  - resources: ["secrets"]
    providers:
      - secretbox:
          keys:
            - name: key1
              secret: ${KEY}
      - identity: {}
EOF
unset KEY
sudo chmod 600 /var/snap/microk8s/current/args/encryption-config.yaml
echo '--encryption-provider-config=/var/snap/microk8s/current/args/encryption-config.yaml' \
    | sudo tee -a /var/snap/microk8s/current/args/kube-apiserver
sudo snap restart microk8s
microk8s status --wait-ready
```

Never delete the file `encryption-config.yaml`: without it the cluster cannot
read its own Secrets. When the installer asks for
`controlPlane.secretsAtRest` (step 4 of
[GETTING-STARTED](../../GETTING-STARTED.md#4-install)), answer `cluster`.

### 2.4 Give `kubectl` access to the cluster

```bash
sudo snap install kubectl --classic --channel=1.35/stable
microk8s config > ~/.kube/config
chmod 600 ~/.kube/config
kubectl get nodes
```

`microk8s config > ~/.kube/config` replaces an existing `~/.kube/config`. If
you already have one for another cluster, save it under another name first.

The last command shows one node with the status `Ready`. Go on with
[§4](#4-the-tools-the-installer-runs).

## 3. Option B: a Mac mini

On a Mac, MicroK8s runs inside an Ubuntu virtual machine that
[Multipass](https://multipass.run) manages. The virtual machine is called
`microk8s-vm`. Run every command in the Terminal app on the Mac unless the step
says otherwise.

### 3.1 Install MicroK8s

Install [Homebrew](https://brew.sh) if the Mac does not have it yet. Then:

```bash
brew install --cask multipass
brew install ubuntu/microk8s/microk8s
microk8s install --channel=1.35/stable --cpu=8 --mem=24 --disk=150
microk8s status --wait-ready
microk8s enable dns rbac hostpath-storage
```

`microk8s install` creates the virtual machine with 8 CPU cores, 24 GB of
memory and 150 GB of disk, which leaves the rest of the Mac for macOS.

So the Mac stays on, open **System Settings → Energy** and turn on **Prevent
automatic sleeping when the display is off** and **Start up automatically
after a power failure**. After the Mac restarts, `multipass list` shows
whether `microk8s-vm` is `Running`; if it is not, start it with
`multipass start microk8s-vm`.

### 3.2 Pods and encryption, inside the virtual machine

The pod limit and the Secret encryption are set inside the virtual machine.
The installer runs on the Mac and cannot change them itself, so this step is
required. Open a shell in the virtual machine:

```bash
multipass shell microk8s-vm
```

Run the commands of [§2.2](#22-let-the-node-run-enough-pods) and
[§2.3](#23-encrypt-secrets-at-rest) there, exactly as written, then type
`exit` to return to the Mac.

### 3.3 Give `kubectl` access to the cluster

```bash
brew install kubectl
mkdir -p ~/.kube && chmod 0700 ~/.kube
microk8s config > ~/.kube/config
chmod 600 ~/.kube/config
kubectl get nodes
```

As on Ubuntu, this replaces an existing `~/.kube/config`. The last command
shows one node with the status `Ready`.

Use the tunnel ([cloudflare.md](cloudflare.md)) to reach this cluster: the
virtual machine has no address that the internet can reach.

## 4. The tools the installer runs

Install these on the machine you run the installer from: the Ubuntu machine
itself, or the Mac.

On Ubuntu:

```bash
sudo apt update
sudo apt install -y git gpg jq curl openssl python3 make age qrencode
sudo snap install helm --classic
ARCH="$(dpkg --print-architecture)"    # amd64 or arm64
sudo curl -fsSL -o /usr/local/bin/yq \
    "https://github.com/mikefarah/yq/releases/latest/download/yq_linux_${ARCH}"
sudo chmod +x /usr/local/bin/yq
```

On the Mac:

```bash
brew install helm jq yq git gnupg python age qrencode
```

Then, on either, get Gentian OS and let it install the Crossplane command-line
tool, which is the one tool left:

```bash
git clone https://github.com/gentian-org/gentian-os.git
cd gentian-os
make install-tools
```

The installer's pre-flight checks every tool again and names any that is
missing.

## 5. Next

The cluster is ready. Next, give it a domain and a way in:
[cloudflare.md](cloudflare.md).

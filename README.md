# kutils <!-- omit from toc -->

A collection of simple Kubernetes CLI utilities to save some keystrokes.

This repo is designed t.  with links to others that I regularly use1. 

- [Installation](#installation)
  - [Homebrew (macOS and Linux)](#homebrew-macos-and-linux)
  - [go install](#go-install)
  - [Build from source](#build-from-source)
- [kgc](#kgc)
  - [Features](#features)
  - [Usage](#usage)
- [kgpv](#kgpv)
  - [Features](#features-1)
  - [Usage](#usage-1)
- [ksd](#ksd)
  - [Features](#features-2)
  - [Usage](#usage-2)
  - [Shell completion](#shell-completion)
- [kgn](#kgn)
  - [Features](#features-3)
  - [Usage](#usage-3)
  - [Shell completion](#shell-completion-1)
- [Requirements](#requirements)
- [License](#license)


## Tools <!-- omit from toc -->

- **[kgc](#kgc)** — `kubectl get pods` with color-coded status and last restart reason
- **[kgpv](#kgpv)** — List PersistentVolumes with bound PVCs, pods, zones, storageClass and sizes
- **[ksd](#ksd)** — Kubernetes secret decoder: `kubectl get secret -o yaml` with the values already base64-decoded
- **[kgn](#kgn)** — Node CPU, memory and GPU capacity, requests and usage, with color-coded health and a `--detail` view explaining why a node is unhealthy

## Installation

Potential for conflicting aliases:

If you use many kubectl aliases (like with oh-my-zsh), you may already have a `kgpv` alias.
To check: 

```sh
alias | grep kgpv
```

To unalias for your current session:

```sh
unalias kgpv
```

To make it permanent, add this to your ~/.zshrc:

```sh
echo "unalias kgpv" >> ~/.zshrc
```

### Homebrew (macOS and Linux)

```sh
brew tap jessegoodier/kutils
brew install jessegoodier/kutils/kgc
brew install jessegoodier/kutils/kgpv
brew install jessegoodier/kutils/ksd
brew install jessegoodier/kutils/kgn
```

### go install

```sh
go install github.com/jessegoodier/kutils/cmd/kgc@latest
go install github.com/jessegoodier/kutils/cmd/kgpv@latest
go install github.com/jessegoodier/kutils/cmd/ksd@latest
go install github.com/jessegoodier/kutils/cmd/kgn@latest
```

### Build from source

```sh
git clone https://github.com/jessegoodier/kutils.git
cd kutils
go build -o kgc ./cmd/kgc
go build -o kgpv ./cmd/kgpv
go build -o ksd ./cmd/ksd
go build -o kgn ./cmd/kgn
```

---

## kgc

Like `kubectl get pods`, but focused on containers and highlighting the last restart reason.

```
kgc -A
NAMESPACE    NAME                                        READY  STATUS     RESTARTS  AGE    LAST RESTART REASON
monitoring   prometheus-kube-prometheus-stack-0          2/2    Running    0         2d23h
opencost     opencost-9fcf95fc8-6tns1                    2/2    Running    3         2d23h  opencost: Error
```

### Features

- Color-coded status: green (healthy), yellow (pending/not-ready), red (failed)
- Watch mode for real-time updates (`-w`)
- All-namespaces view (`-A`)
- Filter to only pods with restarts (`-r`)
- Shows last restart reason per container
- Adapts column widths dynamically

### Usage

```
kgc [flags]

Flags:
  -n, --namespace <namespace>  Namespace to list pods in.  (default: current context)
  -A                           List pods across all namespaces.
  -w, --watch                  Watch for pod changes.
  -r, --has-restarts           Only show pods with restarts.
  --show-image                 Show container image column.
  --kubeconfig <path>          Path to kubeconfig file.
  -v, --version                Print version and exit.
  -h, --help                   Show this help message.
```

---

## kgpv

List PersistentVolumes with their bound PVCs, using pods, availability zones, and sizes.

```
kgpv
NAMESPACE    POD                    ZONE          SIZE  PVC
production   api-server-abc123      us-east-1a    10Gi  api-data
production   worker-xyz789          us-east-1b    50Gi  worker-storage
```

### Features

- Shows PV → PVC → Pod relationship at a glance
- Extracts zone from node affinity or labels
- Converts storage sizes to Gi format
- Color-coded output per column type
- Concurrent PV processing for speed
- Dynamic column width adaptation

### Usage

```
kgpv [flags]

Flags:
  --hide-pod             Hide the pod column.
  --show-pv-name         Show the PV name column.
  --show-reclaim-policy  Show the reclaim policy column.
  --show-all-fields           Show all columns.
  --sort <column>        Sort by column: namespace, pod, zone, size, pvc, pv.  (default: namespace)
  --output <format>      Output format: csv, json, yaml.
  -v, --version          Print version and exit.
```

---

## ksd

Like `kubectl get secret NAME -o yaml`, but the values are base64-decoded for you, with tab completion for secret names.

```
ksd -n kube-system bootstrap-token-abc123
# kube-system/bootstrap-token-abc123 · bootstrap.kubernetes.io/token · 4 keys
apiVersion: v1
data:
  token-id: abc123
  token-secret: 0123456789abcdef
kind: Secret
metadata:
  name: bootstrap-token-abc123
  namespace: kube-system
type: bootstrap.kubernetes.io/token
```

### Features

- Decoded values in YAML form; multi-line values (PEM, kubeconfigs) use block scalars
- Certificates are annotated with subject, issuer, validity dates, SANs and time until expiry (green / yellow under 30 days / red expired)
- Binary values are shown as `<binary N bytes>` instead of garbling the terminal
- Metadata is trimmed to name, namespace and type; `--full` adds labels, annotations, uid and timestamps
- `ksd` without a name lists the secrets in the namespace (name, type, number of keys, age)
- `-i` picks the secret with [fzf](https://github.com/junegunn/fzf) when it is installed
- `-k KEY` prints one raw decoded value, handy for scripts and the clipboard
- `-o yaml|json` for plain machine-readable output (binary values go under `binaryData`)
- Colors adapt to the terminal: disabled when piped, with `--no-color`, or when `NO_COLOR` is set; tables and certificate details shrink to the terminal width

### Usage

```
ksd [secret] [flags]

Flags:
  -n, --namespace <namespace>  Namespace of the secret.  (default: current context)
  -k, --key <key>              Print only the raw decoded value of this key.
  -o, --output <format>        Output format: yaml or json (plain, no color).
  -i, --interactive            Pick the secret with fzf.
  --full                       Include labels, annotations, uid and timestamps.
  --no-color                   Disable colored output (NO_COLOR is also honored).
  --kubeconfig <path>          Path to kubeconfig file.  (default: $KUBECONFIG or ~/.kube/config)
  --completions <shell>        Print shell completion script (bash or zsh).
  -v, --version                Print version and exit.
  -h, --help                   Show this help message.
```

### Shell completion

Completion looks up the cluster on every Tab press, so it follows `-n` and `--kubeconfig` as you type:

```
ksd -n kube-system <TAB>      # secrets in kube-system
ksd -n <TAB>                  # namespaces
ksd my-secret -k <TAB>        # keys of that secret
```

Enable it in your shell startup file:

```sh
# bash (~/.bashrc)
source <(ksd --completions bash)

# zsh (~/.zshrc, after compinit)
source <(ksd --completions zsh)
```

zsh users with [fzf-tab](https://github.com/Aloxaf/fzf-tab) get the fuzzy picker on Tab automatically; everyone else can use `ksd -i`.

---

## kgn

Like `kubectl get nodes`, but with CPU, memory and GPU totals, requests and real usage per node, color-coded health, and a `--detail` view that explains *why* a node is unhealthy.

```
kgn
NAME          STATUS                      ROLES   CPU REQ        CPU USE  MEM REQ            MEM USE  GPU         PODS    AGE  VERSION
ip-10-0-1-23  Ready                       <none>  3.2/7.9 (41%)  27%      11.2Gi/30Gi (37%)  22%      -           28/110  41d  v1.34.2
ip-10-0-1-88  Ready                       <none>  6/7.9 (76%)    61%      20Gi/60Gi (33%)    30%      2/4 nvidia  14/110  12d  v1.34.2
ip-10-0-2-17  Unknown,SchedulingDisabled  <none>  15.1/16 (95%)  -        54Gi/62Gi (87%)    -        -           54/58   2d   v1.32.3
Total: 3 nodes (1 not ready, 1 cordoned, 1 with issues) · CPU 24.3/31.8 requested (76%), 35% used · MEM 85.2Gi/152Gi requested (56%), 21% used · nvidia 2/4
```

### Features

- **Requested** is computed like `kubectl describe node`: the node's non-terminated pods, counting init containers, sidecars, pod-level resources and pod overhead
- **Usage** comes from metrics-server (`kubectl top node`) when it is installed; the USE columns are hidden when it isn't
- **GPUs and accelerators** are detected from extended resources: NVIDIA (including MIG slices), AMD, Intel, AWS Neuron, Google TPU and Habana Gaudi. The GPU column only appears when a node has one
- **Health colors**:
  - STATUS: red for NotReady/Unknown, `NetworkUnavailable`, or `unreachable` / `not-ready` / `out-of-service` taints; yellow for cordoned nodes, Memory/Disk/PID pressure, node-problem-detector conditions, and cluster-autoscaler or Karpenter removal
  - CPU/MEM: yellow above 80% requested or used, red above 95%
  - PODS: yellow at 90% of the node's pod limit, red when full
  - VERSION: yellow when the kubelet lags the control plane, red outside the supported skew (more than 3 minor versions behind, or newer)
  - GPU: yellow when the device plugin reports unhealthy devices (allocatable below capacity), or when a node is labelled with NVIDIA GPUs but advertises none
- `-d` / `--detail` adds a section per node: every health issue with its reason and message, conditions and last heartbeat, taints, a capacity / allocatable / requests / limits / available / usage table for every resource, GPU model, kubelet and runtime versions, OS and kernel, cloud metadata, addresses, and recent Warning events
- `-u` shows only nodes with a health problem; combine with `-d` to see why: `kgn -u -d`
- `-o wide` splits each resource into REQ / LIM / ALLOC / AVAIL / USE columns and adds GPU model, instance type, capacity type (spot / on-demand), zone, node pool and internal IP (EKS, Karpenter, GKE and AKS labels)
- Filter by node name (`kgn node-1 node-2`) or label selector (`-l`), and sort with `--sort cpu|mem|gpu|pods|age|name`
- Colors are disabled when piped, with `--no-color`, or when `NO_COLOR` is set

### Usage

```
kgn [node...] [flags]

Flags:
  -l, --selector <selector>  Label selector to filter nodes (e.g. role=gpu).
  -d, --detail               Show why a node is unhealthy plus conditions, taints, resources, events.
  -u, --unhealthy            Only show nodes with a health problem.
  -o, --output <format>      Output format: wide (split resource columns, cloud info).
  --sort <key>               Sort by name, cpu, mem, gpu, pods, age (cpu/mem/gpu/pods: highest first).  (default: name)
  --no-color                 Disable colored output (NO_COLOR is also honored).
  --kubeconfig <path>        Path to kubeconfig file.  (default: $KUBECONFIG or ~/.kube/config)
  --completions <shell>      Print shell completion script (bash or zsh).
  -v, --version              Print version and exit.
  -h, --help                 Show this help message.
```

`kgn` needs permission to list nodes and pods cluster-wide. Usage needs read access to `metrics.k8s.io` and `--detail` reads events; when either is missing, `kgn` leaves that information out.

### Shell completion

Tab completes node names from the cluster (following `-l` and `--kubeconfig`), plus `--sort` and `-o` values:

```sh
# bash (~/.bashrc)
source <(kgn --completions bash)

# zsh (~/.zshrc, after compinit)
source <(kgn --completions zsh)
```

---

## Requirements

- A valid kubeconfig with cluster access

## License

MIT

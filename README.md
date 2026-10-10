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
- [Requirements](#requirements)
- [License](#license)


## Tools <!-- omit from toc -->

- **[kgc](#kgc)** — `kubectl get pods` with color-coded status and last restart reason
- **[kgpv](#kgpv)** — List PersistentVolumes with bound PVCs, pods, zones, storageClass and sizes
- **[ksd](#ksd)** — Kubernetes secret decoder: `kubectl get secret -o yaml` with the values already base64-decoded

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
```

### go install

```sh
go install github.com/jessegoodier/kutils/cmd/kgc@latest
go install github.com/jessegoodier/kutils/cmd/kgpv@latest
go install github.com/jessegoodier/kutils/cmd/ksd@latest
```

### Build from source

```sh
git clone https://github.com/jessegoodier/kutils.git
cd kutils
go build -o kgc ./cmd/kgc
go build -o kgpv ./cmd/kgpv
go build -o ksd ./cmd/ksd
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

## Requirements

- A valid kubeconfig with cluster access

## License

MIT

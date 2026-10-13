package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"os"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gookit/color"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var version = "dev" // overridden by ldflags at release build time; falls back to module version via ReadBuildInfo

func init() {
	if version == "dev" {
		if info, ok := debug.ReadBuildInfo(); ok {
			v := info.Main.Version
			if v != "" && v != "(devel)" {
				version = strings.TrimPrefix(v, "v")
			}
		}
	}
}

func die(msg string, err error) {
	fmt.Fprintf(os.Stderr, "error: %s: %v\n", msg, err)
	os.Exit(1)
}

// Palette used for terminal output.
var (
	styleTitle  = color.Style{color.Bold, color.FgWhite}
	styleSect   = color.Style{color.Bold, color.FgCyan}
	styleKey    = color.Style{color.FgCyan}
	styleMeta   = color.Style{color.FgGray}
	stylePlain  = color.Style{color.FgWhite}
	styleOK     = color.Style{color.FgGreen}
	styleWarn   = color.Style{color.FgYellow}
	styleBad    = color.Style{color.Bold, color.FgRed}
	styleAccent = color.Style{color.FgMagenta}
)

// Thresholds (percent) for storage health and resource coloring.
const (
	ephemeralWarnPct = 75
	warnPct          = 80
	critPct          = 95
	// Pod-count thresholds: a full node can't take more pods regardless of CPU/memory.
	podsWarnPct = 90
	podsCritPct = 100
	// maxKubeletSkew is the number of minor versions a kubelet may lag the API server.
	maxKubeletSkew = 3
	// maxEvents is how many recent Warning events --detail shows per node.
	maxEvents = 5
)

type level int

const (
	lvlOK level = iota
	lvlWarn
	lvlCrit
)

func (l level) style() color.Style {
	switch l {
	case lvlCrit:
		return styleBad
	case lvlWarn:
		return styleWarn
	default:
		return styleOK
	}
}

func (l level) marker() string {
	switch l {
	case lvlCrit:
		return "✗"
	case lvlWarn:
		return "!"
	default:
		return "✓"
	}
}

type issue struct {
	level level
	text  string
	label string // concise table status; text retains the detailed explanation
}

// amount holds one resource's numbers for a node. CPU is in millicores, every
// other resource in its base unit (bytes, count).
type amount struct {
	capacity, alloc, req, lim, used int64
	hasUsed                         bool
}

func (a amount) avail() int64 { return max(a.alloc-a.req, 0) }

type accel struct {
	name corev1.ResourceName
	amount
}

type cloudInfo struct {
	instanceType, capacityType, zone, pool string
}

// filesystemUsage describes nodefs, independently of scheduler reservations.
type filesystemUsage struct {
	Capacity  int64  `json:"capacityBytes"`
	Used      *int64 `json:"usedBytes"`
	Available *int64 `json:"availableBytes"`
}

func (f *filesystemUsage) valid() bool {
	return f != nil && f.Capacity > 0 && f.Used != nil && f.Available != nil &&
		*f.Used >= 0 && *f.Used <= f.Capacity && *f.Available >= 0 && *f.Available <= f.Capacity
}

func (f *filesystemUsage) low() bool {
	// Compare without rounding percentages or overflowing large byte counts.
	return f.valid() && f.Capacity-*f.Available > f.Capacity/100*ephemeralWarnPct+f.Capacity%100*ephemeralWarnPct/100
}

func storageCells(f *filesystemUsage) []cell {
	if !f.valid() {
		return []cell{dim("-"), dim("-")}
	}
	lvl := lvlOK
	if f.low() {
		lvl = lvlWarn
	}
	return []cell{levelCell(fmtBytes(*f.Used), lvl), levelCell(fmtBytes(*f.Available), lvl)}
}

type nodeInfo struct {
	storage      *filesystemUsage
	node         *corev1.Node
	status       string // e.g. "Ready,SchedulingDisabled"
	statusLevel  level  // from conditions, cordon and taints
	versionLevel level  // kubelet version skew
	gpuLevel     level  // unhealthy / missing accelerators
	issues       []issue
	roles        string
	res          map[corev1.ResourceName]*amount
	accels       []accel
	gpuModel     string
	cloud        cloudInfo
}

func (ni *nodeInfo) get(name corev1.ResourceName) amount {
	if a := ni.res[name]; a != nil {
		return *a
	}
	return amount{}
}

// ---------------------------------------------------------------------------
// Flags and usage
// ---------------------------------------------------------------------------

const defaultSort = "nodegroup"

var sortKeys = []string{"nodegroup", "name", "cpu", "mem", "gpu", "pods", "age"}

func printUsage() {
	fmt.Println(styleTitle.Sprint("Kubernetes Get Nodes (kgn)"))
	fmt.Println(styleMeta.Sprintf("  version %s", version))
	fmt.Println()

	fmt.Println(styleSect.Sprint("USAGE"))
	fmt.Println("  kgn [node...] [flags]")
	fmt.Println()

	fmt.Println(styleSect.Sprint("FLAGS"))

	row := func(names, argument, description, def string) {
		nameCol := styleKey.Sprint(names)
		argCol := ""
		if argument != "" {
			argCol = styleMeta.Sprintf(" <%s>", argument)
		}
		defCol := ""
		if def != "" {
			defCol = styleMeta.Sprintf("  (default: %s)", def)
		}
		fmt.Printf("  %-38s %s%s\n", nameCol+argCol, description, defCol)
	}

	row("-l, --selector", "selector", "Label selector to filter nodes (e.g. role=gpu).", "")
	row("-d, --detail", "", "Show why a node is unhealthy plus conditions, taints, resources, events.", "")
	row("-u, --unhealthy", "", "Only show nodes with a health problem.", "")
	row("-o, --output", "format", "Output format: wide (split resource columns, cloud info).", "")
	row("--sort", "key", "Sort by "+strings.Join(sortKeys, ", ")+" (nodegroup: group then name; cpu/mem/gpu/pods: highest first).", defaultSort)
	row("--no-color", "", "Disable colored output (NO_COLOR is also honored).", "")
	row("--kubeconfig", "path", "Path to kubeconfig file.", "$KUBECONFIG or ~/.kube/config")
	row("--completions", "shell", "Print shell completion script (bash or zsh).", "")
	row("-v, --version", "", "Print version and exit.", "")
	row("-h, --help", "", "Show this help message.", "")
	fmt.Println()

	fmt.Println(styleSect.Sprint("COLUMNS"))
	fmt.Println("  CPU/MEM REQ   requested / allocatable (percent requested)")
	fmt.Println("  CPU/MEM USE   actual usage from metrics-server (hidden when unavailable)")
	fmt.Println("  GPU           requested / allocatable per accelerator type")
	fmt.Println("  EPHEMERAL USED/AVAIL (-o wide)   node filesystem bytes from kubelet; warns above 75% full")
	fmt.Println()

	fmt.Println(styleSect.Sprint("EXAMPLES"))
	fmt.Println("  kgn                       " + styleMeta.Sprint("# all nodes"))
	fmt.Println("  kgn -u -d                 " + styleMeta.Sprint("# unhealthy nodes and why"))
	fmt.Println("  kgn -o wide --sort cpu    " + styleMeta.Sprint("# busiest nodes first, with cloud info"))
	fmt.Println("  kgn <TAB>                 " + styleMeta.Sprint("# tab-complete node names"))
	fmt.Println()
}

// stringFlag registers a string flag under both its short and long names.
func stringFlag(fs *flag.FlagSet, short, long, def string) *string {
	p := fs.String(long, def, "")
	fs.StringVar(p, short, def, "")
	return p
}

func boolFlag(fs *flag.FlagSet, short, long string) *bool {
	p := fs.Bool(long, false, "")
	if short != "" {
		fs.BoolVar(p, short, false, "")
	}
	return p
}

// parseInterleaved parses flags that may appear before or after positional
// arguments (stdlib flag stops at the first positional).
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "__complete" {
		runComplete(os.Args[2:])
		return
	}

	fs := flag.NewFlagSet("kgn", flag.ContinueOnError)
	selector := stringFlag(fs, "l", "selector", "")
	output := stringFlag(fs, "o", "output", "")
	sortBy := fs.String("sort", defaultSort, "")
	detail := boolFlag(fs, "d", "detail")
	unhealthy := boolFlag(fs, "u", "unhealthy")
	noColor := boolFlag(fs, "", "no-color")
	kubeconfig := fs.String("kubeconfig", "", "")
	completionsFlag := fs.String("completions", "", "")
	versionFlag := boolFlag(fs, "v", "version")
	fs.Usage = printUsage

	names, err := parseInterleaved(fs, os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		os.Exit(2)
	}

	if *completionsFlag != "" {
		printCompletions(*completionsFlag)
		return
	}
	if *versionFlag {
		fmt.Println("kgn", version)
		return
	}
	if *output != "" && *output != "wide" {
		die("output", fmt.Errorf("unknown format %q: use wide", *output))
	}
	if !slices.Contains(sortKeys, *sortBy) {
		die("sort", fmt.Errorf("unknown key %q: use one of %s", *sortBy, strings.Join(sortKeys, ", ")))
	}

	color.Enable = term.IsTerminal(int(os.Stdout.Fd())) && !*noColor && os.Getenv("NO_COLOR") == ""

	clientset, err := newClient(*kubeconfig)
	if err != nil {
		die("loading kubeconfig", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	snap, err := fetch(ctx, clientset, *selector, names, *detail)
	if err != nil {
		die("fetching cluster state", err)
	}

	now := time.Now()
	infos := buildInfos(snap)
	total := len(infos)
	if *unhealthy {
		infos = slices.DeleteFunc(infos, func(ni *nodeInfo) bool { return len(ni.issues) == 0 })
	}
	if len(infos) == 0 {
		switch {
		case *unhealthy && total > 0:
			fmt.Println(styleOK.Sprintf("All %d nodes are healthy.", total))
		default:
			fmt.Fprintln(os.Stderr, "No nodes found.")
		}
		return
	}
	sortInfos(infos, *sortBy)

	opts := tableOpts{
		wide:       *output == "wide",
		hasMetrics: snap.usage != nil,
		showGPU:    slices.ContainsFunc(infos, func(ni *nodeInfo) bool { return len(ni.accels) > 0 }),
	}
	headers, rows := buildTable(infos, opts, now)
	var summaryRow []cell
	if len(infos) > 1 {
		summaryRow = buildSummaryRow(infos, opts)
	}
	printTable(os.Stdout, "", headers, rows, summaryRow)

	if *detail {
		for _, ni := range infos {
			fmt.Println()
			renderDetail(os.Stdout, ni, snap, now)
		}
	}
}

// newClient builds a clientset from the given kubeconfig path, or from the
// default loading rules ($KUBECONFIG, ~/.kube/config) when empty.
func newClient(kubeconfig string) (kubernetes.Interface, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// ---------------------------------------------------------------------------
// Fetching
// ---------------------------------------------------------------------------

type snapshot struct {
	storage       map[string]*filesystemUsage
	storageErrors map[string]error
	nodes         []corev1.Node
	pods          []corev1.Pod
	usage         map[string]corev1.ResourceList // nil when metrics-server is unavailable
	serverVersion string
	events        map[string][]corev1.Event // Warning events by node name (detail only)
	eventsErr     error
}

func fetch(ctx context.Context, cs kubernetes.Interface, selector string, names []string, withEvents bool) (*snapshot, error) {
	snap := &snapshot{}

	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	snap.nodes = nodes.Items
	if len(names) > 0 {
		snap.nodes, err = filterNodes(snap.nodes, names)
		if err != nil {
			return nil, err
		}
	}

	// Pods that still hold resources. With a single node, let the API server filter.
	fieldSel := "status.phase!=Succeeded,status.phase!=Failed"
	if len(snap.nodes) == 1 {
		fieldSel += ",spec.nodeName=" + snap.nodes[0].Name
	}
	pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: fieldSel})
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	snap.pods = pods.Items

	if sv, err := cs.Discovery().ServerVersion(); err == nil {
		snap.serverVersion = sv.GitVersion
	}

	snap.usage = fetchNodeMetrics(ctx, cs)
	snap.storage, snap.storageErrors = fetchNodeStorage(ctx, cs, snap.nodes)

	if withEvents {
		snap.events, snap.eventsErr = fetchNodeEvents(ctx, cs)
	}
	return snap, nil
}

func filterNodes(nodes []corev1.Node, names []string) ([]corev1.Node, error) {
	byName := make(map[string]corev1.Node, len(nodes))
	for _, n := range nodes {
		byName[n.Name] = n
	}
	var out []corev1.Node
	var missing []string
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		n, ok := byName[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		out = append(out, n)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("node(s) not found: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// fetchNodeMetrics reads node usage from metrics-server (metrics.k8s.io). Any
// failure (not installed, RBAC, not ready) returns nil so callers hide usage.
func fetchNodeMetrics(ctx context.Context, cs kubernetes.Interface) map[string]corev1.ResourceList {
	raw, err := cs.CoreV1().RESTClient().Get().AbsPath("/apis/metrics.k8s.io/v1beta1/nodes").DoRaw(ctx)
	if err != nil {
		return nil
	}
	return parseNodeMetrics(raw)
}

func parseNodeMetrics(raw []byte) map[string]corev1.ResourceList {
	var list struct {
		Items []struct {
			Metadata metav1.ObjectMeta   `json:"metadata"`
			Usage    corev1.ResourceList `json:"usage"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	out := make(map[string]corev1.ResourceList, len(list.Items))
	for _, it := range list.Items {
		out[it.Metadata.Name] = it.Usage
	}
	return out
}

// fetchNodeStorage uses bounded parallel requests to the kubelet Summary API.
// Missing permissions (nodes/proxy), unsupported stats and timeouts are optional failures.
func fetchNodeStorage(ctx context.Context, cs kubernetes.Interface, nodes []corev1.Node) (map[string]*filesystemUsage, map[string]error) {
	out := map[string]*filesystemUsage{}
	errs := map[string]error{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for _, n := range nodes {
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				mu.Lock()
				errs[n.Name] = ctx.Err()
				mu.Unlock()
				return
			}
			requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			raw, err := cs.CoreV1().RESTClient().Get().Resource("nodes").Name(n.Name).SubResource("proxy").Suffix("stats", "summary").DoRaw(requestCtx)
			var fs *filesystemUsage
			if err == nil {
				fs, err = parseNodeStorage(raw)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[n.Name] = err
			} else {
				out[n.Name] = fs
			}
		})
	}
	wg.Wait()
	return out, errs
}

func parseNodeStorage(raw []byte) (*filesystemUsage, error) {
	var summary struct {
		Node struct {
			FS *filesystemUsage `json:"fs"`
		} `json:"node"`
	}
	if err := json.Unmarshal(raw, &summary); err != nil {
		return nil, err
	}
	if !summary.Node.FS.valid() {
		return nil, errors.New("node filesystem statistics missing or invalid")
	}
	return summary.Node.FS, nil
}

func fetchNodeEvents(ctx context.Context, cs kubernetes.Interface) (map[string][]corev1.Event, error) {
	list, err := cs.CoreV1().Events("").List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.kind=Node,type=Warning",
	})
	if err != nil {
		return nil, err
	}
	out := map[string][]corev1.Event{}
	for _, e := range list.Items {
		out[e.InvolvedObject.Name] = append(out[e.InvolvedObject.Name], e)
	}
	for name := range out {
		slices.SortFunc(out[name], func(a, b corev1.Event) int {
			return eventTime(b).Compare(eventTime(a)) // newest first
		})
	}
	return out, nil
}

func eventTime(e corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	default:
		return e.CreationTimestamp.Time
	}
}

// ---------------------------------------------------------------------------
// Building node info
// ---------------------------------------------------------------------------

func buildInfos(snap *snapshot) []*nodeInfo {
	podsByNode := map[string][]*corev1.Pod{}
	for i := range snap.pods {
		p := &snap.pods[i]
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		podsByNode[p.Spec.NodeName] = append(podsByNode[p.Spec.NodeName], p)
	}

	ref := snap.serverVersion
	if ref == "" {
		ref = newestKubelet(snap.nodes)
	}

	infos := make([]*nodeInfo, 0, len(snap.nodes))
	for i := range snap.nodes {
		n := &snap.nodes[i]
		var usage corev1.ResourceList
		if snap.usage != nil {
			usage = snap.usage[n.Name]
		}
		ni := buildNodeInfo(n, podsByNode[n.Name], usage, ref)
		ni.storage = snap.storage[n.Name]
		if ni.storage.low() {
			ni.issues = append(ni.issues, issue{lvlWarn, fmt.Sprintf("ephemeral-storage-low: node filesystem %.2f%% full (above %d%%); used %s, available %s", 100*float64(ni.storage.Capacity-*ni.storage.Available)/float64(ni.storage.Capacity), ephemeralWarnPct, fmtBytes(*ni.storage.Used), fmtBytes(*ni.storage.Available)), "ephemeral-storage-low"})
		}
		infos = append(infos, ni)
	}
	return infos
}

func buildNodeInfo(n *corev1.Node, pods []*corev1.Pod, usage corev1.ResourceList, refVersion string) *nodeInfo {
	ni := &nodeInfo{
		node:  n,
		roles: nodeRoles(n.Labels),
		res:   map[corev1.ResourceName]*amount{},
		cloud: cloudMetadata(n.Labels),
	}
	get := func(name corev1.ResourceName) *amount {
		a := ni.res[name]
		if a == nil {
			a = &amount{}
			ni.res[name] = a
		}
		return a
	}

	for name, q := range n.Status.Capacity {
		get(name).capacity = qty(name, q)
	}
	for name, q := range n.Status.Allocatable {
		get(name).alloc = qty(name, q)
	}
	for _, p := range pods {
		for name, v := range podResources(p, func(r corev1.ResourceRequirements) corev1.ResourceList { return r.Requests }) {
			get(name).req += v
		}
		for name, v := range podResources(p, func(r corev1.ResourceRequirements) corev1.ResourceList { return r.Limits }) {
			get(name).lim += v
		}
	}
	get(corev1.ResourcePods).req = int64(len(pods))
	for name, q := range usage {
		a := get(name)
		a.used = qty(name, q)
		a.hasUsed = true
	}

	var issues []issue
	ni.status, issues = nodeHealth(n)
	ni.statusLevel = worstLevel(issues)

	for name, a := range ni.res {
		if isAccelerator(name) && (a.capacity > 0 || a.alloc > 0 || a.req > 0) {
			ni.accels = append(ni.accels, accel{name: name, amount: *a})
		}
	}
	slices.SortFunc(ni.accels, func(a, b accel) int { return cmp.Compare(a.name, b.name) })
	ni.gpuModel = gpuModel(n)
	gpuIssues := acceleratorHealth(n.Labels, ni.accels)
	ni.gpuLevel = worstLevel(gpuIssues)
	issues = append(issues, gpuIssues...)

	if lvl, msg := kubeletSkew(n.Status.NodeInfo.KubeletVersion, refVersion); lvl != lvlOK {
		ni.versionLevel = lvl
		issues = append(issues, issue{lvl, msg, "KubeletSkew"})
	}

	ni.issues = issues
	return ni
}

// qty converts a quantity to int64 in the unit used throughout (millicores
// for CPU, base units otherwise).
func qty(name corev1.ResourceName, q resource.Quantity) int64 {
	if name == corev1.ResourceCPU {
		return q.MilliValue()
	}
	return q.Value()
}

// podResources returns a pod's effective requests or limits, following the
// scheduler's rules (as in `kubectl describe node`): app containers and
// restartable (sidecar) init containers add up, a regular init container
// only needs the sidecars started before it, the larger of the two wins, and
// pod-level resources and overhead are applied on top.
func podResources(p *corev1.Pod, pick func(corev1.ResourceRequirements) corev1.ResourceList) map[corev1.ResourceName]int64 {
	total := map[corev1.ResourceName]int64{}
	add := func(dst map[corev1.ResourceName]int64, rl corev1.ResourceList) {
		for name, q := range rl {
			dst[name] += qty(name, q)
		}
	}
	maxInto := func(dst, src map[corev1.ResourceName]int64) {
		for name, v := range src {
			if v > dst[name] {
				dst[name] = v
			}
		}
	}

	for _, c := range p.Spec.Containers {
		add(total, pick(c.Resources))
	}
	sidecars := map[corev1.ResourceName]int64{}
	initPeak := map[corev1.ResourceName]int64{}
	for _, c := range p.Spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			add(total, pick(c.Resources))
			add(sidecars, pick(c.Resources))
			maxInto(initPeak, sidecars)
			continue
		}
		cur := map[corev1.ResourceName]int64{}
		maxInto(cur, sidecars)
		add(cur, pick(c.Resources))
		maxInto(initPeak, cur)
	}
	maxInto(total, initPeak)

	if p.Spec.Resources != nil {
		for name, q := range pick(*p.Spec.Resources) {
			total[name] = qty(name, q)
		}
	}
	add(total, p.Spec.Overhead)
	return total
}

func worstLevel(issues []issue) level {
	l := lvlOK
	for _, is := range issues {
		l = max(l, is.level)
	}
	return l
}

func findCondition(n *corev1.Node, t corev1.NodeConditionType) *corev1.NodeCondition {
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type == t {
			return &n.Status.Conditions[i]
		}
	}
	return nil
}

// conditionHealthy treats readiness conditions (Ready, KernelReady, etc.)
// as healthy when True; pressure and problem conditions are healthy when False.
func conditionHealthy(c corev1.NodeCondition) bool {
	if strings.HasSuffix(string(c.Type), "Ready") {
		return c.Status == corev1.ConditionTrue
	}
	return c.Status == corev1.ConditionFalse
}

func describeCondition(prefix string, c *corev1.NodeCondition) string {
	var b strings.Builder
	b.WriteString(prefix)
	if c.Reason != "" {
		b.WriteString(": " + c.Reason)
	}
	if msg := strings.TrimSpace(c.Message); msg != "" {
		b.WriteString(" - " + msg)
	}
	if !c.LastTransitionTime.IsZero() {
		b.WriteString(" (since " + duration.HumanDuration(time.Since(c.LastTransitionTime.Time)) + ")")
	}
	return b.String()
}

// nodeHealth returns the STATUS text (kubectl style, plus active pressure
// conditions) and the issues found in conditions, cordon state and taints.
func nodeHealth(n *corev1.Node) (string, []issue) {
	var issues []issue
	ready := "Unknown"
	switch c := findCondition(n, corev1.NodeReady); {
	case c == nil:
		issues = append(issues, issue{lvlCrit, "Unknown: kubelet has never reported a Ready condition", "Unknown"})
	case c.Status == corev1.ConditionTrue:
		ready = "Ready"
	case c.Status == corev1.ConditionFalse:
		ready = "NotReady"
		issues = append(issues, issue{lvlCrit, describeCondition("NotReady", c), "NotReady"})
	default:
		issues = append(issues, issue{lvlCrit, describeCondition("Unknown: kubelet stopped posting node status", c), "Unknown"})
	}
	parts := []string{ready}

	if n.Spec.Unschedulable {
		parts = append(parts, "SchedulingDisabled")
		issues = append(issues, issue{lvlWarn, "cordoned: new pods will not be scheduled (kubectl uncordon to undo)", "SchedulingDisabled"})
	}
	if n.DeletionTimestamp != nil {
		parts = append(parts, "Terminating")
		issues = append(issues, issue{lvlWarn, "node object is being deleted", "Terminating"})
	}

	// Ready-suffixed conditions are positive checks. Preserve the existing
	// behavior of ignoring Unknown pressure/problem conditions when a kubelet
	// stops reporting, but flag False or Unknown readiness checks.
	for i := range n.Status.Conditions {
		c := &n.Status.Conditions[i]
		readiness := strings.HasSuffix(string(c.Type), "Ready")
		if c.Type == corev1.NodeReady || conditionHealthy(*c) || (!readiness && c.Status != corev1.ConditionTrue) {
			continue
		}
		label := string(c.Type)
		if readiness {
			label += "=" + string(c.Status)
		}
		parts = append(parts, label)
		lvl := lvlWarn
		if c.Type == corev1.NodeNetworkUnavailable {
			lvl = lvlCrit // pod networking is not configured; pods can't run
		}
		issues = append(issues, issue{lvl, describeCondition(label, c), label})
	}

	for _, t := range n.Spec.Taints {
		switch t.Key {
		case "node.kubernetes.io/unreachable":
			issues = append(issues, issue{lvlCrit, "taint " + t.Key + ":" + string(t.Effect) + ": node controller cannot reach the kubelet", t.Key})
		case "node.kubernetes.io/not-ready":
			issues = append(issues, issue{lvlCrit, "taint " + t.Key + ":" + string(t.Effect) + ": node controller marked the node not ready", t.Key})
		case "node.kubernetes.io/out-of-service":
			issues = append(issues, issue{lvlCrit, "taint " + t.Key + ": node marked out of service, pods are being force-deleted", t.Key})
		case "ToBeDeletedByClusterAutoscaler":
			issues = append(issues, issue{lvlWarn, "scheduled for removal by cluster-autoscaler", t.Key})
		case "karpenter.sh/disrupted", "karpenter.sh/disruption":
			issues = append(issues, issue{lvlWarn, "being disrupted (drained/replaced) by Karpenter", t.Key})
		}
	}
	return strings.Join(parts, ","), issues
}

// kubeletSkew compares a kubelet version with the API server (or newest
// kubelet) version. Kubelets may lag up to three minor versions; newer is
// never supported.
func kubeletSkew(kubelet, ref string) (level, string) {
	kv, err1 := utilversion.ParseGeneric(kubelet)
	rv, err2 := utilversion.ParseGeneric(ref)
	if err1 != nil || err2 != nil {
		return lvlOK, ""
	}
	if kv.Major() != rv.Major() {
		return lvlCrit, fmt.Sprintf("kubelet %s and control plane %s differ in major version", kubelet, ref)
	}
	d := int(rv.Minor()) - int(kv.Minor())
	switch {
	case d < 0:
		return lvlCrit, fmt.Sprintf("kubelet %s is newer than the control plane %s (unsupported)", kubelet, ref)
	case d == 0:
		return lvlOK, ""
	case d <= maxKubeletSkew:
		return lvlWarn, fmt.Sprintf("kubelet %s is %d minor version(s) behind the control plane %s", kubelet, d, ref)
	default:
		return lvlCrit, fmt.Sprintf("kubelet %s is %d minor versions behind the control plane %s (unsupported, max %d)", kubelet, d, ref, maxKubeletSkew)
	}
}

func newestKubelet(nodes []corev1.Node) string {
	var best *utilversion.Version
	bestStr := ""
	for _, n := range nodes {
		v, err := utilversion.ParseGeneric(n.Status.NodeInfo.KubeletVersion)
		if err != nil {
			continue
		}
		if best == nil || best.LessThan(v) {
			best, bestStr = v, n.Status.NodeInfo.KubeletVersion
		}
	}
	return bestStr
}

func nodeRoles(labels map[string]string) string {
	var roles []string
	for k, v := range labels {
		switch {
		case strings.HasPrefix(k, "node-role.kubernetes.io/"):
			if r := strings.TrimPrefix(k, "node-role.kubernetes.io/"); r != "" {
				roles = append(roles, r)
			}
		case k == "kubernetes.io/role" && v != "":
			roles = append(roles, v)
		}
	}
	if len(roles) == 0 {
		return "<none>"
	}
	slices.Sort(roles)
	return strings.Join(slices.Compact(roles), ",")
}

// ---------------------------------------------------------------------------
// Accelerators
// ---------------------------------------------------------------------------

var acceleratorPrefixes = []string{
	"nvidia.com/",           // gpu, mig-*, gpu.shared (time-slicing)
	"amd.com/",              // gpu
	"gpu.intel.com/",        // i915, xe
	"aws.amazon.com/neuron", // neuron, neuroncore, neurondevice
	"google.com/tpu",
	"habana.ai/", // gaudi
}

func isAccelerator(name corev1.ResourceName) bool {
	for _, p := range acceleratorPrefixes {
		if strings.HasPrefix(string(name), p) {
			return true
		}
	}
	return false
}

// acceleratorLabel is the short name shown in the GPU column.
func acceleratorLabel(name corev1.ResourceName) string {
	s := string(name)
	switch s {
	case "nvidia.com/gpu":
		return "nvidia"
	case "amd.com/gpu":
		return "amd"
	case "google.com/tpu":
		return "tpu"
	}
	if rest, ok := strings.CutPrefix(s, "gpu.intel.com/"); ok {
		return "intel-" + rest
	}
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func gpuModel(n *corev1.Node) string {
	if model := firstLabel(n.Labels,
		"nvidia.com/gpu.product",
		"cloud.google.com/gke-accelerator",
		"cloud.google.com/gke-tpu-accelerator",
		"karpenter.k8s.aws/instance-gpu-name",
		"amd.com/gpu.product-name",
		"beta.amd.com/gpu.product-name",
	); model != "" {
		return model
	}
	// A generic instance-type label alone does not prove this is an EC2 node.
	if !strings.HasPrefix(n.Spec.ProviderID, "aws://") {
		return ""
	}
	instanceType := firstLabel(n.Labels, "node.kubernetes.io/instance-type", "beta.kubernetes.io/instance-type")
	return ec2GPUModel(instanceType)
}

// ec2GPUModel infers the physical model, not the schedulable GPU count or
// partition size. G6f/Gr6f expose fractional L4 devices as integer resources.
// Verified against AWS's specifications in October 2026:
// https://docs.aws.amazon.com/ec2/latest/instancetypes/ac.html
func ec2GPUModel(instanceType string) string {
	family, size, ok := strings.Cut(instanceType, ".")
	if !ok || size == "" {
		return ""
	}
	switch family {
	case "g4ad":
		return "AMD-Radeon-Pro-V520"
	case "g4dn":
		return "NVIDIA-T4"
	case "g5":
		return "NVIDIA-A10G"
	case "g5g":
		return "NVIDIA-T4g"
	case "g6", "g6f", "gr6", "gr6f":
		return "NVIDIA-L4"
	case "g6e":
		return "NVIDIA-L40S"
	case "g7":
		return "NVIDIA-RTX-PRO-4500"
	case "g7e":
		return "NVIDIA-RTX-PRO-Server-6000"
	case "p4d", "p4de":
		return "NVIDIA-A100"
	case "p5":
		return "NVIDIA-H100"
	case "p5e", "p5en":
		return "NVIDIA-H200"
	case "p6-b200", "p6e-gb200":
		return "NVIDIA-B200"
	case "p6-b300":
		return "NVIDIA-B300"
	default:
		return ""
	}
}

// acceleratorHealth flags devices the device plugin reports as unhealthy
// (allocatable below capacity) and GPU nodes whose device plugin advertises
// nothing at all.
func acceleratorHealth(labels map[string]string, accels []accel) []issue {
	var issues []issue
	for _, a := range accels {
		if a.capacity > a.alloc {
			issues = append(issues, issue{lvlWarn, fmt.Sprintf("%d of %d %s not allocatable (device plugin reports them unhealthy)", a.capacity-a.alloc, a.capacity, a.name), string(a.name) + "Unhealthy"})
		}
	}
	if n, err := strconv.Atoi(labels["nvidia.com/gpu.count"]); err == nil && n > 0 {
		advertised := slices.ContainsFunc(accels, func(a accel) bool {
			return strings.HasPrefix(string(a.name), "nvidia.com/") && a.capacity > 0
		})
		if !advertised {
			issues = append(issues, issue{lvlWarn, fmt.Sprintf("node is labelled with %d NVIDIA GPU(s) but none are advertised (is the device plugin running?)", n), "GPUsMissing"})
		}
	}
	return issues
}

// ---------------------------------------------------------------------------
// Cloud metadata
// ---------------------------------------------------------------------------

func firstLabel(labels map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := labels[k]; v != "" {
			return v
		}
	}
	return ""
}

func cloudMetadata(labels map[string]string) cloudInfo {
	return cloudInfo{
		instanceType: firstLabel(labels, "node.kubernetes.io/instance-type", "beta.kubernetes.io/instance-type"),
		zone:         firstLabel(labels, "topology.kubernetes.io/zone", "failure-domain.beta.kubernetes.io/zone"),
		capacityType: capacityType(labels),
		pool:         nodeGroup(labels),
	}
}

// nodeGroup prefers provider names, then provider IDs, then custom labels.
// Kubernetes has no standard node-group label. Generic keys are conventions.
func nodeGroup(labels map[string]string) string {
	return firstLabel(labels,
		"karpenter.sh/nodepool",
		"eks.amazonaws.com/nodegroup",
		"alpha.eksctl.io/nodegroup-name",
		"cloud.google.com/gke-nodepool",
		"kubernetes.azure.com/agentpool",
		"agentpool",
		"doks.digitalocean.com/node-pool",
		"ibm-cloud.kubernetes.io/worker-pool-name",
		"k8s.scaleway.com/pool-name",
		"doks.digitalocean.com/node-pool-id",
		"k8s.scaleway.com/pool",
		"node.kubernetes.io/pool",
		"kubernetes.io/nodegroup",
		"nodegroup",
		"nodepool", // OVHcloud also uses this unprefixed label.
	)
}

// FNV keeps group colors stable across runs, filters, and sorting changes.
// Cool hues distinguish groups without reusing warning yellow/critical red.
var nodeGroupPalette = []uint8{
	39, 81, 45, 75, 111, 141, 177, 213, 205,
	99, 117, 147, 183, 219, 51, 105, 69, 171,
}

var nodeGroupBasicPalette = []color.Style{
	{color.FgLightBlue}, {color.FgLightCyan}, {color.FgLightMagenta},
	{color.FgBlue}, {color.FgCyan}, {color.FgMagenta},
	{color.Bold, color.FgLightBlue}, {color.Bold, color.FgLightMagenta},
}

func nodeGroupCell(group string) cell {
	if group == "" {
		return dim("-")
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(group))
	if color.Support256Color() {
		index := h.Sum32() % uint32(len(nodeGroupPalette))
		return cell{text: group, style: color.C256(nodeGroupPalette[index])}
	}
	index := h.Sum32() % uint32(len(nodeGroupBasicPalette))
	return cell{text: group, style: nodeGroupBasicPalette[index]}
}

func capacityType(labels map[string]string) string {
	normalize := func(v string) string {
		v = strings.ToLower(strings.ReplaceAll(v, "_", "-"))
		if v == "normal" || v == "regular" {
			return "on-demand"
		}
		return v
	}
	if v := firstLabel(labels, "karpenter.sh/capacity-type", "eks.amazonaws.com/capacityType", "node.kubernetes.io/lifecycle"); v != "" {
		return normalize(v)
	}
	switch {
	case labels["cloud.google.com/gke-spot"] == "true":
		return "spot"
	case labels["cloud.google.com/gke-preemptible"] == "true":
		return "preemptible"
	case labels["kubernetes.azure.com/scalesetpriority"] != "":
		return normalize(labels["kubernetes.azure.com/scalesetpriority"])
	}
	return ""
}

// ---------------------------------------------------------------------------
// Formatting
// ---------------------------------------------------------------------------

func trimZeros(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// fmtCPU renders millicores as cores: "8", "7.9", "0.25".
func fmtCPU(milli int64) string {
	switch {
	case milli%1000 == 0:
		return strconv.FormatInt(milli/1000, 10)
	case milli < 1000:
		return trimZeros(fmt.Sprintf("%.2f", float64(milli)/1000))
	default:
		return trimZeros(fmt.Sprintf("%.1f", float64(milli)/1000))
	}
}

// fmtBytes uses binary sizes with compact suffixes: "30.1G", "512M".
func fmtBytes(b int64) string {
	units := []struct {
		suffix string
		size   float64
		prec   int
	}{
		{"T", 1 << 40, 1},
		{"G", 1 << 30, 1},
		{"M", 1 << 20, 0},
		{"K", 1 << 10, 0},
	}
	for _, u := range units {
		if float64(b) >= u.size {
			return trimZeros(strconv.FormatFloat(float64(b)/u.size, 'f', u.prec, 64)) + u.suffix
		}
	}
	return strconv.FormatInt(b, 10)
}

func fmtAmount(name corev1.ResourceName, v int64) string {
	switch {
	case name == corev1.ResourceCPU:
		return fmtCPU(v)
	case name == corev1.ResourceMemory, name == corev1.ResourceEphemeralStorage,
		strings.HasPrefix(string(name), corev1.ResourceHugePagesPrefix):
		return fmtBytes(v)
	default:
		return strconv.FormatInt(v, 10)
	}
}

func pct(part, whole int64) (int, bool) {
	if whole <= 0 {
		return 0, false
	}
	return int(math.Round(float64(part) * 100 / float64(whole))), true
}

func pctLevel(p int) level {
	switch {
	case p > critPct:
		return lvlCrit
	case p > warnPct:
		return lvlWarn
	default:
		return lvlOK
	}
}

func podsLevel(p int) level {
	switch {
	case p >= podsCritPct:
		return lvlCrit
	case p >= podsWarnPct:
		return lvlWarn
	default:
		return lvlOK
	}
}

// valueWithPct renders "41%: 3.2" or just "3.2" when the percentage is unknown.
func valueWithPct(name corev1.ResourceName, v, whole int64) (string, int, bool) {
	p, ok := pct(v, whole)
	if !ok {
		return fmtAmount(name, v), 0, false
	}
	return fmt.Sprintf("%d%%: %s", p, fmtAmount(name, v)), p, true
}

// ---------------------------------------------------------------------------
// Table
// ---------------------------------------------------------------------------

type terminalStyle interface {
	Sprint(...any) string
	String() string
}

type cell struct {
	text      string
	style     terminalStyle
	parts     []cell // independently styled segments; text stays plain for alignment
	separator string
}

func plain(s string) cell { return cell{text: s, style: stylePlain} }
func dim(s string) cell   { return cell{text: s, style: styleMeta} }

// levelCell colors healthy values white and problems yellow/red.
func levelCell(s string, l level) cell {
	if l == lvlOK {
		return plain(s)
	}
	return cell{text: s, style: l.style()}
}

// percentageStyle uses the displayed whole-number percentage, so rounding
// never produces a value whose color contradicts its visible band.
func percentageStyle(p int) terminalStyle {
	band := 0
	switch {
	case p > 95:
		band = 4
	case p > 80:
		band = 3
	case p > 60:
		band = 2
	case p >= 40:
		band = 1
	}
	if color.Support256Color() {
		palette := [...]uint8{75, 45, 84, 220, 203}
		return color.C256(palette[band])
	}
	palette := [...]color.Style{
		{color.FgLightBlue}, {color.FgLightCyan}, {color.FgLightGreen}, styleWarn, styleBad,
	}
	return palette[band]
}

// percentageCell colors only the percentage; quantities remain neutral.
func percentageCell(s string, p int, known bool) cell {
	if !known {
		return plain(s)
	}
	st := percentageStyle(p)
	percentage, values, hasValues := strings.Cut(s, ": ")
	if !hasValues {
		return cell{text: s, style: st}
	}
	return cell{text: s, style: stylePlain, parts: []cell{
		{text: percentage, style: st}, plain(": " + values),
	}}
}

// statusCell shows a single healthy label or only the failing health checks.
func statusCell(ni *nodeInfo) cell {
	var labels []string
	var parts []cell
	for _, is := range ni.issues {
		if is.level == lvlOK {
			continue
		}
		label := is.label
		if label == "" {
			label = is.text
		}
		labels = append(labels, label)
		parts = append(parts, cell{text: label, style: is.level.style()})
	}
	if len(parts) == 0 {
		return cell{text: "Healthy", style: styleOK}
	}
	return cell{text: strings.Join(labels, ","), style: stylePlain, parts: parts, separator: ","}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

type tableOpts struct {
	wide, hasMetrics, showGPU bool
}

// tableHeaders describes the source columns before wide pairs are compacted.
func tableHeaders(o tableOpts) []string {
	headers := []string{"NAME"}
	for _, label := range []string{"CPU", "MEM"} {
		if o.wide {
			headers = append(headers, label+" REQ", label+" LIM", label+" ALLOC", label+" AVAIL")
		} else {
			headers = append(headers, label+" REQ")
		}
		if o.hasMetrics {
			headers = append(headers, label+" USE")
		}
	}
	if o.showGPU {
		headers = append(headers, "GPU TYPE", "GPU REQ", "GPU ALLOC", "GPU AVAIL", "GPU MODEL")
	}
	if o.wide {
		headers = append(headers, "EPHEMERAL USED", "EPHEMERAL AVAIL")
	}
	headers = append(headers, "PODS")
	if o.wide {
		headers = append(headers, "INSTANCE", "CAPACITY", "ZONE", "INTERNAL-IP", "AGE", "VERSION", "STATUS", "ROLES")
	}
	headers = append(headers, "NODE GROUP")

	return headers
}

// pairCell flattens styled segments so each percentage keeps its own color.
func pairCell(left, right cell) cell {
	parts := []cell{}
	for i, c := range []cell{left, right} {
		if i > 0 {
			parts = append(parts, plain("/"))
		}
		if len(c.parts) == 0 {
			parts = append(parts, c)
			continue
		}
		for j, part := range c.parts {
			if j > 0 && c.separator != "" {
				parts = append(parts, plain(c.separator))
			}
			parts = append(parts, part)
		}
	}
	return cell{text: left.text + "/" + right.text, style: stylePlain, parts: parts}
}

func compactWideTable(headers []string, rows [][]cell) ([]string, [][]cell) {
	pairs := map[string]string{
		"CPU REQ": "CPU LIM", "CPU ALLOC": "CPU AVAIL",
		"MEM REQ": "MEM LIM", "MEM ALLOC": "MEM AVAIL",
		"GPU ALLOC": "GPU AVAIL", "EPHEMERAL USED": "EPHEMERAL AVAIL",
	}
	var compactHeaders []string
	compactRows := make([][]cell, len(rows))
	for i := 0; i < len(headers); i++ {
		second, paired := pairs[headers[i]]
		paired = paired && i+1 < len(headers) && headers[i+1] == second
		header := headers[i]
		if paired {
			header += "/" + strings.TrimPrefix(second, strings.Split(header, " ")[0]+" ")
		}
		compactHeaders = append(compactHeaders, header)
		for j, row := range rows {
			c := row[i]
			if paired {
				c = pairCell(c, row[i+1])
			}
			compactRows[j] = append(compactRows[j], c)
		}
		if paired {
			i++
		}
	}
	return compactHeaders, compactRows
}

func buildTable(infos []*nodeInfo, o tableOpts, now time.Time) ([]string, [][]cell) {
	headers := tableHeaders(o)
	rows := make([][]cell, 0, len(infos))
	for _, ni := range infos {
		nameCell := plain(ni.node.Name)
		if ni.cloud.pool != "" {
			nameCell.style = nodeGroupCell(ni.cloud.pool).style
		}
		row := []cell{nameCell}
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			a := ni.get(name)
			if o.wide {
				req, p, known := valueWithPct(name, a.req, a.alloc)
				lim, lp, limKnown := valueWithPct(name, a.lim, a.alloc)
				row = append(row,
					percentageCell(req, p, known),
					percentageCell(lim, lp, limKnown),
					plain(fmtAmount(name, a.alloc)),
					plain(fmtAmount(name, a.avail())),
				)
			} else {
				p, known := pct(a.req, a.alloc)
				row = append(row, percentageCell(fmt.Sprintf("%d%%: %s/%s", p, fmtAmount(name, a.req), fmtAmount(name, a.alloc)), p, known))
			}
			if o.hasMetrics {
				row = append(row, usageCell(name, a, o.wide))
			}
		}
		if o.showGPU {
			row = append(row, gpuCells(ni)...)
		}
		if o.wide {
			row = append(row, storageCells(ni.storage)...)
		}
		pods := ni.get(corev1.ResourcePods)
		pp, _ := pct(pods.req, pods.alloc)
		row = append(row, levelCell(fmt.Sprintf("%d/%d", pods.req, pods.alloc), podsLevel(pp)))
		if o.wide {
			row = append(row,
				plain(orDash(ni.cloud.instanceType)),
				plain(orDash(ni.cloud.capacityType)),
				plain(orDash(ni.cloud.zone)),
				plain(orDash(nodeAddress(ni.node, corev1.NodeInternalIP))),
				plain(duration.HumanDuration(now.Sub(ni.node.CreationTimestamp.Time))),
				levelCell(ni.node.Status.NodeInfo.KubeletVersion, ni.versionLevel),
				statusCell(ni),
				dim(ni.roles),
			)
		}
		row = append(row, nodeGroupCell(ni.cloud.pool))
		rows = append(rows, row)
	}
	if o.wide {
		return compactWideTable(headers, rows)
	}
	return headers, rows
}

func usageCell(name corev1.ResourceName, a amount, wide bool) cell {
	if !a.hasUsed {
		return dim("-")
	}
	if wide {
		s, p, known := valueWithPct(name, a.used, a.alloc)
		return percentageCell(s, p, known)
	}
	p, ok := pct(a.used, a.alloc)
	if !ok {
		return plain(fmtAmount(name, a.used))
	}
	return percentageCell(fmt.Sprintf("%d%%", p), p, true)
}

// gpuCells renders accelerators. GPUs are expected to be fully used on
// dedicated nodes, so they are colored by device health, not by percentage.
func gpuCells(ni *nodeInfo) []cell {
	if len(ni.accels) == 0 {
		return []cell{dim("-"), dim("-"), dim("-"), dim("-"), dim("-")}
	}
	st := styleAccent
	if ni.gpuLevel != lvlOK {
		st = ni.gpuLevel.style()
	}
	model := dim(orDash(ni.gpuModel))
	if ni.gpuModel != "" {
		model.style = st
	}
	var types, reqs, allocs, avails []string
	for _, a := range ni.accels {
		label := acceleratorLabel(a.name)
		types = append(types, label)
		reqs = append(reqs, strconv.FormatInt(a.req, 10))
		allocs = append(allocs, strconv.FormatInt(a.alloc, 10))
		avails = append(avails, strconv.FormatInt(a.avail(), 10))
	}
	return []cell{
		{text: strings.Join(types, ","), style: st},
		plain(strings.Join(reqs, ",")),
		plain(strings.Join(allocs, ",")),
		plain(strings.Join(avails, ",")),
		model,
	}
}

func nodeAddress(n *corev1.Node, t corev1.NodeAddressType) string {
	for _, a := range n.Status.Addresses {
		if a.Type == t {
			return a.Address
		}
	}
	return ""
}

// tableColumnDividers returns column indices after which the main node table
// draws white vertical rules (CPU|memory, then memory|pods/metadata).
func tableColumnDividers(headers []string) []int {
	var out []int
	for i, h := range headers {
		if strings.HasPrefix(h, "MEM") && i > 0 {
			out = append(out, i-1)
			break
		}
	}
	if i := slices.Index(headers, "PODS"); i > 0 {
		out = append(out, i-1)
	}
	return out
}

func gapAfterColumn(i int, dividers []int, gap int) int {
	if i >= 0 && slices.Contains(dividers, i) {
		return 3 // " │ "
	}
	return gap
}

// printTable pads plain text first, then colors it, so ANSI codes never
// affect alignment.
func printTable(w io.Writer, indent string, headers []string, rows [][]cell, summary ...[]cell) {
	const gap = 2
	dividers := tableColumnDividers(headers)
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if i < len(widths) {
				widths[i] = max(widths[i], utf8.RuneCountInString(c.text))
			}
		}
	}
	var sumRow []cell
	if len(summary) > 0 && len(summary[0]) > 0 {
		sumRow = summary[0]
		for i, c := range sumRow {
			if i < len(widths) {
				widths[i] = max(widths[i], utf8.RuneCountInString(c.text))
			}
		}
	}
	gapWidth := func(i int) int {
		if i >= len(widths)-1 {
			return 0
		}
		return gapAfterColumn(i, dividers, gap)
	}
	pad := func(s string, i int) string {
		if i >= len(widths)-1 {
			return s
		}
		n := widths[i] - utf8.RuneCountInString(s)
		if n < 0 {
			n = 0
		}
		return s + strings.Repeat(" ", n)
	}
	writeGap := func(b *strings.Builder, i int) {
		if i >= len(widths)-1 {
			return
		}
		if slices.Contains(dividers, i) {
			b.WriteByte(' ')
			b.WriteString(stylePlain.Sprint("│"))
			b.WriteByte(' ')
			return
		}
		b.WriteString(strings.Repeat(" ", gap))
	}
	writeCells := func(b *strings.Builder, cells []cell) {
		for i, c := range cells {
			if i >= len(widths) {
				break
			}
			padded := pad(c.text, i)
			if len(c.parts) == 0 {
				b.WriteString(c.style.Sprint(padded))
			} else {
				for j, part := range c.parts {
					if j > 0 {
						b.WriteString(c.separator)
					}
					b.WriteString(part.style.Sprint(part.text))
				}
				b.WriteString(strings.TrimPrefix(padded, c.text))
			}
			writeGap(b, i)
		}
	}

	var hb strings.Builder
	headerCells := make([]cell, len(headers))
	for i, h := range headers {
		headerCells[i] = cell{text: h, style: styleTitle}
	}
	writeCells(&hb, headerCells)
	_, _ = fmt.Fprintln(w, indent+hb.String())
	for _, r := range rows {
		var lb strings.Builder
		writeCells(&lb, r)
		_, _ = fmt.Fprintln(w, indent+lb.String())
	}
	if len(sumRow) > 0 {
		totalWidth := 0
		for i, width := range widths {
			totalWidth += width + gapWidth(i)
		}
		if totalWidth > 0 {
			_, _ = fmt.Fprintln(w, indent+summaryRule(widths, dividers, gap, totalWidth))
		}
		var lb strings.Builder
		writeCells(&lb, sumRow)
		_, _ = fmt.Fprintln(w, indent+lb.String())
	}
}

// summaryRule is the horizontal line above Summary. Vertical rules cross it as
// white ┼ so the column dividers stay aligned.
func summaryRule(widths []int, dividers []int, gap, totalWidth int) string {
	var crosses []int
	for _, div := range dividers {
		if div < 0 || div >= len(widths)-1 {
			continue
		}
		off := 0
		for i := 0; i <= div; i++ {
			off += widths[i]
			if i < div {
				off += gapAfterColumn(i, dividers, gap)
			}
		}
		off++ // space before │
		crosses = append(crosses, off)
	}
	if len(crosses) == 0 {
		return styleMeta.Sprint(strings.Repeat("─", totalWidth))
	}
	slices.Sort(crosses)
	var b strings.Builder
	last := 0
	for _, p := range crosses {
		if p > last {
			b.WriteString(styleMeta.Sprint(strings.Repeat("─", p-last)))
		}
		b.WriteString(stylePlain.Sprint("┼"))
		last = p + 1
	}
	if last < totalWidth {
		b.WriteString(styleMeta.Sprint(strings.Repeat("─", totalWidth-last)))
	}
	return b.String()
}

// buildSummaryRow builds a cluster-wide summary row formatted identically to node rows.
func buildSummaryRow(infos []*nodeInfo, o tableOpts) []cell {
	var (
		cpu, mem       amount
		totalPodsReq   int64
		totalPodsAlloc int64
		gpus           = map[corev1.ResourceName]*amount{}
		gpuModels      []string
		gpuLevel       = lvlOK
	)
	for _, ni := range infos {
		c, m := ni.get(corev1.ResourceCPU), ni.get(corev1.ResourceMemory)
		cpu.req += c.req
		cpu.alloc += c.alloc
		cpu.lim += c.lim
		cpu.used += c.used
		if c.hasUsed {
			cpu.hasUsed = true
		}

		mem.req += m.req
		mem.alloc += m.alloc
		mem.lim += m.lim
		mem.used += m.used
		if m.hasUsed {
			mem.hasUsed = true
		}

		pods := ni.get(corev1.ResourcePods)
		totalPodsReq += pods.req
		totalPodsAlloc += pods.alloc

		for _, a := range ni.accels {
			g := gpus[a.name]
			if g == nil {
				g = &amount{}
				gpus[a.name] = g
			}
			g.req += a.req
			g.alloc += a.alloc
		}
		if ni.gpuModel != "" && !slices.Contains(gpuModels, ni.gpuModel) {
			gpuModels = append(gpuModels, ni.gpuModel)
		}
		if ni.gpuLevel > gpuLevel {
			gpuLevel = ni.gpuLevel
		}
	}

	row := []cell{plain("Summary")}
	for _, r := range []struct {
		name corev1.ResourceName
		a    amount
	}{
		{corev1.ResourceCPU, cpu},
		{corev1.ResourceMemory, mem},
	} {
		if o.wide {
			req, p, known := valueWithPct(r.name, r.a.req, r.a.alloc)
			lim, lp, limKnown := valueWithPct(r.name, r.a.lim, r.a.alloc)
			row = append(row,
				percentageCell(req, p, known),
				percentageCell(lim, lp, limKnown),
				plain(fmtAmount(r.name, r.a.alloc)),
				plain(fmtAmount(r.name, r.a.avail())),
			)
		} else {
			p, known := pct(r.a.req, r.a.alloc)
			row = append(row, percentageCell(fmt.Sprintf("%d%%: %s/%s", p, fmtAmount(r.name, r.a.req), fmtAmount(r.name, r.a.alloc)), p, known))
		}
		if o.hasMetrics {
			row = append(row, usageCell(r.name, r.a, o.wide))
		}
	}

	if o.showGPU {
		if len(gpus) == 0 {
			row = append(row, dim("-"), dim("-"), dim("-"), dim("-"), dim("-"))
		} else {
			st := styleAccent
			if gpuLevel != lvlOK {
				st = gpuLevel.style()
			}
			names := make([]corev1.ResourceName, 0, len(gpus))
			for name := range gpus {
				names = append(names, name)
			}
			slices.Sort(names)

			var types, reqs, allocs, avails []string
			for _, name := range names {
				g := gpus[name]
				types = append(types, acceleratorLabel(name))
				reqs = append(reqs, strconv.FormatInt(g.req, 10))
				allocs = append(allocs, strconv.FormatInt(g.alloc, 10))
				avails = append(avails, strconv.FormatInt(g.alloc-g.req, 10))
			}

			modelText := "-"
			if len(gpuModels) > 0 {
				modelText = strings.Join(gpuModels, ",")
			}
			modelCell := dim(modelText)
			if modelText != "-" {
				modelCell = cell{text: modelText, style: st}
			}

			row = append(row,
				cell{text: strings.Join(types, ","), style: st},
				plain(strings.Join(reqs, ",")),
				plain(strings.Join(allocs, ",")),
				plain(strings.Join(avails, ",")),
				modelCell,
			)
		}
	}

	if o.wide {
		// Avoid presenting a partial sum as a cluster total.
		total := &filesystemUsage{Used: new(int64), Available: new(int64)}
		for _, ni := range infos {
			if !ni.storage.valid() || ni.storage.Capacity > math.MaxInt64-total.Capacity {
				total = nil
				break
			}
			total.Capacity += ni.storage.Capacity
			*total.Used += *ni.storage.Used
			*total.Available += *ni.storage.Available
		}
		row = append(row, storageCells(total)...)
	}
	pp, _ := pct(totalPodsReq, totalPodsAlloc)
	row = append(row, levelCell(fmt.Sprintf("%d/%d", totalPodsReq, totalPodsAlloc), podsLevel(pp)))

	if o.wide {
		row = append(row,
			dim("-"),
			dim("-"),
			dim("-"),
			dim("-"),
			dim("-"),
			dim("-"),
			dim("-"),
			dim("-"),
		)
	}

	row = append(row, dim("-"))
	if o.wide {
		_, rows := compactWideTable(tableHeaders(o), [][]cell{row})
		return rows[0]
	}
	return row
}

func ratio(a amount) float64 {
	if a.alloc <= 0 {
		return 0
	}
	return float64(a.req) / float64(a.alloc)
}

func gpuRatio(ni *nodeInfo) float64 {
	var req, alloc int64
	for _, a := range ni.accels {
		req += a.req
		alloc += a.alloc
	}
	return ratio(amount{req: req, alloc: alloc})
}

func sortInfos(infos []*nodeInfo, by string) {
	key := func(ni *nodeInfo) float64 {
		switch by {
		case "cpu":
			return ratio(ni.get(corev1.ResourceCPU))
		case "mem":
			return ratio(ni.get(corev1.ResourceMemory))
		case "gpu":
			return gpuRatio(ni)
		case "pods":
			return ratio(ni.get(corev1.ResourcePods))
		case "age":
			return -float64(ni.node.CreationTimestamp.Unix()) // oldest first
		}
		return 0
	}
	slices.SortStableFunc(infos, func(a, b *nodeInfo) int {
		if by == "nodegroup" {
			if c := cmp.Compare(a.cloud.pool, b.cloud.pool); c != 0 {
				return c
			}
		}
		if c := cmp.Compare(key(b), key(a)); c != 0 {
			return c
		}
		return cmp.Compare(a.node.Name, b.node.Name)
	})
}

// ---------------------------------------------------------------------------
// Detail view
// ---------------------------------------------------------------------------

// resourceOrder lists the well-known resources first in the detail table.
var resourceOrder = []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage, corev1.ResourcePods}

func renderDetail(w io.Writer, ni *nodeInfo, snap *snapshot, now time.Time) {
	n := ni.node
	field := func(label, value string) {
		_, _ = fmt.Fprintf(w, "  %s %s\n", styleKey.Sprintf("%-11s", label), value)
	}
	cont := func(value string) {
		_, _ = fmt.Fprintf(w, "  %-11s %s\n", "", value)
	}

	_, _ = fmt.Fprintln(w, styleTitle.Sprint("── "+n.Name+" ")+styleMeta.Sprint(strings.Repeat("─", max(0, 60-utf8.RuneCountInString(n.Name)))))
	field("Status:", ni.statusLevel.style().Sprint(ni.status)+styleMeta.Sprint("  roles: "+ni.roles))

	// Health: the "why".
	if len(ni.issues) == 0 {
		field("Health:", styleOK.Sprint("✓ no issues"))
	} else {
		for i, is := range ni.issues {
			line := is.level.style().Sprint(is.level.marker() + " " + is.text)
			if i == 0 {
				field("Health:", line)
			} else {
				cont(line)
			}
		}
	}

	if ni.storage.valid() {
		field("Ephemeral:", fmt.Sprintf("nodefs used %s, available %s / capacity %s", fmtBytes(*ni.storage.Used), fmtBytes(*ni.storage.Available), fmtBytes(ni.storage.Capacity)))
	} else if err := snap.storageErrors[n.Name]; err != nil {
		field("Ephemeral:", styleMeta.Sprint("unavailable: "+err.Error()))
	} else {
		field("Ephemeral:", styleMeta.Sprint("unavailable: node filesystem statistics not reported"))
	}

	// Conditions, compact, with problems colored.
	var conds []string
	for _, c := range n.Status.Conditions {
		s := string(c.Type) + "=" + string(c.Status)
		switch {
		case conditionHealthy(c):
			conds = append(conds, styleOK.Sprint(s))
		case c.Type == corev1.NodeReady || c.Type == corev1.NodeNetworkUnavailable:
			conds = append(conds, styleBad.Sprint(s))
		default:
			conds = append(conds, styleWarn.Sprint(s))
		}
	}
	if len(conds) > 0 {
		field("Conditions:", strings.Join(conds, "  "))
	}
	if c := findCondition(n, corev1.NodeReady); c != nil && !c.LastHeartbeatTime.IsZero() {
		hb := now.Sub(c.LastHeartbeatTime.Time)
		st := styleMeta
		if hb > 5*time.Minute {
			st = styleWarn
		}
		cont(st.Sprint("last heartbeat " + duration.HumanDuration(hb) + " ago"))
	}

	if len(n.Spec.Taints) == 0 {
		field("Taints:", styleMeta.Sprint("<none>"))
	} else {
		for i, t := range n.Spec.Taints {
			s := t.Key
			if t.Value != "" {
				s += "=" + t.Value
			}
			s += ":" + string(t.Effect)
			if i == 0 {
				field("Taints:", s)
			} else {
				cont(s)
			}
		}
	}

	_, _ = fmt.Fprintln(w, "  "+styleKey.Sprint("Resources:"))
	headers, rows := detailResourceRows(ni, snap.usage != nil)
	printTable(w, "    ", headers, rows)

	if ni.gpuModel != "" {
		field("GPU model:", styleAccent.Sprint(ni.gpuModel))
	}

	info := n.Status.NodeInfo
	field("System:", strings.Join(nonEmpty(
		"kubelet "+info.KubeletVersion,
		info.ContainerRuntimeVersion,
		info.OperatingSystem+"/"+info.Architecture,
		info.OSImage,
		"kernel "+info.KernelVersion,
	), styleMeta.Sprint(" · ")))

	c := ni.cloud
	var cloud []string
	if c.instanceType != "" {
		s := c.instanceType
		if c.capacityType != "" {
			s += " (" + c.capacityType + ")"
		}
		cloud = append(cloud, s)
	} else if c.capacityType != "" {
		cloud = append(cloud, c.capacityType)
	}
	if c.zone != "" {
		cloud = append(cloud, "zone "+c.zone)
	}
	if c.pool != "" {
		cloud = append(cloud, "node group "+c.pool)
	}
	if n.Spec.ProviderID != "" {
		cloud = append(cloud, styleMeta.Sprint(n.Spec.ProviderID))
	}
	if len(cloud) > 0 {
		field("Cloud:", strings.Join(cloud, styleMeta.Sprint(" · ")))
	}

	var addrs []string
	for _, a := range n.Status.Addresses {
		addrs = append(addrs, string(a.Type)+"="+a.Address)
	}
	if len(addrs) > 0 {
		field("Addresses:", strings.Join(addrs, "  "))
	}
	field("Created:", n.CreationTimestamp.Local().Format("2006-01-02 15:04")+styleMeta.Sprint(" ("+duration.HumanDuration(now.Sub(n.CreationTimestamp.Time))+" ago)"))

	switch {
	case snap.eventsErr != nil:
		field("Events:", styleMeta.Sprint("unavailable: "+snap.eventsErr.Error()))
	case len(snap.events[n.Name]) == 0:
		field("Events:", styleMeta.Sprint("no recent Warning events"))
	default:
		evs := snap.events[n.Name]
		for i, e := range evs[:min(len(evs), maxEvents)] {
			age := "-"
			if t := eventTime(e); !t.IsZero() {
				age = duration.HumanDuration(now.Sub(t))
			}
			s := styleMeta.Sprintf("%-5s", age) + " " + styleWarn.Sprint(e.Reason)
			if e.Count > 1 {
				s += styleMeta.Sprintf(" (x%d)", e.Count)
			}
			if msg := strings.TrimSpace(e.Message); msg != "" {
				s += " " + msg
			}
			if i == 0 {
				field("Events:", s)
			} else {
				cont(s)
			}
		}
	}
}

func nonEmpty(parts ...string) []string {
	return slices.DeleteFunc(parts, func(s string) bool {
		s = strings.TrimSpace(s)
		return s == "" || s == "/" || s == "kubelet" || s == "kernel"
	})
}

func detailResourceRows(ni *nodeInfo, hasMetrics bool) ([]string, [][]cell) {
	headers := []string{"RESOURCE", "CAPACITY", "ALLOCATABLE", "REQUESTS", "LIMITS", "AVAILABLE"}
	if hasMetrics {
		headers = append(headers, "USAGE")
	}

	var names []corev1.ResourceName
	for _, name := range resourceOrder {
		if _, ok := ni.res[name]; ok {
			names = append(names, name)
		}
	}
	var rest []corev1.ResourceName
	for name, a := range ni.res {
		if slices.Contains(resourceOrder, name) || (a.capacity == 0 && a.alloc == 0 && a.req == 0) {
			continue
		}
		rest = append(rest, name)
	}
	slices.Sort(rest)
	names = append(names, rest...)

	rows := make([][]cell, 0, len(names))
	for _, name := range names {
		a := ni.get(name)
		req, p, known := valueWithPct(name, a.req, a.alloc)
		reqLvl := pctLevel(p)
		switch {
		case name == corev1.ResourcePods:
			reqLvl = podsLevel(p)
		case isAccelerator(name):
			reqLvl = lvlOK
		}
		lim, lp, limKnown := valueWithPct(name, a.lim, a.alloc)
		if name == corev1.ResourcePods {
			lim = "-"
		}
		allocCell := plain(fmtAmount(name, a.alloc))
		if isAccelerator(name) && a.alloc < a.capacity {
			allocCell = cell{text: allocCell.text, style: styleWarn}
		}
		reqCell, limCell := levelCell(req, reqLvl), dim(lim)
		if name == corev1.ResourceCPU || name == corev1.ResourceMemory {
			reqCell = percentageCell(req, p, known)
			limCell = percentageCell(lim, lp, limKnown)
		}
		row := []cell{
			styleCellFor(name),
			plain(fmtAmount(name, a.capacity)),
			allocCell,
			reqCell,
			limCell,
			plain(fmtAmount(name, a.avail())),
		}
		if hasMetrics {
			switch {
			case a.hasUsed:
				s, up, known := valueWithPct(name, a.used, a.alloc)
				usedCell := levelCell(s, pctLevel(up))
				if name == corev1.ResourceCPU || name == corev1.ResourceMemory {
					usedCell = percentageCell(s, up, known)
				}
				row = append(row, usedCell)
			default:
				row = append(row, dim("-"))
			}
		}
		rows = append(rows, row)
	}
	return headers, rows
}

func styleCellFor(name corev1.ResourceName) cell {
	if isAccelerator(name) {
		return cell{text: string(name), style: styleAccent}
	}
	return cell{text: string(name), style: styleKey}
}

// ---------------------------------------------------------------------------
// Shell completion
// ---------------------------------------------------------------------------

const completeFiles = ":files" // directive: ask the shell for file completion

var (
	valueFlags = map[string]bool{
		"-l": true, "--selector": true,
		"-o": true, "--output": true,
		"--sort": true, "--kubeconfig": true, "--completions": true,
	}
	allFlags = []string{
		"-l", "--selector", "-d", "--detail", "-u", "--unhealthy", "-o", "--output", "--sort",
		"--no-color", "--kubeconfig", "--completions", "-v", "--version", "-h", "--help",
	}
)

type completeState struct {
	kubeconfig string
	selector   string
	nodes      []string
	prevFlag   string // value-taking flag awaiting its argument, if any
}

// scanWords interprets the words that precede the one being completed.
func scanWords(prev []string) completeState {
	var st completeState
	for _, w := range prev {
		if w == "=" && st.prevFlag != "" {
			continue // bash splits "--flag=value" into "--flag", "=", "value"
		}
		if st.prevFlag != "" {
			st.apply(st.prevFlag, w)
			st.prevFlag = ""
			continue
		}
		if name, val, ok := strings.Cut(w, "="); ok && strings.HasPrefix(w, "-") && valueFlags[name] {
			st.apply(name, val)
			continue
		}
		switch {
		case valueFlags[w]:
			st.prevFlag = w
		case strings.HasPrefix(w, "-"):
			// boolean flag
		default:
			st.nodes = append(st.nodes, w)
		}
	}
	return st
}

func (st *completeState) apply(flagName, val string) {
	switch flagName {
	case "--kubeconfig":
		st.kubeconfig = val
	case "-l", "--selector":
		st.selector = val
	}
}

// complete returns completion candidates for the last word in words (the word
// under the cursor, possibly empty). newClient is only invoked when a cluster
// lookup is needed; any error yields no candidates.
func complete(ctx context.Context, words []string, newClient func(kubeconfig string) (kubernetes.Interface, error)) []string {
	if len(words) == 0 {
		words = []string{""}
	}
	cur := words[len(words)-1]
	if cur == "=" {
		cur = ""
	}
	st := scanWords(words[:len(words)-1])

	// "--sort=<partial>" style: complete the value, keep the prefix.
	if name, partial, ok := strings.Cut(cur, "="); ok && strings.HasPrefix(cur, "-") && valueFlags[name] {
		cands := complete(ctx, append(append([]string{}, words[:len(words)-1]...), name, partial), newClient)
		for i, c := range cands {
			if c != completeFiles {
				cands[i] = name + "=" + c
			}
		}
		return cands
	}

	switch st.prevFlag {
	case "--kubeconfig":
		return []string{completeFiles}
	case "--completions":
		return filterPrefix([]string{"bash", "zsh"}, cur)
	case "-o", "--output":
		return filterPrefix([]string{"wide"}, cur)
	case "--sort":
		return filterPrefix(sortKeys, cur)
	case "":
	default:
		return nil // free-form value (label selector)
	}

	if strings.HasPrefix(cur, "-") {
		return filterPrefix(allFlags, cur)
	}

	cs, err := newClient(st.kubeconfig)
	if err != nil {
		return nil
	}
	list, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: st.selector})
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(list.Items))
	for _, n := range list.Items {
		if !slices.Contains(st.nodes, n.Name) {
			names = append(names, n.Name)
		}
	}
	return filterPrefix(names, cur)
}

func filterPrefix(candidates []string, prefix string) []string {
	var out []string
	for _, c := range candidates {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// runComplete implements the hidden `kgn __complete WORDS...` command used by
// the generated shell scripts: it prints one candidate per line and never
// fails loudly (a broken cluster must not spam the user's prompt).
func runComplete(words []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, c := range complete(ctx, words, newClient) {
		fmt.Println(c)
	}
}

func printCompletions(shell string) {
	switch shell {
	case "bash":
		fmt.Print(`_kgn_completions() {
    local cur="${COMP_WORDS[COMP_CWORD]}"
    local out
    out="$("${COMP_WORDS[0]}" __complete "${COMP_WORDS[@]:1:COMP_CWORD}" 2>/dev/null)"
    if [[ "${out%%$'\n'*}" == ":files" ]]; then
        COMPREPLY=($(compgen -f -- "${cur}"))
        return
    fi
    COMPREPLY=($(compgen -W "${out}" -- "${cur}"))
}
complete -F _kgn_completions kgn
`)
	case "zsh":
		fmt.Print(`#compdef kgn
_kgn() {
    local -a out
    out=("${(@f)$(${words[1]} __complete "${(@)words[2,CURRENT]}" 2>/dev/null)}")
    if [[ "${out[1]}" == ":files" ]]; then
        _files
        return
    fi
    out=(${out:#})
    compadd -- "${out[@]}"
}
compdef _kgn kgn
`)
	default:
		fmt.Fprintf(os.Stderr, "unknown shell %q: use bash or zsh\n", shell)
		os.Exit(1)
	}
}

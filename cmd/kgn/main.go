package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
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

// Thresholds (percent) for coloring requested/used CPU and memory.
const (
	warnPct = 80
	critPct = 95
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

type nodeInfo struct {
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

var sortKeys = []string{"name", "cpu", "mem", "gpu", "pods", "age"}

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
	row("--sort", "key", "Sort by "+strings.Join(sortKeys, ", ")+" (cpu/mem/gpu/pods: highest first).", "name")
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
	sortBy := fs.String("sort", "name", "")
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
	printTable(os.Stdout, "", headers, rows)
	if len(infos) > 1 {
		fmt.Println(styleMeta.Sprint(summary(infos, opts)))
	}

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
		infos = append(infos, buildNodeInfo(n, podsByNode[n.Name], usage, ref))
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
	ni.gpuModel = gpuModel(n.Labels)
	gpuIssues := acceleratorHealth(n.Labels, ni.accels)
	ni.gpuLevel = worstLevel(gpuIssues)
	issues = append(issues, gpuIssues...)

	if lvl, msg := kubeletSkew(n.Status.NodeInfo.KubeletVersion, refVersion); lvl != lvlOK {
		ni.versionLevel = lvl
		issues = append(issues, issue{lvl, msg})
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

// conditionHealthy reports whether a condition is in its good state: Ready
// is healthy when True, everything else (pressure conditions and
// node-problem-detector style conditions) when False.
func conditionHealthy(c corev1.NodeCondition) bool {
	if c.Type == corev1.NodeReady {
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
		issues = append(issues, issue{lvlCrit, "Unknown: kubelet has never reported a Ready condition"})
	case c.Status == corev1.ConditionTrue:
		ready = "Ready"
	case c.Status == corev1.ConditionFalse:
		ready = "NotReady"
		issues = append(issues, issue{lvlCrit, describeCondition("NotReady", c)})
	default:
		issues = append(issues, issue{lvlCrit, describeCondition("Unknown: kubelet stopped posting node status", c)})
	}
	parts := []string{ready}

	if n.Spec.Unschedulable {
		parts = append(parts, "SchedulingDisabled")
		issues = append(issues, issue{lvlWarn, "cordoned: new pods will not be scheduled (kubectl uncordon to undo)"})
	}
	if n.DeletionTimestamp != nil {
		parts = append(parts, "Terminating")
		issues = append(issues, issue{lvlWarn, "node object is being deleted"})
	}

	// Only flag pressure conditions while the kubelet is reporting; once
	// Ready is Unknown they all go Unknown and add nothing.
	for i := range n.Status.Conditions {
		c := &n.Status.Conditions[i]
		if c.Type == corev1.NodeReady || c.Status != corev1.ConditionTrue {
			continue
		}
		parts = append(parts, string(c.Type))
		lvl := lvlWarn
		if c.Type == corev1.NodeNetworkUnavailable {
			lvl = lvlCrit // pod networking is not configured; pods can't run
		}
		issues = append(issues, issue{lvl, describeCondition(string(c.Type), c)})
	}

	for _, t := range n.Spec.Taints {
		switch t.Key {
		case "node.kubernetes.io/unreachable":
			issues = append(issues, issue{lvlCrit, "taint " + t.Key + ":" + string(t.Effect) + ": node controller cannot reach the kubelet"})
		case "node.kubernetes.io/not-ready":
			issues = append(issues, issue{lvlCrit, "taint " + t.Key + ":" + string(t.Effect) + ": node controller marked the node not ready"})
		case "node.kubernetes.io/out-of-service":
			issues = append(issues, issue{lvlCrit, "taint " + t.Key + ": node marked out of service, pods are being force-deleted"})
		case "ToBeDeletedByClusterAutoscaler":
			issues = append(issues, issue{lvlWarn, "scheduled for removal by cluster-autoscaler"})
		case "karpenter.sh/disrupted", "karpenter.sh/disruption":
			issues = append(issues, issue{lvlWarn, "being disrupted (drained/replaced) by Karpenter"})
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

func gpuModel(labels map[string]string) string {
	return firstLabel(labels,
		"nvidia.com/gpu.product",
		"cloud.google.com/gke-accelerator",
		"cloud.google.com/gke-tpu-accelerator",
		"karpenter.k8s.aws/instance-gpu-name",
		"amd.com/gpu.product-name",
		"beta.amd.com/gpu.product-name",
	)
}

// acceleratorHealth flags devices the device plugin reports as unhealthy
// (allocatable below capacity) and GPU nodes whose device plugin advertises
// nothing at all.
func acceleratorHealth(labels map[string]string, accels []accel) []issue {
	var issues []issue
	for _, a := range accels {
		if a.capacity > a.alloc {
			issues = append(issues, issue{lvlWarn, fmt.Sprintf("%d of %d %s not allocatable (device plugin reports them unhealthy)", a.capacity-a.alloc, a.capacity, a.name)})
		}
	}
	if n, err := strconv.Atoi(labels["nvidia.com/gpu.count"]); err == nil && n > 0 {
		advertised := slices.ContainsFunc(accels, func(a accel) bool {
			return strings.HasPrefix(string(a.name), "nvidia.com/") && a.capacity > 0
		})
		if !advertised {
			issues = append(issues, issue{lvlWarn, fmt.Sprintf("node is labelled with %d NVIDIA GPU(s) but none are advertised (is the device plugin running?)", n)})
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
		pool: firstLabel(labels,
			"karpenter.sh/nodepool",
			"eks.amazonaws.com/nodegroup",
			"alpha.eksctl.io/nodegroup-name",
			"cloud.google.com/gke-nodepool",
			"kubernetes.azure.com/agentpool",
			"agentpool",
			"node.kubernetes.io/pool",
		),
	}
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

// fmtBytes renders bytes with binary units: "30.1Gi", "512Mi".
func fmtBytes(b int64) string {
	units := []struct {
		suffix string
		size   float64
		prec   int
	}{
		{"Ti", 1 << 40, 1},
		{"Gi", 1 << 30, 1},
		{"Mi", 1 << 20, 0},
		{"Ki", 1 << 10, 0},
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

// valueWithPct renders "3.2 (41%)" or just "3.2" when the percentage is unknown.
func valueWithPct(name corev1.ResourceName, v, whole int64) (string, int, bool) {
	p, ok := pct(v, whole)
	if !ok {
		return fmtAmount(name, v), 0, false
	}
	return fmt.Sprintf("%s (%d%%)", fmtAmount(name, v), p), p, true
}

// ---------------------------------------------------------------------------
// Table
// ---------------------------------------------------------------------------

type cell struct {
	text  string
	style color.Style
}

func plain(s string) cell { return cell{s, stylePlain} }
func dim(s string) cell   { return cell{s, styleMeta} }

// levelCell colors healthy values white and problems yellow/red.
func levelCell(s string, l level) cell {
	if l == lvlOK {
		return plain(s)
	}
	return cell{s, l.style()}
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

func buildTable(infos []*nodeInfo, o tableOpts, now time.Time) ([]string, [][]cell) {
	headers := []string{"NAME", "STATUS", "ROLES"}
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
		if o.wide {
			headers = append(headers, "GPU TYPE", "GPU REQ", "GPU ALLOC", "GPU AVAIL", "GPU MODEL")
		} else {
			headers = append(headers, "GPU")
		}
	}
	headers = append(headers, "PODS")
	if o.wide {
		headers = append(headers, "INSTANCE", "CAPACITY", "ZONE", "POOL", "INTERNAL-IP")
	}
	headers = append(headers, "AGE", "VERSION")

	rows := make([][]cell, 0, len(infos))
	for _, ni := range infos {
		row := []cell{
			plain(ni.node.Name),
			{ni.status, ni.statusLevel.style()},
			dim(ni.roles),
		}
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			a := ni.get(name)
			if o.wide {
				req, p, _ := valueWithPct(name, a.req, a.alloc)
				lim, _, _ := valueWithPct(name, a.lim, a.alloc)
				row = append(row,
					levelCell(req, pctLevel(p)),
					dim(lim),
					plain(fmtAmount(name, a.alloc)),
					plain(fmtAmount(name, a.avail())),
				)
			} else {
				p, _ := pct(a.req, a.alloc)
				row = append(row, levelCell(fmt.Sprintf("%s/%s (%d%%)", fmtAmount(name, a.req), fmtAmount(name, a.alloc), p), pctLevel(p)))
			}
			if o.hasMetrics {
				row = append(row, usageCell(name, a, o.wide))
			}
		}
		if o.showGPU {
			row = append(row, gpuCells(ni, o.wide)...)
		}
		pods := ni.get(corev1.ResourcePods)
		pp, _ := pct(pods.req, pods.alloc)
		row = append(row, levelCell(fmt.Sprintf("%d/%d", pods.req, pods.alloc), podsLevel(pp)))
		if o.wide {
			row = append(row,
				plain(orDash(ni.cloud.instanceType)),
				plain(orDash(ni.cloud.capacityType)),
				plain(orDash(ni.cloud.zone)),
				plain(orDash(ni.cloud.pool)),
				plain(orDash(nodeAddress(ni.node, corev1.NodeInternalIP))),
			)
		}
		row = append(row,
			plain(duration.HumanDuration(now.Sub(ni.node.CreationTimestamp.Time))),
			levelCell(ni.node.Status.NodeInfo.KubeletVersion, ni.versionLevel),
		)
		rows = append(rows, row)
	}
	return headers, rows
}

func usageCell(name corev1.ResourceName, a amount, wide bool) cell {
	if !a.hasUsed {
		return dim("-")
	}
	if wide {
		s, p, _ := valueWithPct(name, a.used, a.alloc)
		return levelCell(s, pctLevel(p))
	}
	p, ok := pct(a.used, a.alloc)
	if !ok {
		return plain(fmtAmount(name, a.used))
	}
	return levelCell(fmt.Sprintf("%d%%", p), pctLevel(p))
}

// gpuCells renders accelerators. GPUs are expected to be fully used on
// dedicated nodes, so they are colored by device health, not by percentage.
func gpuCells(ni *nodeInfo, wide bool) []cell {
	if len(ni.accels) == 0 {
		if wide {
			return []cell{dim("-"), dim("-"), dim("-"), dim("-"), dim("-")}
		}
		return []cell{dim("-")}
	}
	st := styleAccent
	if ni.gpuLevel != lvlOK {
		st = ni.gpuLevel.style()
	}
	var types, reqs, allocs, avails, combined []string
	for _, a := range ni.accels {
		label := acceleratorLabel(a.name)
		types = append(types, label)
		reqs = append(reqs, strconv.FormatInt(a.req, 10))
		allocs = append(allocs, strconv.FormatInt(a.alloc, 10))
		avails = append(avails, strconv.FormatInt(a.avail(), 10))
		combined = append(combined, fmt.Sprintf("%d/%d %s", a.req, a.alloc, label))
	}
	if !wide {
		return []cell{{strings.Join(combined, ", "), st}}
	}
	return []cell{
		{strings.Join(types, ","), st},
		plain(strings.Join(reqs, ",")),
		plain(strings.Join(allocs, ",")),
		plain(strings.Join(avails, ",")),
		dim(orDash(ni.gpuModel)),
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

// printTable pads plain text first, then colors it, so ANSI codes never
// affect alignment.
func printTable(w io.Writer, indent string, headers []string, rows [][]cell) {
	const gap = 2
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], utf8.RuneCountInString(c.text))
		}
	}
	pad := func(s string, i int) string {
		if i == len(widths)-1 {
			return s
		}
		return s + strings.Repeat(" ", widths[i]-utf8.RuneCountInString(s)+gap)
	}

	var hb strings.Builder
	for i, h := range headers {
		hb.WriteString(pad(h, i))
	}
	_, _ = fmt.Fprintln(w, indent+styleTitle.Sprint(hb.String()))
	for _, r := range rows {
		var lb strings.Builder
		for i, c := range r {
			lb.WriteString(c.style.Sprint(pad(c.text, i)))
		}
		_, _ = fmt.Fprintln(w, indent+lb.String())
	}
}

// summary is the cluster-wide footer under the table.
func summary(infos []*nodeInfo, o tableOpts) string {
	var notReady, cordoned, unhealthy int
	var cpu, mem amount
	gpus := map[corev1.ResourceName]*amount{}
	for _, ni := range infos {
		if !strings.HasPrefix(ni.status, "Ready") {
			notReady++
		}
		if ni.node.Spec.Unschedulable {
			cordoned++
		}
		if len(ni.issues) > 0 {
			unhealthy++
		}
		c, m := ni.get(corev1.ResourceCPU), ni.get(corev1.ResourceMemory)
		cpu.req, cpu.alloc, cpu.used = cpu.req+c.req, cpu.alloc+c.alloc, cpu.used+c.used
		mem.req, mem.alloc, mem.used = mem.req+m.req, mem.alloc+m.alloc, mem.used+m.used
		for _, a := range ni.accels {
			g := gpus[a.name]
			if g == nil {
				g = &amount{}
				gpus[a.name] = g
			}
			g.req += a.req
			g.alloc += a.alloc
		}
	}

	parts := []string{fmt.Sprintf("%d nodes", len(infos))}
	var health []string
	if notReady > 0 {
		health = append(health, fmt.Sprintf("%d not ready", notReady))
	}
	if cordoned > 0 {
		health = append(health, fmt.Sprintf("%d cordoned", cordoned))
	}
	if unhealthy > 0 {
		health = append(health, fmt.Sprintf("%d with issues", unhealthy))
	}
	if len(health) > 0 {
		parts[0] += " (" + strings.Join(health, ", ") + ")"
	}
	for _, r := range []struct {
		label string
		name  corev1.ResourceName
		a     amount
	}{{"CPU", corev1.ResourceCPU, cpu}, {"MEM", corev1.ResourceMemory, mem}} {
		p, _ := pct(r.a.req, r.a.alloc)
		s := fmt.Sprintf("%s %s/%s requested (%d%%)", r.label, fmtAmount(r.name, r.a.req), fmtAmount(r.name, r.a.alloc), p)
		if o.hasMetrics {
			up, _ := pct(r.a.used, r.a.alloc)
			s += fmt.Sprintf(", %d%% used", up)
		}
		parts = append(parts, s)
	}
	names := make([]corev1.ResourceName, 0, len(gpus))
	for name := range gpus {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s %d/%d", acceleratorLabel(name), gpus[name].req, gpus[name].alloc))
	}
	return "Total: " + strings.Join(parts, " · ")
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
		cloud = append(cloud, "pool "+c.pool)
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
		req, p, _ := valueWithPct(name, a.req, a.alloc)
		reqLvl := pctLevel(p)
		switch {
		case name == corev1.ResourcePods:
			reqLvl = podsLevel(p)
		case isAccelerator(name):
			reqLvl = lvlOK
		}
		lim, _, _ := valueWithPct(name, a.lim, a.alloc)
		if name == corev1.ResourcePods {
			lim = "-"
		}
		allocCell := plain(fmtAmount(name, a.alloc))
		if isAccelerator(name) && a.alloc < a.capacity {
			allocCell = cell{allocCell.text, styleWarn}
		}
		row := []cell{
			styleCellFor(name),
			plain(fmtAmount(name, a.capacity)),
			allocCell,
			levelCell(req, reqLvl),
			dim(lim),
			plain(fmtAmount(name, a.avail())),
		}
		if hasMetrics {
			switch {
			case a.hasUsed:
				s, up, _ := valueWithPct(name, a.used, a.alloc)
				row = append(row, levelCell(s, pctLevel(up)))
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
		return cell{string(name), styleAccent}
	}
	return cell{string(name), styleKey}
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

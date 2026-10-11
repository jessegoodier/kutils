package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gookit/color"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func init() {
	color.Enable = false
}

func rl(kv ...string) corev1.ResourceList {
	out := corev1.ResourceList{}
	for i := 0; i < len(kv); i += 2 {
		out[corev1.ResourceName(kv[i])] = resource.MustParse(kv[i+1])
	}
	return out
}

func container(req corev1.ResourceList) corev1.Container {
	return corev1.Container{Resources: corev1.ResourceRequirements{Requests: req, Limits: req}}
}

func requests(p *corev1.Pod) map[corev1.ResourceName]int64 {
	return podResources(p, func(r corev1.ResourceRequirements) corev1.ResourceList { return r.Requests })
}

func TestPodResources(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	sidecar := container(rl("cpu", "200m", "memory", "100Mi"))
	sidecar.RestartPolicy = &always

	tests := []struct {
		name string
		spec corev1.PodSpec
		cpu  int64 // millicores
		mem  int64 // bytes
	}{
		{
			name: "app containers add up",
			spec: corev1.PodSpec{Containers: []corev1.Container{
				container(rl("cpu", "250m", "memory", "128Mi")),
				container(rl("cpu", "500m", "memory", "256Mi")),
			}},
			cpu: 750, mem: 384 << 20,
		},
		{
			name: "larger init container wins",
			spec: corev1.PodSpec{
				InitContainers: []corev1.Container{container(rl("cpu", "2", "memory", "64Mi"))},
				Containers:     []corev1.Container{container(rl("cpu", "500m", "memory", "1Gi"))},
			},
			cpu: 2000, mem: 1 << 30,
		},
		{
			name: "sidecars add to app containers and to later init containers",
			spec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					sidecar,
					container(rl("cpu", "1", "memory", "10Mi")), // needs 1 + 0.2 sidecar = 1.2
				},
				Containers: []corev1.Container{container(rl("cpu", "500m", "memory", "200Mi"))},
			},
			cpu: 1200, mem: 300 << 20,
		},
		{
			name: "overhead is added",
			spec: corev1.PodSpec{
				Containers: []corev1.Container{container(rl("cpu", "100m"))},
				Overhead:   rl("cpu", "50m", "memory", "20Mi"),
			},
			cpu: 150, mem: 20 << 20,
		},
		{
			name: "pod-level resources override container sums",
			spec: corev1.PodSpec{
				Containers: []corev1.Container{container(rl("cpu", "100m", "memory", "64Mi"))},
				Resources:  &corev1.ResourceRequirements{Requests: rl("cpu", "1")},
			},
			cpu: 1000, mem: 64 << 20,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := requests(&corev1.Pod{Spec: tt.spec})
			if got[corev1.ResourceCPU] != tt.cpu || got[corev1.ResourceMemory] != tt.mem {
				t.Errorf("got cpu=%d mem=%d, want cpu=%d mem=%d", got[corev1.ResourceCPU], got[corev1.ResourceMemory], tt.cpu, tt.mem)
			}
		})
	}
}

func cond(t corev1.NodeConditionType, s corev1.ConditionStatus) corev1.NodeCondition {
	return corev1.NodeCondition{Type: t, Status: s, Reason: "R", Message: "msg"}
}

func TestNodeHealth(t *testing.T) {
	healthy := []corev1.NodeCondition{
		cond(corev1.NodeReady, corev1.ConditionTrue),
		cond(corev1.NodeMemoryPressure, corev1.ConditionFalse),
	}
	tests := []struct {
		name       string
		node       corev1.Node
		wantStatus string
		wantLevel  level
	}{
		{"healthy", corev1.Node{Status: corev1.NodeStatus{Conditions: healthy}}, "Ready", lvlOK},
		{
			"not ready",
			corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{cond(corev1.NodeReady, corev1.ConditionFalse)}}},
			"NotReady", lvlCrit,
		},
		{
			"unknown",
			corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{cond(corev1.NodeReady, corev1.ConditionUnknown)}}},
			"Unknown", lvlCrit,
		},
		{"no conditions", corev1.Node{}, "Unknown", lvlCrit},
		{
			"cordoned",
			corev1.Node{Spec: corev1.NodeSpec{Unschedulable: true}, Status: corev1.NodeStatus{Conditions: healthy}},
			"Ready,SchedulingDisabled", lvlWarn,
		},
		{
			"memory pressure",
			corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				cond(corev1.NodeReady, corev1.ConditionTrue),
				cond(corev1.NodeMemoryPressure, corev1.ConditionTrue),
			}}},
			"Ready,MemoryPressure", lvlWarn,
		},
		{
			"node-problem-detector condition",
			corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				cond(corev1.NodeReady, corev1.ConditionTrue),
				cond("KernelDeadlock", corev1.ConditionTrue),
			}}},
			"Ready,KernelDeadlock", lvlWarn,
		},
		{
			"network unavailable",
			corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				cond(corev1.NodeReady, corev1.ConditionTrue),
				cond(corev1.NodeNetworkUnavailable, corev1.ConditionTrue),
			}}},
			"Ready,NetworkUnavailable", lvlCrit,
		},
		{
			"unreachable taint",
			corev1.Node{
				Spec:   corev1.NodeSpec{Taints: []corev1.Taint{{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoExecute}}},
				Status: corev1.NodeStatus{Conditions: healthy},
			},
			"Ready", lvlCrit,
		},
		{
			"cluster-autoscaler removal",
			corev1.Node{
				Spec:   corev1.NodeSpec{Taints: []corev1.Taint{{Key: "ToBeDeletedByClusterAutoscaler", Effect: corev1.TaintEffectNoSchedule}}},
				Status: corev1.NodeStatus{Conditions: healthy},
			},
			"Ready", lvlWarn,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, issues := nodeHealth(&tt.node)
			if status != tt.wantStatus {
				t.Errorf("status = %q, want %q", status, tt.wantStatus)
			}
			if got := worstLevel(issues); got != tt.wantLevel {
				t.Errorf("level = %d, want %d (issues: %v)", got, tt.wantLevel, issues)
			}
		})
	}
}

func TestNodeHealthDescribesWhy(t *testing.T) {
	n := corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
		Type: corev1.NodeReady, Status: corev1.ConditionFalse,
		Reason: "KubeletNotReady", Message: "container runtime network not ready",
	}}}}
	_, issues := nodeHealth(&n)
	if len(issues) != 1 || !strings.Contains(issues[0].text, "KubeletNotReady - container runtime network not ready") {
		t.Errorf("issues = %v", issues)
	}
}

func TestKubeletSkew(t *testing.T) {
	tests := []struct {
		kubelet, ref string
		want         level
	}{
		{"v1.34.2", "v1.34.0", lvlOK},
		{"v1.34.2-eks-abc123", "v1.34.1-eks-def456", lvlOK},
		{"v1.33.5", "v1.34.0", lvlWarn},
		{"v1.31.0", "v1.34.0", lvlWarn},
		{"v1.30.0", "v1.34.0", lvlCrit},
		{"v1.35.0", "v1.34.0", lvlCrit},
		{"garbage", "v1.34.0", lvlOK},
		{"v1.34.0", "", lvlOK},
	}
	for _, tt := range tests {
		if got, _ := kubeletSkew(tt.kubelet, tt.ref); got != tt.want {
			t.Errorf("kubeletSkew(%q, %q) = %d, want %d", tt.kubelet, tt.ref, got, tt.want)
		}
	}
}

func TestNewestKubelet(t *testing.T) {
	nodes := []corev1.Node{
		{Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.33.4"}}},
		{Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.34.1"}}},
		{Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.9.0"}}},
	}
	if got := newestKubelet(nodes); got != "v1.34.1" {
		t.Errorf("newestKubelet = %q", got)
	}
}

func TestFormatting(t *testing.T) {
	cpu := map[int64]string{0: "0", 250: "0.25", 100: "0.1", 1000: "1", 7910: "7.9", 15500: "15.5", 64000: "64"}
	for in, want := range cpu {
		if got := fmtCPU(in); got != want {
			t.Errorf("fmtCPU(%d) = %q, want %q", in, got, want)
		}
	}
	mem := map[int64]string{0: "0", 512: "512", 2048: "2Ki", 512 << 20: "512Mi", 30 << 30: "30Gi", 32315650048: "30.1Gi", 2 << 40: "2Ti"}
	for in, want := range mem {
		if got := fmtBytes(in); got != want {
			t.Errorf("fmtBytes(%d) = %q, want %q", in, got, want)
		}
	}
	if got := fmtAmount("nvidia.com/gpu", 4); got != "4" {
		t.Errorf("fmtAmount gpu = %q", got)
	}
	if p, ok := pct(1, 3); !ok || p != 33 {
		t.Errorf("pct(1,3) = %d %v", p, ok)
	}
	if _, ok := pct(1, 0); ok {
		t.Error("pct with zero whole should be unknown")
	}
	if pctLevel(80) != lvlOK || pctLevel(81) != lvlWarn || pctLevel(96) != lvlCrit {
		t.Error("pctLevel thresholds")
	}
	if podsLevel(89) != lvlOK || podsLevel(90) != lvlWarn || podsLevel(100) != lvlCrit {
		t.Error("podsLevel thresholds")
	}
}

func TestAccelerators(t *testing.T) {
	labels := map[corev1.ResourceName]string{
		"nvidia.com/gpu":            "nvidia",
		"nvidia.com/mig-1g.5gb":     "mig-1g.5gb",
		"amd.com/gpu":               "amd",
		"gpu.intel.com/i915":        "intel-i915",
		"aws.amazon.com/neuron":     "neuron",
		"aws.amazon.com/neuroncore": "neuroncore",
		"google.com/tpu":            "tpu",
		"habana.ai/gaudi":           "gaudi",
	}
	for name, want := range labels {
		if !isAccelerator(name) {
			t.Errorf("isAccelerator(%q) = false", name)
		}
		if got := acceleratorLabel(name); got != want {
			t.Errorf("acceleratorLabel(%q) = %q, want %q", name, got, want)
		}
	}
	for _, name := range []corev1.ResourceName{"cpu", "memory", "hugepages-2Mi", "example.com/foo", "aws.amazon.com/vpc-ip"} {
		if isAccelerator(name) {
			t.Errorf("isAccelerator(%q) = true", name)
		}
	}
}

func TestAcceleratorHealth(t *testing.T) {
	ok := []accel{{name: "nvidia.com/gpu", amount: amount{capacity: 4, alloc: 4}}}
	if issues := acceleratorHealth(map[string]string{"nvidia.com/gpu.count": "4"}, ok); len(issues) != 0 {
		t.Errorf("healthy GPUs: %v", issues)
	}
	bad := []accel{{name: "nvidia.com/gpu", amount: amount{capacity: 8, alloc: 6}}}
	if issues := acceleratorHealth(nil, bad); len(issues) != 1 || !strings.Contains(issues[0].text, "2 of 8") {
		t.Errorf("unhealthy GPUs: %v", issues)
	}
	if issues := acceleratorHealth(map[string]string{"nvidia.com/gpu.count": "8"}, nil); len(issues) != 1 {
		t.Errorf("missing device plugin: %v", issues)
	}
	mig := []accel{{name: "nvidia.com/mig-1g.5gb", amount: amount{capacity: 7, alloc: 7}}}
	if issues := acceleratorHealth(map[string]string{"nvidia.com/gpu.count": "1"}, mig); len(issues) != 0 {
		t.Errorf("MIG-only node should count as advertised: %v", issues)
	}
}

func TestCloudMetadata(t *testing.T) {
	tests := []struct {
		labels map[string]string
		want   cloudInfo
	}{
		{
			map[string]string{
				"node.kubernetes.io/instance-type": "m7i.2xlarge",
				"topology.kubernetes.io/zone":      "us-east-1a",
				"karpenter.sh/capacity-type":       "spot",
				"karpenter.sh/nodepool":            "default",
			},
			cloudInfo{"m7i.2xlarge", "spot", "us-east-1a", "default"},
		},
		{
			map[string]string{"eks.amazonaws.com/capacityType": "ON_DEMAND", "eks.amazonaws.com/nodegroup": "ng-1"},
			cloudInfo{capacityType: "on-demand", pool: "ng-1"},
		},
		{
			map[string]string{"cloud.google.com/gke-spot": "true", "cloud.google.com/gke-nodepool": "pool-a"},
			cloudInfo{capacityType: "spot", pool: "pool-a"},
		},
		{
			map[string]string{"kubernetes.azure.com/scalesetpriority": "spot", "kubernetes.azure.com/agentpool": "gpu"},
			cloudInfo{capacityType: "spot", pool: "gpu"},
		},
		{nil, cloudInfo{}},
	}
	for _, tt := range tests {
		if got := cloudMetadata(tt.labels); got != tt.want {
			t.Errorf("cloudMetadata(%v) = %+v, want %+v", tt.labels, got, tt.want)
		}
	}
}

func TestNodeRoles(t *testing.T) {
	if got := nodeRoles(map[string]string{"node-role.kubernetes.io/control-plane": "", "node-role.kubernetes.io/master": ""}); got != "control-plane,master" {
		t.Errorf("roles = %q", got)
	}
	if got := nodeRoles(map[string]string{"kubernetes.io/role": "worker"}); got != "worker" {
		t.Errorf("roles = %q", got)
	}
	if got := nodeRoles(nil); got != "<none>" {
		t.Errorf("roles = %q", got)
	}
}

func testNode(name string, alloc corev1.ResourceList, labels map[string]string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, CreationTimestamp: metav1.NewTime(time.Now().Add(-48 * time.Hour))},
		Status: corev1.NodeStatus{
			Capacity:    alloc,
			Allocatable: alloc,
			Conditions:  []corev1.NodeCondition{cond(corev1.NodeReady, corev1.ConditionTrue)},
			NodeInfo:    corev1.NodeSystemInfo{KubeletVersion: "v1.34.1"},
		},
	}
}

func testPod(node string, phase corev1.PodPhase, req corev1.ResourceList) corev1.Pod {
	return corev1.Pod{
		Spec:   corev1.PodSpec{NodeName: node, Containers: []corev1.Container{container(req)}},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func testSnapshot() *snapshot {
	return &snapshot{
		nodes: []corev1.Node{
			testNode("cpu-1", rl("cpu", "4", "memory", "16Gi", "pods", "110"), nil),
			testNode("gpu-1", rl("cpu", "8", "memory", "64Gi", "pods", "110", "nvidia.com/gpu", "4"),
				map[string]string{"nvidia.com/gpu.product": "NVIDIA-A10G"}),
		},
		pods: []corev1.Pod{
			testPod("cpu-1", corev1.PodRunning, rl("cpu", "1", "memory", "4Gi")),
			testPod("cpu-1", corev1.PodPending, rl("cpu", "500m")),
			testPod("cpu-1", corev1.PodSucceeded, rl("cpu", "3")), // finished: ignored
			testPod("gpu-1", corev1.PodRunning, rl("cpu", "2", "nvidia.com/gpu", "2")),
			testPod("", corev1.PodPending, rl("cpu", "1")), // unscheduled: ignored
		},
		usage:         map[string]corev1.ResourceList{"cpu-1": rl("cpu", "3800m", "memory", "2Gi")},
		serverVersion: "v1.34.0",
	}
}

func TestBuildInfos(t *testing.T) {
	infos := buildInfos(testSnapshot())
	if len(infos) != 2 {
		t.Fatalf("got %d infos", len(infos))
	}
	c := infos[0]
	if cpu := c.get(corev1.ResourceCPU); cpu.req != 1500 || cpu.alloc != 4000 || cpu.used != 3800 || !cpu.hasUsed {
		t.Errorf("cpu-1 cpu = %+v", cpu)
	}
	if pods := c.get(corev1.ResourcePods); pods.req != 2 || pods.alloc != 110 {
		t.Errorf("cpu-1 pods = %+v", pods)
	}
	if len(c.accels) != 0 || len(c.issues) != 0 {
		t.Errorf("cpu-1 accels=%v issues=%v", c.accels, c.issues)
	}

	g := infos[1]
	if len(g.accels) != 1 || g.accels[0].req != 2 || g.accels[0].alloc != 4 {
		t.Errorf("gpu-1 accels = %+v", g.accels)
	}
	if g.gpuModel != "NVIDIA-A10G" {
		t.Errorf("gpu-1 model = %q", g.gpuModel)
	}
	if g.get(corev1.ResourceCPU).hasUsed {
		t.Error("gpu-1 has no metrics and should not report usage")
	}
}

func TestBuildTable(t *testing.T) {
	infos := buildInfos(testSnapshot())
	now := time.Now()

	headers, rows := buildTable(infos, tableOpts{hasMetrics: true, showGPU: true}, now)
	want := []string{"NAME", "STATUS", "ROLES", "CPU REQ", "CPU USE", "MEM REQ", "MEM USE", "GPU", "PODS", "AGE", "VERSION"}
	if !slices.Equal(headers, want) {
		t.Fatalf("headers = %v", headers)
	}
	texts := func(r []cell) []string {
		var out []string
		for _, c := range r {
			out = append(out, c.text)
		}
		return out
	}
	r0 := texts(rows[0])
	if r0[3] != "1.5/4 (38%)" || r0[4] != "95%" || r0[5] != "4Gi/16Gi (25%)" || r0[7] != "-" || r0[8] != "2/110" {
		t.Errorf("row 0 = %v", r0)
	}
	if rows[0][4].style.String() != styleWarn.String() {
		t.Error("95% usage should be yellow")
	}
	r1 := texts(rows[1])
	if r1[4] != "-" || r1[7] != "2/4 nvidia" {
		t.Errorf("row 1 = %v", r1)
	}

	headers, rows = buildTable(infos, tableOpts{wide: true, showGPU: true}, now)
	if slices.Contains(headers, "CPU USE") || !slices.Contains(headers, "GPU MODEL") || !slices.Contains(headers, "INSTANCE") {
		t.Errorf("wide headers = %v", headers)
	}
	for _, r := range rows {
		if len(r) != len(headers) {
			t.Errorf("row has %d cells, want %d", len(r), len(headers))
		}
	}

	headers, _ = buildTable(infos[:1], tableOpts{}, now)
	if slices.Contains(headers, "GPU") {
		t.Errorf("GPU column should be hidden without accelerators: %v", headers)
	}
}

func TestPrintTableAligns(t *testing.T) {
	var buf bytes.Buffer
	printTable(&buf, "", []string{"A", "B"}, [][]cell{{plain("long-value"), plain("x")}, {plain("s"), plain("y")}})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 || strings.Index(lines[1], "x") != strings.Index(lines[2], "y") || strings.Index(lines[0], "B") != strings.Index(lines[1], "x") {
		t.Errorf("misaligned:\n%s", buf.String())
	}
}

func TestSummary(t *testing.T) {
	infos := buildInfos(testSnapshot())
	infos[0].node.Spec.Unschedulable = true
	infos[0].issues = append(infos[0].issues, issue{lvlWarn, "cordoned"})
	got := summary(infos, tableOpts{})
	for _, want := range []string{"2 nodes (1 cordoned, 1 with issues)", "CPU 3.5/12 requested (29%)", "nvidia 2/4"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q missing %q", got, want)
		}
	}
}

func TestSortInfos(t *testing.T) {
	infos := buildInfos(testSnapshot())
	sortInfos(infos, "cpu")
	if infos[0].node.Name != "cpu-1" { // 38% vs 25%
		t.Errorf("cpu sort: first = %s", infos[0].node.Name)
	}
	sortInfos(infos, "gpu")
	if infos[0].node.Name != "gpu-1" {
		t.Errorf("gpu sort: first = %s", infos[0].node.Name)
	}
	sortInfos(infos, "name")
	if infos[0].node.Name != "cpu-1" {
		t.Errorf("name sort: first = %s", infos[0].node.Name)
	}
}

func TestRenderDetail(t *testing.T) {
	snap := testSnapshot()
	snap.nodes[1].Status.Allocatable = rl("cpu", "8", "memory", "64Gi", "pods", "110", "nvidia.com/gpu", "3")
	snap.nodes[1].Spec.Taints = []corev1.Taint{{Key: "nvidia.com/gpu", Value: "present", Effect: corev1.TaintEffectNoSchedule}}
	snap.usage = nil
	snap.events = map[string][]corev1.Event{"gpu-1": {{Reason: "XidError", Message: "GPU fell off the bus", Count: 3}}}
	infos := buildInfos(snap)

	var buf bytes.Buffer
	renderDetail(&buf, infos[1], snap, time.Now())
	out := buf.String()
	for _, want := range []string{
		"── gpu-1",
		"! 1 of 4 nvidia.com/gpu not allocatable",
		"nvidia.com/gpu=present:NoSchedule",
		"RESOURCE", "nvidia.com/gpu",
		"GPU model:  NVIDIA-A10G",
		"XidError (x3) GPU fell off the bus",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("detail missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "USAGE") {
		t.Error("USAGE column shown without metrics")
	}

	buf.Reset()
	snap.events, snap.eventsErr = nil, errors.New("forbidden")
	renderDetail(&buf, infos[0], snap, time.Now())
	if !strings.Contains(buf.String(), "✓ no issues") || !strings.Contains(buf.String(), "unavailable: forbidden") {
		t.Errorf("detail:\n%s", buf.String())
	}
}

func TestParseNodeMetrics(t *testing.T) {
	raw := []byte(`{"kind":"NodeMetricsList","items":[{"metadata":{"name":"n1"},"usage":{"cpu":"1234567n","memory":"2048Ki"}}]}`)
	m := parseNodeMetrics(raw)
	u := m["n1"]
	if got := qty(corev1.ResourceCPU, u[corev1.ResourceCPU]); got != 2 { // 1.23ms rounds up to 2m
		t.Errorf("cpu = %d", got)
	}
	if got := qty(corev1.ResourceMemory, u[corev1.ResourceMemory]); got != 2048*1024 {
		t.Errorf("memory = %d", got)
	}
	if parseNodeMetrics([]byte("not json")) != nil {
		t.Error("invalid JSON should return nil")
	}
}

func TestFilterNodes(t *testing.T) {
	nodes := []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "a"}}, {ObjectMeta: metav1.ObjectMeta{Name: "b"}}}
	got, err := filterNodes(nodes, []string{"b", "b"})
	if err != nil || len(got) != 1 || got[0].Name != "b" {
		t.Errorf("filterNodes = %v, %v", got, err)
	}
	if _, err := filterNodes(nodes, []string{"a", "zzz"}); err == nil || !strings.Contains(err.Error(), "zzz") {
		t.Errorf("expected not-found error, got %v", err)
	}
}

func TestFetchEventsGroupsAndSorts(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-time.Hour))
	recent := metav1.NewTime(time.Now())
	ev := func(name, node string, ts metav1.Time) *corev1.Event {
		return &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "default"},
			InvolvedObject: corev1.ObjectReference{Kind: "Node", Name: node},
			Type:           corev1.EventTypeWarning,
			LastTimestamp:  ts,
		}
	}
	cs := fake.NewClientset(ev("e1", "n1", old), ev("e2", "n1", recent), ev("e3", "n2", old))
	got, err := fetchNodeEvents(context.Background(), cs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["n1"]) != 2 || got["n1"][0].Name != "e2" || len(got["n2"]) != 1 {
		t.Errorf("events = %v", got)
	}
}

func fakeFactory(nodes ...string) func(string) (kubernetes.Interface, error) {
	cs := fake.NewClientset()
	for _, n := range nodes {
		_, _ = cs.CoreV1().Nodes().Create(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n}}, metav1.CreateOptions{})
	}
	return func(string) (kubernetes.Interface, error) { return cs, nil }
}

func TestComplete(t *testing.T) {
	f := fakeFactory("node-a", "node-b", "gpu-1")
	ctx := context.Background()
	tests := []struct {
		words []string
		want  []string
	}{
		{[]string{""}, []string{"gpu-1", "node-a", "node-b"}},
		{[]string{"node"}, []string{"node-a", "node-b"}},
		{[]string{"node-a", "node"}, []string{"node-b"}},
		{[]string{"--sort", ""}, []string{"age", "cpu", "gpu", "mem", "name", "pods"}},
		{[]string{"--sort=c"}, []string{"--sort=cpu"}},
		{[]string{"-o", ""}, []string{"wide"}},
		{[]string{"--kubeconfig", ""}, []string{completeFiles}},
		{[]string{"--completions", "z"}, []string{"zsh"}},
		{[]string{"-l", ""}, nil},
		{[]string{"--det"}, []string{"--detail"}},
	}
	for _, tt := range tests {
		got := complete(ctx, tt.words, f)
		slices.Sort(got)
		if !slices.Equal(got, tt.want) {
			t.Errorf("complete(%q) = %v, want %v", tt.words, got, tt.want)
		}
	}
}

func TestCompleteClientErrorIsSilent(t *testing.T) {
	failing := func(string) (kubernetes.Interface, error) { return nil, errors.New("no cluster") }
	if got := complete(context.Background(), []string{""}, failing); got != nil {
		t.Errorf("got %v", got)
	}
}

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gookit/color"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
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

func TestReadinessConditions(t *testing.T) {
	kinds := []corev1.NodeConditionType{"KernelReady", "StorageReady", "ContainerRuntimeReady", "NetworkingReady", "AcceleratedHardwareReady"}
	n := corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{cond(corev1.NodeReady, corev1.ConditionTrue)}}}
	for _, kind := range kinds {
		n.Status.Conditions = append(n.Status.Conditions, cond(kind, corev1.ConditionTrue))
	}
	check := func(want string) {
		t.Helper()
		ni := buildNodeInfo(&n, nil, nil, "")
		got := statusCell(ni)
		if got.text != want {
			t.Fatalf("status = %q, want %q", got.text, want)
		}
		if want == "Healthy" && got.style.String() != styleOK.String() {
			t.Error("all readiness checks passing should be green")
		}
	}
	check("Healthy")
	for i, kind := range kinds {
		for _, state := range []corev1.ConditionStatus{corev1.ConditionFalse, corev1.ConditionUnknown} {
			n.Status.Conditions[i+1].Status = state
			if conditionHealthy(n.Status.Conditions[i+1]) {
				t.Errorf("%s=%s should not be healthy", kind, state)
			}
			check(string(kind) + "=" + string(state))
		}
		n.Status.Conditions[i+1].Status = corev1.ConditionTrue
		if !conditionHealthy(n.Status.Conditions[i+1]) {
			t.Errorf("%s=True should be healthy", kind)
		}
	}
	check("Healthy")
}

func TestPercentageFormatting(t *testing.T) {
	got, p, ok := valueWithPct(corev1.ResourceMemory, 512<<20, 2<<30)
	if got != "25%: 512M" || p != 25 || !ok {
		t.Errorf("valueWithPct = %q, %d, %v", got, p, ok)
	}
	got, _, ok = valueWithPct(corev1.ResourceMemory, 512<<20, 0)
	if got != "512M" || ok {
		t.Errorf("unknown percentage = %q, %v", got, ok)
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
	mem := map[int64]string{0: "0", 512: "512", 2048: "2K", 512 << 20: "512M", 30 << 30: "30G", 32315650048: "30.1G", 2 << 40: "2T"}
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

func TestGPUModelFallback(t *testing.T) {
	tests := []struct {
		name, provider, instanceType, want string
		labels                             map[string]string
	}{
		{name: "current fractional GPU node", provider: "aws:///us-east-2a/i-example", instanceType: "g6f.xlarge", want: "NVIDIA-L4"},
		{name: "full L4", provider: "aws:///us-east-2a/i-example", instanceType: "g6.xlarge", want: "NVIDIA-L4"},
		{name: "different G6 variant", provider: "aws:///us-east-2a/i-example", instanceType: "g6e.xlarge", want: "NVIDIA-L40S"},
		{name: "P5", provider: "aws:///us-east-2a/i-example", instanceType: "p5.48xlarge", want: "NVIDIA-H100"},
		{name: "P5 enhanced", provider: "aws:///us-east-2a/i-example", instanceType: "p5en.48xlarge", want: "NVIDIA-H200"},
		{name: "GB200 system physical GPU", provider: "aws:///us-east-2a/i-example", instanceType: "p6e-gb200.36xlarge", want: "NVIDIA-B200"},
		{name: "AMD GPU", provider: "aws:///us-east-2a/i-example", instanceType: "g4ad.xlarge", want: "AMD-Radeon-Pro-V520"},
		{name: "CPU node", provider: "aws:///us-east-2a/i-example", instanceType: "m7a.2xlarge"},
		{name: "unknown family", provider: "aws:///us-east-2a/i-example", instanceType: "g99.xlarge"},
		{name: "missing size", provider: "aws:///us-east-2a/i-example", instanceType: "g6f."},
		{name: "missing dot", provider: "aws:///us-east-2a/i-example", instanceType: "g6f"},
		{name: "non-AWS", provider: "gce://project/zone/node", instanceType: "g6f.xlarge"},
		{name: "unidentified provider", instanceType: "g6f.xlarge"},
		{name: "explicit model takes priority", provider: "aws:///us-east-2a/i-example", instanceType: "g6f.xlarge", labels: map[string]string{"nvidia.com/gpu.product": "NVIDIA-L4-vGPU"}, want: "NVIDIA-L4-vGPU"},
		{name: "explicit model without AWS", labels: map[string]string{"cloud.google.com/gke-accelerator": "nvidia-l4"}, want: "nvidia-l4"},
		{name: "legacy instance type", provider: "aws:///us-east-2a/i-example", labels: map[string]string{"beta.kubernetes.io/instance-type": "g6f.xlarge"}, want: "NVIDIA-L4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := testNode("gpu-node", rl("nvidia.com/gpu", "1"), tt.labels)
			n.Spec.ProviderID = tt.provider
			if n.Labels == nil {
				n.Labels = map[string]string{}
			}
			if tt.instanceType != "" {
				n.Labels["node.kubernetes.io/instance-type"] = tt.instanceType
			}
			ni := buildNodeInfo(&n, nil, nil, "")
			if ni.gpuModel != tt.want {
				t.Fatalf("model = %q, want %q", ni.gpuModel, tt.want)
			}
			for _, wide := range []bool{false, true} {
				headers, rows := buildTable([]*nodeInfo{ni}, tableOpts{wide: wide, showGPU: true}, time.Now())
				got := rows[0][slices.Index(headers, "GPU MODEL")].text
				if got != orDash(tt.want) {
					t.Errorf("GPU MODEL cell = %q, want %q", got, orDash(tt.want))
				}
			}
			if tt.name == "current fractional GPU node" && (ni.accels[0].alloc != 1 || ni.accels[0].capacity != 1) {
				t.Error("inferring the physical model must preserve the device plugin's schedulable count")
			}
		})
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

func TestNodeGroupLabels(t *testing.T) {
	keys := []string{
		"karpenter.sh/nodepool", "eks.amazonaws.com/nodegroup", "alpha.eksctl.io/nodegroup-name",
		"cloud.google.com/gke-nodepool", "kubernetes.azure.com/agentpool", "agentpool",
		"doks.digitalocean.com/node-pool", "ibm-cloud.kubernetes.io/worker-pool-name",
		"k8s.scaleway.com/pool-name", "doks.digitalocean.com/node-pool-id", "k8s.scaleway.com/pool",
		"node.kubernetes.io/pool", "kubernetes.io/nodegroup", "nodegroup", "nodepool",
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			labels := map[string]string{key: "workers"}
			ni := buildNodeInfo(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: labels}}, nil, nil, "")
			headers, rows := buildTable([]*nodeInfo{ni}, tableOpts{wide: true}, time.Now())
			if headers[len(headers)-1] != "NODE GROUP" || rows[0][len(headers)-1].text != "workers" {
				t.Errorf("node group should be the final column: %v, %v", headers, rows[0])
			}
			if slices.Contains(headers, "POOL") {
				t.Error("node group should not also appear in a POOL column")
			}
		})
	}
	for _, labels := range []map[string]string{
		nil, {"unrelated": "workers"}, {"nodegroup": ""},
	} {
		ni := buildNodeInfo(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: labels}}, nil, nil, "")
		headers, rows := buildTable([]*nodeInfo{ni}, tableOpts{wide: true}, time.Now())
		if rows[0][len(headers)-1].text != "-" {
			t.Error("missing node group should display a dash")
		}
	}
	for _, labels := range []map[string]string{
		{"karpenter.sh/nodepool": "workers", "eks.amazonaws.com/nodegroup": "other", "nodegroup": "custom"},
		{"doks.digitalocean.com/node-pool": "workers", "doks.digitalocean.com/node-pool-id": "123", "nodegroup": "custom"},
		{"k8s.scaleway.com/pool-name": "workers", "k8s.scaleway.com/pool": "123"},
		{"eks.amazonaws.com/nodegroup": "", "nodegroup": "workers"},
	} {
		if got := nodeGroup(labels); got != "workers" {
			t.Errorf("label precedence: got %q, want workers", got)
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
	infos[0].cloud.pool = "workers"
	now := time.Now()

	headers, rows := buildTable(infos, tableOpts{hasMetrics: true, showGPU: true}, now)
	want := []string{"NAME", "CPU REQ", "CPU USE", "MEM REQ", "MEM USE", "GPU TYPE", "GPU REQ", "GPU ALLOC", "GPU AVAIL", "GPU MODEL", "PODS", "NODE GROUP"}
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
	if r0[1] != "38%: 1.5/4" || r0[2] != "95%" || r0[3] != "25%: 4G/16G" || r0[5] != "-" || r0[10] != "2/110" || r0[11] != "workers" {
		t.Errorf("row 0 = %v", r0)
	}
	if rows[0][2].style.String() != percentageStyle(95).String() {
		t.Error("95% usage should be yellow")
	}
	r1 := texts(rows[1])
	if r1[2] != "-" || r1[5] != "nvidia" || r1[6] != "2" || r1[7] != "4" || r1[8] != "2" || r1[9] != "NVIDIA-A10G" || r1[11] != "-" {
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

	for header, want := range map[string]string{
		"CPU ALLOC/AVAIL": "4/2.5", "MEM ALLOC/AVAIL": "16G/12G", "EPHEMERAL USED/AVAIL": "-/-",
	} {
		idx := slices.Index(headers, header)
		if idx < 0 || rows[0][idx].text != want {
			t.Errorf("%s: headers=%v rows=%v", header, headers, rows)
		}
	}
	if idx := slices.Index(headers, "GPU ALLOC/AVAIL"); idx < 0 || rows[1][idx].text != "4/2" {
		t.Fatal("GPU pair missing or incorrect")
	}
	for _, metrics := range []bool{false, true} {
		for _, gpu := range []bool{false, true} {
			opts := tableOpts{wide: true, hasMetrics: metrics, showGPU: gpu}
			h, r := buildTable(infos, opts, now)
			summary := buildSummaryRow(infos, opts)
			if len(summary) != len(h) || len(r[0]) != len(h) {
				t.Fatal("wide summary alignment")
			}
			for _, header := range []string{"CPU REQ/LIM", "CPU ALLOC/AVAIL", "MEM REQ/LIM", "MEM ALLOC/AVAIL", "EPHEMERAL USED/AVAIL"} {
				if !slices.Contains(h, header) {
					t.Errorf("missing %s", header)
				}
			}
		}
	}

	headers, _ = buildTable(infos[:1], tableOpts{}, now)
	for _, h := range headers {
		if strings.HasPrefix(h, "GPU") {
			t.Errorf("GPU columns should be hidden without accelerators: %v", headers)
		}
	}
}

func TestTableHealthStatus(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*corev1.Node)
		want   string
		levels []level
	}{
		{name: "healthy", want: "Healthy"},
		{
			name:   "cordoned but ready",
			mutate: func(n *corev1.Node) { n.Spec.Unschedulable = true },
			want:   "SchedulingDisabled", levels: []level{lvlWarn},
		},
		{
			name: "mixed critical and warning",
			mutate: func(n *corev1.Node) {
				n.Status.Conditions[0].Status = corev1.ConditionFalse
				n.Spec.Unschedulable = true
				n.Status.Conditions = append(n.Status.Conditions, cond(corev1.NodeMemoryPressure, corev1.ConditionTrue))
			},
			want: "NotReady,SchedulingDisabled,MemoryPressure", levels: []level{lvlCrit, lvlWarn, lvlWarn},
		},
		{
			name: "taint on ready node",
			mutate: func(n *corev1.Node) {
				n.Spec.Taints = []corev1.Taint{{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoExecute}}
			},
			want: "node.kubernetes.io/unreachable", levels: []level{lvlCrit},
		},
		{
			name:   "missing GPUs",
			mutate: func(n *corev1.Node) { n.Labels = map[string]string{"nvidia.com/gpu.count": "4"} },
			want:   "GPUsMissing", levels: []level{lvlWarn},
		},
		{
			name: "unhealthy GPUs",
			mutate: func(n *corev1.Node) {
				n.Status.Capacity = rl("nvidia.com/gpu", "4")
				n.Status.Allocatable = rl("nvidia.com/gpu", "2")
			},
			want: "nvidia.com/gpuUnhealthy", levels: []level{lvlWarn},
		},
		{
			name:   "version skew",
			mutate: func(n *corev1.Node) { n.Status.NodeInfo.KubeletVersion = "v1.35.0" },
			want:   "KubeletSkew", levels: []level{lvlWarn},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := &corev1.Node{Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{cond(corev1.NodeReady, corev1.ConditionTrue)},
				NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.36.0"},
			}}
			if tt.mutate != nil {
				tt.mutate(n)
			}
			ni := buildNodeInfo(n, nil, nil, "v1.36.0")
			for _, wide := range []bool{false, true} {
				headers, rows := buildTable([]*nodeInfo{ni}, tableOpts{wide: wide}, time.Now())
				if !wide {
					for _, h := range []string{"AGE", "VERSION", "STATUS", "ROLES"} {
						if slices.Contains(headers, h) {
							t.Errorf("compact output should hide %s", h)
						}
					}
					if len(rows[0]) != len(headers) {
						t.Errorf("compact row has %d cells, want %d", len(rows[0]), len(headers))
					}
					continue
				}
				if !slices.Equal(headers[len(headers)-5:], []string{"AGE", "VERSION", "STATUS", "ROLES", "NODE GROUP"}) {
					t.Fatalf("last columns = %v", headers)
				}
				if rows[0][len(headers)-2].text != ni.roles {
					t.Error("penultimate cell should contain node roles")
				}
				got := rows[0][len(headers)-3]
				if got.text != tt.want || len(got.parts) != len(tt.levels) {
					t.Fatalf("status = %+v, want %q with %d problems", got, tt.want, len(tt.levels))
				}
				if len(tt.levels) == 0 && got.style.String() != styleOK.String() {
					t.Error("healthy status should be green")
				}
				for i, lvl := range tt.levels {
					if got.parts[i].style.String() != lvl.style().String() {
						t.Errorf("problem %d has wrong severity color", i)
					}
				}
			}
		})
	}
}

func TestPercentageColorBands(t *testing.T) {
	previous := color.ForceSetColorLevel(color.Level256)
	defer color.ForceSetColorLevel(previous)
	for _, tt := range []struct {
		p    int
		code uint8
	}{
		{0, 75}, {39, 75}, {40, 45}, {60, 45}, {61, 84},
		{80, 84}, {81, 220}, {95, 220}, {96, 203}, {150, 203},
	} {
		if got := percentageStyle(tt.p).String(); got != color.C256(tt.code).String() {
			t.Errorf("%d%% color = %q, want %q", tt.p, got, color.C256(tt.code).String())
		}
	}
	color.ForceSetColorLevel(color.Level16)
	if percentageStyle(95).String() != styleWarn.String() || percentageStyle(96).String() != styleBad.String() {
		t.Error("basic terminal must retain yellow/red problem colors")
	}
}

func TestPercentageCellRendering(t *testing.T) {
	c := percentageCell("81%: 13.5G/16.7G", 81, true)
	if len(c.parts) != 2 || c.parts[0].text != "81%" || c.parts[1].text != ": 13.5G/16.7G" || c.parts[1].style.String() != stylePlain.String() {
		t.Fatalf("percentage and quantities should have separate styles: %+v", c)
	}
	var buf bytes.Buffer
	printTable(&buf, "", []string{"MEM REQ", "NEXT"}, [][]cell{{c, plain("x")}})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if !strings.Contains(lines[1], c.text) || strings.Index(lines[0], "NEXT") != strings.Index(lines[1], "x") {
		t.Fatalf("percentage text or alignment changed: %q", buf.String())
	}
	paired := pairCell(c, percentageCell("96%: 16G", 96, true))
	if paired.text != "81%: 13.5G/16.7G/96%: 16G" || len(paired.parts) != 5 || paired.parts[0].style.String() != c.parts[0].style.String() || paired.parts[3].style.String() != percentageStyle(96).String() {
		t.Fatalf("paired percentages lost their styles: %+v", paired)
	}
	buf.Reset()
	printTable(&buf, "", []string{"MEM REQ/LIM", "NEXT"}, [][]cell{{paired, plain("x")}})
	lines = strings.Split(strings.TrimSpace(buf.String()), "\n")
	if !strings.Contains(lines[1], paired.text) || strings.Index(lines[0], "NEXT") != strings.Index(lines[1], "x") {
		t.Fatal("paired cell text or alignment changed")
	}
	unknown := percentageCell("512M", 0, false)
	if unknown.style.String() != stylePlain.String() || len(unknown.parts) != 0 {
		t.Error("unknown percentage should not get a scale color")
	}
}

func TestPrintTableMixedStatusColors(t *testing.T) {
	status := statusCell(&nodeInfo{issues: []issue{
		{lvlCrit, "not ready", "NotReady"},
		{lvlWarn, "cordoned", "SchedulingDisabled"},
	}})
	var buf bytes.Buffer
	printTable(&buf, "", []string{"STATUS", "NEXT"}, [][]cell{{status, plain("x")}})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if !strings.Contains(lines[1], "NotReady,SchedulingDisabled") || strings.Index(lines[0], "NEXT") != strings.Index(lines[1], "x") {
		t.Fatalf("plain status text or alignment incorrect: %q", buf.String())
	}
	previous := color.Enable
	color.Enable = true
	defer func() { color.Enable = previous }()
	buf.Reset()
	printTable(&buf, "", []string{"STATUS"}, [][]cell{{status}})
	want := styleBad.Sprint("NotReady") + "," + styleWarn.Sprint("SchedulingDisabled")
	if !strings.Contains(buf.String(), want) {
		t.Errorf("status colors = %q, want %q", buf.String(), want)
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

func TestCPUMemDivider(t *testing.T) {
	headers := []string{"NAME", "CPU REQ", "CPU USE", "MEM REQ", "MEM USE", "PODS"}
	rows := [][]cell{{
		plain("node-a"), plain("39%: 6.2/15.9"), plain("5%"), plain("11%: 13.6G/120.3G"), plain("11%"), plain("54/110"),
	}}
	sum := []cell{plain("Summary"), plain("41%"), plain("4%"), plain("16%"), plain("13%"), plain("114/220")}
	var buf bytes.Buffer
	printTable(&buf, "", headers, rows, sum)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %d\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "CPU USE │ MEM REQ") || !strings.Contains(lines[0], "MEM USE │ PODS") {
		t.Fatalf("header = %q", lines[0])
	}
	barsAt := func(s string) []int {
		var out []int
		for i := 0; i < len(s); {
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == '│' {
				out = append(out, utf8.RuneCountInString(s[:i]))
			}
			i += size
		}
		return out
	}
	wantBars := barsAt(lines[0])
	if len(wantBars) != 2 {
		t.Fatalf("header bars = %v: %q", wantBars, lines[0])
	}
	for _, line := range []string{lines[1], lines[3]} {
		if got := barsAt(line); !slices.Equal(got, wantBars) {
			t.Errorf("bars = %v, want %v: %q", got, wantBars, line)
		}
	}
	crossAt := func(s string) []int {
		var out []int
		for i := 0; i < len(s); {
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == '┼' {
				out = append(out, utf8.RuneCountInString(s[:i]))
			}
			i += size
		}
		return out
	}
	if got := crossAt(lines[2]); !slices.Equal(got, wantBars) || strings.Count(lines[2], "─") == 0 {
		t.Errorf("summary rule = %q", lines[2])
	}
	if strings.Index(lines[0], "MEM REQ") != strings.Index(lines[1], "11%: 13.6G/120.3G") {
		t.Errorf("memory column drifted:\n%s", buf.String())
	}
	if !slices.Equal(tableColumnDividers([]string{"CPU REQ", "MEM REQ"}), []int{0}) {
		t.Error("divider should follow CPU when usage is hidden")
	}
	if len(tableColumnDividers([]string{"RESOURCE", "CAPACITY"})) != 0 {
		t.Error("detail table should not grow resource dividers")
	}

	previous := color.Enable
	color.Enable = true
	previousLevel := color.ForceSetColorLevel(color.Level16)
	defer func() {
		color.Enable = previous
		color.ForceSetColorLevel(previousLevel)
	}()
	buf.Reset()
	printTable(&buf, "", headers, rows)
	if !strings.Contains(buf.String(), stylePlain.Sprint("│")) || strings.Contains(buf.String(), styleTitle.Sprint("│")) {
		t.Errorf("divider should be white:\n%s", buf.String())
	}
}

func TestSummaryRow(t *testing.T) {
	infos := buildInfos(testSnapshot())
	opts := tableOpts{showGPU: true}
	sumRow := buildSummaryRow(infos, opts)
	if sumRow[0].text != "Summary" {
		t.Fatalf("first cell = %q, want Summary", sumRow[0].text)
	}

	headers, rows := buildTable(infos, opts, time.Now())
	var buf bytes.Buffer
	printTable(&buf, "", headers, rows, sumRow)
	out := buf.String()

	if !strings.Contains(out, "─") {
		t.Errorf("output missing solid divider line: %s", out)
	}
	for _, want := range []string{"Summary", "29%: 3.5/12", "5%: 4G/80G", "nvidia", "NVIDIA-A10G", "3/220"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
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

func TestDefaultSortByNodeGroup(t *testing.T) {
	infos := []*nodeInfo{
		{node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}, cloud: cloudInfo{pool: "beta"}},
		{node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-z"}}, cloud: cloudInfo{pool: "alpha"}},
		{node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}}, cloud: cloudInfo{pool: "alpha"}},
		{node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-c"}}},
	}
	sortInfos(infos, defaultSort)
	want := []string{"node-c", "node-b", "node-z", "node-a"}
	for i, ni := range infos {
		if ni.node.Name != want[i] {
			t.Errorf("sorted node %d = %s, want %s", i, ni.node.Name, want[i])
		}
	}
	sortInfos(infos, "name")
	want = []string{"node-a", "node-b", "node-c", "node-z"}
	for i, ni := range infos {
		if ni.node.Name != want[i] {
			t.Errorf("explicit name sort %d = %s, want %s", i, ni.node.Name, want[i])
		}
	}
}

func TestNodeGroupColorsAcrossSortAndFilter(t *testing.T) {
	node := func(name, group string) *nodeInfo {
		n := testNode(name, nil, map[string]string{"nodegroup": group})
		return buildNodeInfo(&n, nil, nil, "")
	}
	infos := []*nodeInfo{node("node-z", "alpha"), node("node-b", "beta"), node("node-a", "alpha"), node("node-c", "")}
	styles := map[string]string{}
	for _, by := range []string{defaultSort, "name"} {
		sortInfos(infos, by)
		headers, rows := buildTable(infos, tableOpts{wide: true}, time.Now())
		for i, row := range rows {
			group := infos[i].cloud.pool
			c := row[len(headers)-1]
			if previous, ok := styles[group]; ok && previous != c.style.String() {
				t.Errorf("group %q changed color", group)
			}
			styles[group] = c.style.String()
			if group != "" && row[0].style.String() != c.style.String() {
				t.Error("node name should match its group color")
			}
			if group == "" && (c.text != "-" || c.style.String() != styleMeta.String()) {
				t.Error("unknown group should be a dim dash")
			}
		}
	}
	if styles["alpha"] == styles["beta"] {
		t.Error("example groups should have different palette colors")
	}
	headers, rows := buildTable(infos[:1], tableOpts{wide: true}, time.Now())
	if rows[0][len(headers)-1].style.String() != styles[infos[0].cloud.pool] {
		t.Error("filtering changed the group color")
	}
	var buf bytes.Buffer
	printTable(&buf, "", headers, rows)
	if strings.Contains(buf.String(), "\x1b[") || !strings.Contains(buf.String(), infos[0].cloud.pool) {
		t.Error("color-disabled output should retain plain group text")
	}
}

func TestExpandedGroupColors(t *testing.T) {
	previous := color.ForceSetColorLevel(color.Level256)
	defer color.ForceSetColorLevel(previous)
	groups := []string{"clickhouse-m7a-2xl-2a", "gpu-g6f-xlarge", "main-m7a-2xlarge", "nat-gateway-nodes", "r7a-4xl-2a"}
	seen := map[string]string{}
	for _, group := range groups {
		n := testNode("node-1", nil, map[string]string{"nodegroup": group})
		ni := buildNodeInfo(&n, nil, nil, "")
		headers, rows := buildTable([]*nodeInfo{ni}, tableOpts{}, time.Now())
		nameCell, groupCell := rows[0][0], rows[0][len(headers)-1]
		code := groupCell.style.String()
		if !strings.HasPrefix(code, "38;5;") || nameCell.style.String() != code {
			t.Errorf("node and group should share a 256-color style: %q / %q", nameCell.style.String(), code)
		}
		if other, ok := seen[code]; ok {
			t.Errorf("example groups %q and %q share a color", other, group)
		}
		seen[code] = group
		var buf bytes.Buffer
		printTable(&buf, "", headers, rows)
		if strings.Contains(buf.String(), "\x1b[") {
			t.Error("disabled colors should suppress 256-color escapes")
		}
	}
	color.ForceSetColorLevel(color.Level16)
	if code := nodeGroupCell(groups[0]).style.String(); strings.Contains(code, "38;5;") {
		t.Error("16-color terminal should use the basic palette")
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
		{[]string{"--sort", ""}, []string{"age", "cpu", "gpu", "mem", "name", "nodegroup", "pods"}},
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

func filesystem(capacity, used, available int64) *filesystemUsage {
	return &filesystemUsage{Capacity: capacity, Used: &used, Available: &available}
}

func TestEphemeralStorageHealth(t *testing.T) {
	for _, tt := range []struct {
		name string
		fs   *filesystemUsage
		low  bool
	}{
		{"below", filesystem(10000, 7499, 2501), false},
		{"exactly 75", filesystem(10000, 7500, 2500), false},
		{"one byte above", filesystem(10000, 7501, 2499), true},
		{"full", filesystem(100, 100, 0), true},
		{"empty", filesystem(100, 0, 100), false},
		{"reserved blocks", filesystem(100, 70, 20), true},
		{"large capacity", filesystem(math.MaxInt64, math.MaxInt64, 0), true},
		{"small capacity", filesystem(3, 3, 0), true},
		{"missing", nil, false},
		{"zero capacity", filesystem(0, 0, 0), false},
		{"negative", filesystem(100, -1, 20), false},
		{"available exceeds capacity", filesystem(100, 0, 101), false},
		{"used exceeds capacity", filesystem(100, 101, 0), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			snap := &snapshot{nodes: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{cond(corev1.NodeReady, corev1.ConditionTrue)}, Allocatable: rl("ephemeral-storage", "1")}}}, storage: map[string]*filesystemUsage{"node": tt.fs}}
			ni := buildInfos(snap)[0]
			want := "Healthy"
			if tt.low {
				want = "ephemeral-storage-low"
			}
			if got := statusCell(ni); got.text != want {
				t.Fatalf("status = %q, want %q", got.text, want)
			}
			if tt.low && (len(ni.issues) != 1 || ni.issues[0].level != lvlWarn) {
				t.Fatal("storage warning must participate in unhealthy filtering")
			}
			for _, wide := range []bool{false, true} {
				opts := tableOpts{wide: wide}
				headers, rows := buildTable([]*nodeInfo{ni}, opts, time.Now())
				idx := slices.Index(headers, "EPHEMERAL USED/AVAIL")
				if (idx >= 0) != wide {
					t.Fatal("storage columns must appear only in wide output")
				}
				if len(rows[0]) != len(headers) || len(buildSummaryRow([]*nodeInfo{ni}, opts)) != len(headers) {
					t.Fatal("column alignment")
				}
				if wide && !tt.fs.valid() && rows[0][idx].text != "-/-" {
					t.Fatal("unknown statistics must show dashes")
				}
			}
		})
	}
	// DiskPressure is independent of byte usage (e.g. inode exhaustion).
	snap := &snapshot{nodes: []corev1.Node{{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{cond(corev1.NodeReady, corev1.ConditionTrue), cond(corev1.NodeDiskPressure, corev1.ConditionTrue)}}}}}
	if got := statusCell(buildInfos(snap)[0]).text; got != "DiskPressure" {
		t.Fatalf("status = %q", got)
	}
}

func TestParseNodeStorage(t *testing.T) {
	for _, raw := range []string{`{`, `{}`, `{"node":{"fs":{}}}`, `{"node":{"fs":{"capacityBytes":100,"usedBytes":0}}}`, `{"node":{"fs":{"capacityBytes":100,"usedBytes":null,"availableBytes":100}}}`, `{"node":{"fs":{"capacityBytes":100,"usedBytes":-1,"availableBytes":100}}}`, `{"node":{"fs":{"capacityBytes":9223372036854775808,"usedBytes":0,"availableBytes":0}}}`} {
		if _, err := parseNodeStorage([]byte(raw)); err == nil {
			t.Errorf("accepted invalid stats: %s", raw)
		}
	}
	fs, err := parseNodeStorage([]byte(`{"node":{"fs":{"capacityBytes":100,"usedBytes":0,"availableBytes":100}},"runtime":{"imageFs":{"usedBytes":999}}}`))
	if err != nil || *fs.Used != 0 || fs.low() {
		t.Fatalf("zero usage should be valid: %+v, %v", fs, err)
	}
}

type storageTransport func(*http.Request) (*http.Response, error)

func (f storageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetchNodeStorage(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nodes/healthy/proxy/stats/summary":
			_, _ = fmt.Fprint(w, `{"node":{"fs":{"capacityBytes":100,"usedBytes":80,"availableBytes":20}}}`)
		case "/api/v1/nodes/forbidden/proxy/stats/summary":
			http.Error(w, "forbidden", http.StatusForbidden)
		case "/api/v1/nodes/slow/proxy/stats/summary":
			<-r.Context().Done()
		case "/api/v1/nodes/invalid/proxy/stats/summary":
			_, _ = fmt.Fprint(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	})
	transport := storageTransport(func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		return recorder.Result(), nil
	})
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: "https://cluster.example", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	nodes := []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "healthy"}}, {ObjectMeta: metav1.ObjectMeta{Name: "forbidden"}}, {ObjectMeta: metav1.ObjectMeta{Name: "invalid"}}}
	stats, errs := fetchNodeStorage(context.Background(), cs, nodes)
	if len(stats) != 1 || !stats["healthy"].low() || len(errs) != 2 {
		t.Fatalf("stats=%v errors=%v", stats, errs)
	}
	deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer deadlineCancel()
	stats, errs = fetchNodeStorage(deadlineCtx, cs, []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "slow"}}})
	if len(stats) != 0 || !errors.Is(errs["slow"], context.DeadlineExceeded) {
		t.Fatalf("deadline errors = %v", errs)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stats, errs = fetchNodeStorage(ctx, cs, nodes)
	if len(stats) != 0 || len(errs) != len(nodes) {
		t.Fatalf("canceled fetch: stats=%v errors=%v", stats, errs)
	}
}

func TestStorageSummaryAndDetail(t *testing.T) {
	snap := testSnapshot()
	snap.storage = map[string]*filesystemUsage{snap.nodes[0].Name: filesystem(100<<30, 80<<30, 20<<30), snap.nodes[1].Name: filesystem(100<<30, 70<<30, 30<<30)}
	infos := buildInfos(snap)
	opts := tableOpts{wide: true, hasMetrics: true, showGPU: true}
	headers, rows := buildTable(infos, opts, time.Now())
	idx := slices.Index(headers, "EPHEMERAL USED/AVAIL")
	sum := buildSummaryRow(infos, opts)
	if rows[0][idx].text != "80G/20G" || sum[idx].text != "150G/50G" || len(sum) != len(headers) {
		t.Fatalf("rows=%v summary=%v", rows, sum)
	}
	infos[0].storage = filesystem(math.MaxInt64, 0, math.MaxInt64)
	infos[1].storage = filesystem(math.MaxInt64, 0, math.MaxInt64)
	sum = buildSummaryRow(infos, opts)
	if sum[idx].text != "-/-" {
		t.Fatal("overflowing totals must be unknown")
	}
	infos[1].storage = nil
	sum = buildSummaryRow(infos, opts)
	if sum[idx].text != "-/-" {
		t.Fatal("partial totals must be unknown")
	}
	snap.storageErrors = map[string]error{infos[1].node.Name: errors.New("forbidden")}
	var buf bytes.Buffer
	renderDetail(&buf, infos[1], snap, time.Now())
	if !strings.Contains(buf.String(), "Ephemeral:") || !strings.Contains(buf.String(), "unavailable: forbidden") {
		t.Fatal(buf.String())
	}
}

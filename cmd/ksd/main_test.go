package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gookit/color"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"
)

func init() { color.Enable = false }

func makeCert(t *testing.T, cn string, notBefore, notAfter time.Time, dns ...string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"Acme"}},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		DNSNames:     dns,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func testSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "prod", Labels: map[string]string{"app": "db"}},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"password": []byte("s3cr3t"),
			"port":     []byte("5432"),
			"empty":    []byte(""),
			"config":   []byte("a: 1\nb: 2\n"),
			"noeol":    []byte("line1\nline2"),
			"keystore": {0x00, 0xff, 0xfe, 0x01},
		},
	}
}

func TestIsText(t *testing.T) {
	cases := map[string]struct {
		in   []byte
		want bool
	}{
		"ascii":     {[]byte("hello"), true},
		"multiline": {[]byte("a\nb\tc"), true},
		"nul":       {[]byte{'a', 0, 'b'}, false},
		"invalid":   {[]byte{0xff, 0xfe}, false},
		"empty":     {nil, true},
	}
	for name, c := range cases {
		if got := isText(c.in); got != c.want {
			t.Errorf("%s: isText = %v, want %v", name, got, c.want)
		}
	}
}

func TestYamlScalar(t *testing.T) {
	cases := map[string]string{
		"hello":     "hello",
		"":          `""`,
		"123":       `"123"`,
		"true":      `"true"`,
		"a: b":      `'a: b'`,
		"with #":    `'with #'`,
		"password1": "password1",
	}
	for in, want := range cases {
		if got := yamlScalar(in); got != want {
			t.Errorf("yamlScalar(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("x", 500)
	if got := yamlScalar(long); got != long {
		t.Errorf("long scalar must not be folded, got %d bytes", len(got))
	}
}

func TestBlockScalarHeader(t *testing.T) {
	cases := map[string]string{
		"single":     "",
		"a\nb":       "|-",
		"a\nb\n":     "|",
		"a\nb\n\n":   "|+",
		" lead\nb\n": "",
		"a\r\nb":     "",
	}
	for in, want := range cases {
		if got := blockScalarHeader(in); got != want {
			t.Errorf("blockScalarHeader(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderPretty(t *testing.T) {
	var buf bytes.Buffer
	renderPrettyAt(&buf, testSecret(), false, 0, time.Now())
	out := buf.String()

	for _, want := range []string{
		"# prod/db · Opaque · 6 keys",
		"apiVersion: v1\n",
		"kind: Secret\n",
		"  name: db\n",
		"  namespace: prod\n",
		"type: Opaque\n",
		"  password: s3cr3t\n",
		`  port: "5432"` + "\n",
		`  empty: ""` + "\n",
		"  config: |\n    a: 1\n    b: 2\n",
		"  noeol: |-\n    line1\n    line2\n",
		"  keystore: <binary 4 bytes>\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
	if strings.Contains(out, "labels") {
		t.Errorf("labels must be hidden without --full:\n%s", out)
	}

	buf.Reset()
	renderPrettyAt(&buf, testSecret(), true, 0, time.Now())
	if !strings.Contains(buf.String(), "labels:\n    app: db") {
		t.Errorf("--full should include labels:\n%s", buf.String())
	}
}

func TestRenderPrettyOutputIsValidYAML(t *testing.T) {
	var buf bytes.Buffer
	renderPrettyAt(&buf, testSecret(), true, 0, time.Now())
	var parsed map[string]any
	if err := yaml.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("pretty output is not valid YAML: %v\n%s", err, buf.String())
	}
	data := parsed["data"].(map[string]any)
	if data["config"] != "a: 1\nb: 2\n" || data["noeol"] != "line1\nline2" || data["port"] != "5432" {
		t.Errorf("round trip mismatch: %#v", data)
	}
}

func TestCertParsingAndExpiry(t *testing.T) {
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	pemData := makeCert(t, "api.example.com", now.AddDate(0, -1, 0), now.AddDate(0, 0, 82), "api.example.com", "www.example.com")

	certs := parseCerts(pemData)
	if len(certs) != 1 {
		t.Fatalf("got %d certs, want 1", len(certs))
	}
	if !strings.Contains(certs[0].Subject, "CN=api.example.com") {
		t.Errorf("subject = %q", certs[0].Subject)
	}
	if !reflect.DeepEqual(certs[0].SANs, []string{"api.example.com", "www.example.com"}) {
		t.Errorf("sans = %v", certs[0].SANs)
	}

	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tls", Namespace: "prod"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": pemData},
	}
	var buf bytes.Buffer
	renderPrettyAt(&buf, s, false, 0, now)
	out := buf.String()
	for _, want := range []string{
		"# certificate", "CN=api.example.com", "issuer", "valid",
		"(expires in 82 days)", "sans", "www.example.com",
		"  tls.crt: |\n    -----BEGIN CERTIFICATE-----",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestExpiryStatus(t *testing.T) {
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		notAfter time.Time
		want     string
	}{
		{now.AddDate(0, 0, -3), "EXPIRED 3 days ago"},
		{now.AddDate(0, 0, 10), "expires in 10 days"},
		{now.AddDate(3, 0, 0), "expires in 3.0 years"},
	}
	for _, c := range cases {
		if got, _ := expiry(c.notAfter, now); got != c.want {
			t.Errorf("expiry = %q, want %q", got, c.want)
		}
	}
}

func TestCertCommentTruncatesToWidth(t *testing.T) {
	now := time.Now()
	pemData := makeCert(t, strings.Repeat("long-name", 20), now.Add(-time.Hour), now.Add(time.Hour*24*90))
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "n"}, Data: map[string][]byte{"c": pemData}}
	var buf bytes.Buffer
	renderPrettyAt(&buf, s, false, 60, now)
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "# ") && len([]rune(line)) > 60 {
			t.Errorf("line exceeds width 60 (%d): %q", len([]rune(line)), line)
		}
	}
}

func TestMachineOutput(t *testing.T) {
	var buf bytes.Buffer
	if err := writeMachine(&buf, testSecret(), "json", false); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	data := got["data"].(map[string]any)
	if data["password"] != "s3cr3t" {
		t.Errorf("data = %v", data)
	}
	if _, ok := data["keystore"]; ok {
		t.Error("binary value must not be in data")
	}
	if got["binaryData"].(map[string]any)["keystore"] != "AP/+AQ==" {
		t.Errorf("binaryData = %v", got["binaryData"])
	}

	buf.Reset()
	if err := writeMachine(&buf, testSecret(), "yaml", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "password: s3cr3t") || strings.Contains(buf.String(), "labels") {
		t.Errorf("unexpected yaml:\n%s", buf.String())
	}
}

func TestSecretTableDropsColumnsToFit(t *testing.T) {
	secrets := []corev1.Secret{{
		ObjectMeta: metav1.ObjectMeta{Name: "my-secret", CreationTimestamp: metav1.NewTime(time.Now().Add(-48 * time.Hour))},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"a": nil},
	}}
	var wide, narrow bytes.Buffer
	printSecretTable(&wide, secrets, 0)
	printSecretTable(&narrow, secrets, 14)
	if !strings.Contains(wide.String(), "AGE") || !strings.Contains(wide.String(), "2d") {
		t.Errorf("wide table:\n%s", wide.String())
	}
	if strings.Contains(narrow.String(), "AGE") || !strings.Contains(narrow.String(), "NAME") {
		t.Errorf("narrow table should keep NAME and drop AGE:\n%s", narrow.String())
	}
}

// ---- completion ----

func fakeFactory(objs ...corev1.Secret) func(string) (kubernetes.Interface, string, error) {
	cs := fake.NewClientset()
	for i := range objs {
		_, _ = cs.CoreV1().Secrets(objs[i].Namespace).Create(context.Background(), &objs[i], metav1.CreateOptions{})
	}
	for _, ns := range []string{"default", "kube-system", "prod"} {
		_, _ = cs.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	}
	return func(string) (kubernetes.Interface, string, error) { return cs, "default", nil }
}

func sec(ns, name string, keys ...string) corev1.Secret {
	d := map[string][]byte{}
	for _, k := range keys {
		d[k] = []byte("v")
	}
	return corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: d}
}

func TestComplete(t *testing.T) {
	f := fakeFactory(
		sec("kube-system", "bootstrap-token", "token-id"),
		sec("kube-system", "bootstrap-signer"),
		sec("kube-system", "other"),
		sec("default", "default-secret"),
		sec("prod", "db", "password", "username"),
	)
	ctx := context.Background()
	cases := []struct {
		name  string
		words []string
		want  []string
	}{
		{"secrets in -n namespace", []string{"-n", "kube-system", ""}, []string{"bootstrap-signer", "bootstrap-token", "other"}},
		{"secrets prefix", []string{"-n", "kube-system", "boot"}, []string{"bootstrap-signer", "bootstrap-token"}},
		{"long flag", []string{"--namespace", "prod", ""}, []string{"db"}},
		{"equals form", []string{"--namespace=prod", ""}, []string{"db"}},
		{"bash split equals", []string{"--namespace", "=", "prod", ""}, []string{"db"}},
		{"default namespace", []string{""}, []string{"default-secret"}},
		{"namespaces", []string{"-n", ""}, []string{"default", "kube-system", "prod"}},
		{"namespaces prefix", []string{"-n", "ku"}, []string{"kube-system"}},
		{"namespace equals value", []string{"--namespace=ku"}, []string{"--namespace=kube-system"}},
		{"keys", []string{"db", "-n", "prod", "-k", ""}, []string{"password", "username"}},
		{"keys prefix", []string{"-n", "prod", "db", "--key", "pa"}, []string{"password"}},
		{"keys without secret", []string{"-k", ""}, nil},
		{"flags", []string{"--ke"}, []string{"--key"}},
		{"flags after secret", []string{"db", "--no"}, []string{"--no-color"}},
		{"nothing after secret", []string{"db", ""}, nil},
		{"kubeconfig files", []string{"--kubeconfig", ""}, []string{completeFiles}},
		{"output formats", []string{"-o", ""}, []string{"yaml", "json"}},
		{"completions shells", []string{"--completions", "b"}, []string{"bash"}},
	}
	for _, c := range cases {
		if got := complete(ctx, c.words, f); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: complete(%q) = %v, want %v", c.name, c.words, got, c.want)
		}
	}
}

func TestCompleteClientErrorIsSilent(t *testing.T) {
	bad := func(string) (kubernetes.Interface, string, error) { return nil, "", errors.New("no cluster") }
	if got := complete(context.Background(), []string{""}, bad); got != nil {
		t.Errorf("want no candidates, got %v", got)
	}
}

func TestParseInterleaved(t *testing.T) {
	fs := flag.NewFlagSet("ksd", flag.ContinueOnError)
	ns := stringFlag(fs, "n", "namespace", "")
	key := stringFlag(fs, "k", "key", "")
	pos, err := parseInterleaved(fs, []string{"mysecret", "-n", "prod", "-k", "password"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pos, []string{"mysecret"}) || *ns != "prod" || *key != "password" {
		t.Errorf("pos=%v ns=%q key=%q", pos, *ns, *key)
	}
}

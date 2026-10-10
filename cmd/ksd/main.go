package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gookit/color"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
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

// Palette used for the pretty (terminal) output.
var (
	styleTitle  = color.Style{color.Bold, color.FgWhite}
	styleSect   = color.Style{color.Bold, color.FgCyan}
	styleTopKey = color.Style{color.Bold, color.FgCyan}
	styleKey    = color.FgCyan
	styleString = color.FgGreen
	styleNumber = color.FgYellow
	styleBinary = color.FgMagenta
	styleMeta   = color.FgGray
	styleName   = color.Style{color.Bold, color.FgWhite}
	styleOK     = color.FgGreen
	styleWarn   = color.FgYellow
	styleBad    = color.Style{color.Bold, color.FgRed}
)

func printUsage() {
	fmt.Println(styleTitle.Sprint("Kubernetes Secret Decoder (ksd)"))
	fmt.Println(styleMeta.Sprintf("  version %s", version))
	fmt.Println()

	fmt.Println(styleSect.Sprint("USAGE"))
	fmt.Println("  ksd [secret] [flags]")
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

	row("-n, --namespace", "namespace", "Namespace of the secret.", "current context")
	row("-k, --key", "key", "Print only the raw decoded value of this key.", "")
	row("-o, --output", "format", "Output format: yaml or json (plain, no color).", "")
	row("-i, --interactive", "", "Pick the secret with fzf.", "")
	row("--full", "", "Include labels, annotations, uid and timestamps.", "")
	row("--no-color", "", "Disable colored output (NO_COLOR is also honored).", "")
	row("--kubeconfig", "path", "Path to kubeconfig file.", "$KUBECONFIG or ~/.kube/config")
	row("--completions", "shell", "Print shell completion script (bash or zsh).", "")
	row("-v, --version", "", "Print version and exit.", "")
	row("-h, --help", "", "Show this help message.", "")
	fmt.Println()
	fmt.Println(styleSect.Sprint("EXAMPLES"))
	fmt.Println("  ksd -n kube-system <TAB>      " + styleMeta.Sprint("# tab-complete secret names"))
	fmt.Println("  ksd my-secret -k password     " + styleMeta.Sprint("# raw value of one key"))
	fmt.Println("  ksd -n prod -i                " + styleMeta.Sprint("# choose a secret with fzf"))
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

	fs := flag.NewFlagSet("ksd", flag.ContinueOnError)
	namespace := stringFlag(fs, "n", "namespace", "")
	keyFlag := stringFlag(fs, "k", "key", "")
	output := stringFlag(fs, "o", "output", "")
	interactive := boolFlag(fs, "i", "interactive")
	full := boolFlag(fs, "", "full")
	noColor := boolFlag(fs, "", "no-color")
	kubeconfig := fs.String("kubeconfig", "", "")
	completionsFlag := fs.String("completions", "", "")
	versionFlag := boolFlag(fs, "v", "version")
	fs.Usage = printUsage

	args, err := parseInterleaved(fs, os.Args[1:])
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
		fmt.Println("ksd", version)
		return
	}
	if len(args) > 1 {
		die("arguments", fmt.Errorf("expected at most one secret name, got %d", len(args)))
	}

	switch *output {
	case "", "yaml", "json":
	default:
		die("output", fmt.Errorf("unknown format %q: use yaml or json", *output))
	}

	stdoutTTY := term.IsTerminal(int(os.Stdout.Fd()))
	colorOn := stdoutTTY && !*noColor && os.Getenv("NO_COLOR") == "" && *output == ""
	color.Enable = colorOn
	width := 0
	if stdoutTTY {
		if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
			width = w
		}
	}

	clientset, defaultNS, err := newClient(*kubeconfig)
	if err != nil {
		die("loading kubeconfig", err)
	}
	ns := *namespace
	if ns == "" {
		ns = defaultNS
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name := ""
	if len(args) == 1 {
		name = args[0]
	}

	if name == "" {
		list, err := clientset.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			die("listing secrets", err)
		}
		if len(list.Items) == 0 {
			fmt.Fprintf(os.Stderr, "No secrets found in namespace %s.\n", ns)
			return
		}
		if !*interactive {
			printSecretTable(os.Stdout, list.Items, width)
			return
		}
		names := make([]string, 0, len(list.Items))
		for _, s := range list.Items {
			names = append(names, s.Name)
		}
		picked, err := pickWithFzf(names, ns)
		if err != nil {
			die("fzf", err)
		}
		if picked == "" {
			os.Exit(130)
		}
		name = picked
	}

	secret, err := clientset.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		die(fmt.Sprintf("getting secret %s/%s", ns, name), err)
	}

	if *keyFlag != "" {
		val, ok := secret.Data[*keyFlag]
		if !ok {
			die("key", fmt.Errorf("%q not found in %s/%s (available: %s)", *keyFlag, ns, name, strings.Join(sortedKeys(secret.Data), ", ")))
		}
		_, _ = os.Stdout.Write(val)
		if stdoutTTY {
			fmt.Println()
		}
		return
	}

	switch *output {
	case "yaml", "json":
		if err := writeMachine(os.Stdout, secret, *output, *full); err != nil {
			die("encoding output", err)
		}
	default:
		renderPretty(os.Stdout, secret, *full, width)
	}
}

// newClient builds a clientset and returns the namespace of the active
// kubeconfig context ("default" when unset).
func newClient(kubeconfig string) (kubernetes.Interface, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, "", err
	}
	ns, _, err := cc.Namespace()
	if err != nil {
		return nil, "", err
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, "", err
	}
	return clientset, ns, nil
}

func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// pickWithFzf lets the user choose one name with fzf. It returns "" when the
// selection was cancelled.
func pickWithFzf(names []string, ns string) (string, error) {
	path, err := exec.LookPath("fzf")
	if err != nil {
		return "", errors.New("fzf not found in PATH (install it from https://github.com/junegunn/fzf)")
	}
	cmd := exec.Command(path, "--height=40%", "--reverse", "--prompt=secret> ", "--header=namespace: "+ns)
	cmd.Stdin = strings.NewReader(strings.Join(names, "\n"))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", nil // no match (1) or cancelled (130)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ---------------------------------------------------------------------------
// Secret listing
// ---------------------------------------------------------------------------

// printSecretTable renders the secrets of a namespace, dropping columns from
// the right until the table fits the terminal width (0 = unlimited).
func printSecretTable(w io.Writer, secrets []corev1.Secret, width int) {
	const gap = 2
	header := []string{"NAME", "TYPE", "KEYS", "AGE"}
	secrets = append([]corev1.Secret(nil), secrets...)
	sort.Slice(secrets, func(i, j int) bool { return secrets[i].Name < secrets[j].Name })
	rows := make([][]string, 0, len(secrets))
	for _, s := range secrets {
		age := ""
		if !s.CreationTimestamp.IsZero() {
			age = duration.HumanDuration(time.Since(s.CreationTimestamp.Time))
		}
		rows = append(rows, []string{s.Name, string(s.Type), strconv.Itoa(len(s.Data)), age})
	}

	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], len(c))
		}
	}

	cols := len(header)
	total := func(n int) int {
		t := 0
		for i := 0; i < n; i++ {
			t += widths[i] + gap
		}
		return t - gap
	}
	for width > 0 && cols > 1 && total(cols) > width {
		cols--
	}

	cell := func(i int, text string, st color.Style) string {
		padded := text
		if i < cols-1 {
			padded = fmt.Sprintf("%-*s", widths[i]+gap, text)
		}
		return st.Sprint(padded)
	}
	hdrStyle := color.Style{color.Bold, color.FgGray}
	var b strings.Builder
	for i := 0; i < cols; i++ {
		b.WriteString(cell(i, header[i], hdrStyle))
	}
	fmt.Fprintln(w, b.String())

	for _, r := range rows {
		b.Reset()
		for i := 0; i < cols; i++ {
			st := color.Style{color.FgWhite}
			switch i {
			case 0:
				st = color.Style{color.Bold, color.FgCyan}
			case 1, 3:
				st = color.Style{color.FgGray}
			}
			b.WriteString(cell(i, r[i], st))
		}
		fmt.Fprintln(w, b.String())
	}
}

// ---------------------------------------------------------------------------
// Value classification
// ---------------------------------------------------------------------------

// isText reports whether b is safe to show as text in a terminal.
func isText(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if r == 0 || (r < 0x20 && r != '\n' && r != '\t' && r != '\r') || r == 0x7f {
			return false
		}
	}
	return true
}

func isNumberish(s string) bool {
	switch s {
	case "true", "false", "null":
		return true
	}
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

// yamlScalar renders s as a single-line YAML scalar, quoting when required.
func yamlScalar(s string) string {
	b, err := yaml.Marshal(s)
	out := strings.TrimSuffix(string(b), "\n")
	if err != nil || strings.Contains(out, "\n") {
		q, _ := json.Marshal(s) // JSON strings are valid YAML double-quoted scalars
		return string(q)
	}
	return out
}

// blockScalarHeader returns the YAML block scalar header for s, or "" when s
// must be rendered as a quoted scalar instead (see renderValue).
func blockScalarHeader(s string) string {
	if !strings.Contains(s, "\n") || strings.Contains(s, "\r") {
		return ""
	}
	if s[0] == ' ' || s[0] == '\n' || s[0] == '\t' {
		return "" // would need an explicit indentation indicator
	}
	trailing := len(s) - len(strings.TrimRight(s, "\n"))
	switch {
	case trailing == 0:
		return "|-"
	case trailing == 1:
		return "|"
	default:
		return "|+"
	}
}

// ---------------------------------------------------------------------------
// Certificates
// ---------------------------------------------------------------------------

type certInfo struct {
	Subject   string
	Issuer    string
	NotBefore time.Time
	NotAfter  time.Time
	SANs      []string
}

// parseCerts extracts every CERTIFICATE block from a PEM bundle.
func parseCerts(data []byte) []certInfo {
	var certs []certInfo
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return certs
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		sans := append([]string{}, c.DNSNames...)
		for _, ip := range c.IPAddresses {
			sans = append(sans, ip.String())
		}
		certs = append(certs, certInfo{
			Subject:   c.Subject.String(),
			Issuer:    c.Issuer.String(),
			NotBefore: c.NotBefore,
			NotAfter:  c.NotAfter,
			SANs:      sans,
		})
	}
}

// expiry returns a human readable validity status and its style.
func expiry(notAfter, now time.Time) (string, color.Style) {
	d := notAfter.Sub(now)
	switch {
	case d < 0:
		return "EXPIRED " + shortDays(-d) + " ago", styleBad
	case d < 30*24*time.Hour:
		return "expires in " + shortDays(d), color.Style{styleWarn}
	default:
		return "expires in " + shortDays(d), color.Style{styleOK}
	}
}

func shortDays(d time.Duration) string {
	days := int(d.Hours() / 24)
	switch {
	case days >= 730:
		return fmt.Sprintf("%.1f years", float64(days)/365)
	case days >= 1:
		return fmt.Sprintf("%d days", days)
	default:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
}

// ---------------------------------------------------------------------------
// Pretty rendering
// ---------------------------------------------------------------------------

// fit truncates s to n runes with an ellipsis (n <= 0 means unlimited).
func fit(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func renderPretty(w io.Writer, s *corev1.Secret, full bool, width int) {
	renderPrettyAt(w, s, full, width, time.Now())
}

func renderPrettyAt(w io.Writer, s *corev1.Secret, full bool, width int, now time.Time) {
	p := func(format string, a ...any) { fmt.Fprintf(w, format+"\n", a...) }

	// Header (a YAML comment, so the output stays valid YAML).
	keysWord := "keys"
	if len(s.Data) == 1 {
		keysWord = "key"
	}
	p("%s", styleMeta.Sprint("# ")+styleMeta.Sprint(s.Namespace+"/")+styleName.Sprint(s.Name)+
		styleMeta.Sprintf(" · %s · %d %s", orDash(string(s.Type)), len(s.Data), keysWord))

	p("%s %s", styleTopKey.Sprint("apiVersion:"), styleString.Sprint("v1"))

	// data
	if len(s.Data) == 0 {
		p("%s %s", styleTopKey.Sprint("data:"), styleMeta.Sprint("{}"))
	} else {
		p("%s", styleTopKey.Sprint("data:"))
		for _, k := range sortedKeys(s.Data) {
			renderEntry(w, k, s.Data[k], width, now)
		}
	}

	p("%s %s", styleTopKey.Sprint("kind:"), styleString.Sprint("Secret"))

	// metadata
	p("%s", styleTopKey.Sprint("metadata:"))
	if full {
		writeStringMap(w, "annotations", s.Annotations)
		if !s.CreationTimestamp.IsZero() {
			p("  %s %s", styleKey.Sprint("creationTimestamp:"), styleString.Sprint(yamlScalar(s.CreationTimestamp.UTC().Format(time.RFC3339))))
		}
		writeStringMap(w, "labels", s.Labels)
	}
	p("  %s %s", styleKey.Sprint("name:"), styleString.Sprint(yamlScalar(s.Name)))
	p("  %s %s", styleKey.Sprint("namespace:"), styleString.Sprint(yamlScalar(s.Namespace)))
	if full {
		if s.ResourceVersion != "" {
			p("  %s %s", styleKey.Sprint("resourceVersion:"), styleString.Sprint(yamlScalar(s.ResourceVersion)))
		}
		if s.UID != "" {
			p("  %s %s", styleKey.Sprint("uid:"), styleString.Sprint(yamlScalar(string(s.UID))))
		}
	}

	if s.Type != "" {
		p("%s %s", styleTopKey.Sprint("type:"), styleString.Sprint(yamlScalar(string(s.Type))))
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func writeStringMap(w io.Writer, name string, m map[string]string) {
	if len(m) == 0 {
		return
	}
	fmt.Fprintf(w, "  %s\n", styleKey.Sprint(name+":"))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "    %s %s\n", styleKey.Sprint(yamlScalar(k)+":"), styleString.Sprint(yamlScalar(m[k])))
	}
}

// renderEntry prints one decoded secret key, preceded by certificate details
// (as YAML comments) when the value contains PEM certificates.
func renderEntry(w io.Writer, key string, raw []byte, width int, now time.Time) {
	const indent = "  "
	keyOut := styleKey.Sprint(yamlScalar(key) + ":")

	if !isText(raw) {
		fmt.Fprintf(w, "%s%s %s\n", indent, keyOut, styleBinary.Sprintf("<binary %d bytes>", len(raw)))
		return
	}
	val := string(raw)

	if strings.Contains(val, "-----BEGIN CERTIFICATE-----") {
		for _, c := range parseCerts(raw) {
			renderCertComment(w, indent, c, width, now)
		}
	}

	if hdr := blockScalarHeader(val); hdr != "" {
		fmt.Fprintf(w, "%s%s %s\n", indent, keyOut, styleMeta.Sprint(hdr))
		lines := strings.Split(val, "\n")
		if hdr != "|-" {
			lines = lines[:len(lines)-1] // drop the empty element after the final newline
		}
		for _, line := range lines {
			if line == "" {
				fmt.Fprintln(w)
				continue
			}
			fmt.Fprintf(w, "%s  %s\n", indent, styleString.Sprint(line))
		}
		return
	}

	scalar := yamlScalar(val)
	st := styleString
	if isNumberish(val) {
		st = styleNumber
	}
	fmt.Fprintf(w, "%s%s %s\n", indent, keyOut, st.Sprint(scalar))
}

func renderCertComment(w io.Writer, indent string, c certInfo, width int, now time.Time) {
	const labelW = 13
	status, st := expiry(c.NotAfter, now)

	// line prints "<indent># <label> <text>[ (status)]", truncating text to fit.
	line := func(label, text string, textStyle color.Style, suffix string, suffixStyle color.Style) {
		prefix := fmt.Sprintf("%s# %-*s", indent, labelW, label)
		if width > 0 {
			text = fit(text, max(width-len(prefix)-len(suffix), 1))
		}
		out := styleMeta.Sprint(prefix) + textStyle.Sprint(text)
		if suffix != "" {
			out += suffixStyle.Sprint(suffix)
		}
		fmt.Fprintln(w, out)
	}

	meta := color.Style{styleMeta}
	line("certificate", orDash(c.Subject), styleName, "", meta)
	line("  issuer", orDash(c.Issuer), meta, "", meta)
	valid := c.NotBefore.UTC().Format("2006-01-02") + " to " + c.NotAfter.UTC().Format("2006-01-02")
	if width > 0 && len(indent)+2+labelW+len(valid)+len(status)+3 > width {
		line("  valid", status, st, "", st) // too narrow for the date range: keep what matters
	} else {
		line("  valid", valid, meta, " ("+status+")", st)
	}
	if len(c.SANs) > 0 {
		sans := c.SANs
		extra := ""
		if len(sans) > 5 {
			extra = fmt.Sprintf(" +%d more", len(sans)-5)
			sans = sans[:5]
		}
		line("  sans", strings.Join(sans, ", ")+extra, meta, "", meta)
	}
}

// ---------------------------------------------------------------------------
// Machine-readable output
// ---------------------------------------------------------------------------

type outMeta struct {
	Annotations       map[string]string `json:"annotations,omitempty"`
	CreationTimestamp string            `json:"creationTimestamp,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	UID               string            `json:"uid,omitempty"`
}

type outSecret struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   outMeta           `json:"metadata"`
	Type       string            `json:"type,omitempty"`
	Data       map[string]string `json:"data,omitempty"`
	// BinaryData holds values that are not valid text; encoded as base64.
	BinaryData map[string][]byte `json:"binaryData,omitempty"`
}

func buildOutput(s *corev1.Secret, full bool) outSecret {
	o := outSecret{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata:   outMeta{Name: s.Name, Namespace: s.Namespace},
		Type:       string(s.Type),
	}
	if full {
		o.Metadata.Annotations = s.Annotations
		o.Metadata.Labels = s.Labels
		if !s.CreationTimestamp.IsZero() {
			o.Metadata.CreationTimestamp = s.CreationTimestamp.UTC().Format(time.RFC3339)
		}
		o.Metadata.ResourceVersion = s.ResourceVersion
		o.Metadata.UID = string(s.UID)
	}
	for k, v := range s.Data {
		if isText(v) {
			if o.Data == nil {
				o.Data = map[string]string{}
			}
			o.Data[k] = string(v)
			continue
		}
		if o.BinaryData == nil {
			o.BinaryData = map[string][]byte{}
		}
		o.BinaryData[k] = v
	}
	return o
}

func writeMachine(w io.Writer, s *corev1.Secret, format string, full bool) error {
	o := buildOutput(s, full)
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(o)
	}
	b, err := yaml.Marshal(o)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ---------------------------------------------------------------------------
// Shell completion
// ---------------------------------------------------------------------------

const completeFiles = ":files" // directive: ask the shell for file completion

var (
	valueFlags = map[string]bool{
		"-n": true, "--namespace": true,
		"-k": true, "--key": true,
		"-o": true, "--output": true,
		"--kubeconfig": true, "--completions": true,
	}
	allFlags = []string{
		"-n", "--namespace", "-k", "--key", "-o", "--output", "-i", "--interactive",
		"--full", "--no-color", "--kubeconfig", "--completions", "-v", "--version", "-h", "--help",
	}
)

type completeState struct {
	namespace  string
	kubeconfig string
	secret     string
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
		case st.secret == "":
			st.secret = w
		}
	}
	return st
}

func (st *completeState) apply(flagName, val string) {
	switch flagName {
	case "-n", "--namespace":
		st.namespace = val
	case "--kubeconfig":
		st.kubeconfig = val
	}
}

// complete returns completion candidates for the last word in words (the word
// under the cursor, possibly empty). newClient is only invoked when a cluster
// lookup is needed; any error yields no candidates.
func complete(ctx context.Context, words []string, newClient func(kubeconfig string) (kubernetes.Interface, string, error)) []string {
	if len(words) == 0 {
		words = []string{""}
	}
	cur := words[len(words)-1]
	if cur == "=" {
		cur = ""
	}
	st := scanWords(words[:len(words)-1])

	// "--namespace=<partial>" style: complete the value, keep the prefix.
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
		return filterPrefix([]string{"yaml", "json"}, cur)
	case "-n", "--namespace":
		cs, _, err := newClient(st.kubeconfig)
		if err != nil {
			return nil
		}
		list, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil
		}
		names := make([]string, 0, len(list.Items))
		for _, n := range list.Items {
			names = append(names, n.Name)
		}
		return filterPrefix(names, cur)
	case "-k", "--key":
		if st.secret == "" {
			return nil
		}
		cs, defNS, err := newClient(st.kubeconfig)
		if err != nil {
			return nil
		}
		ns := st.namespace
		if ns == "" {
			ns = defNS
		}
		s, err := cs.CoreV1().Secrets(ns).Get(ctx, st.secret, metav1.GetOptions{})
		if err != nil {
			return nil
		}
		return filterPrefix(sortedKeys(s.Data), cur)
	case "":
	default:
		return nil // free-form value (none today)
	}

	if strings.HasPrefix(cur, "-") {
		return filterPrefix(allFlags, cur)
	}
	if st.secret != "" {
		// Secret already chosen; offer flags only when explicitly asked for.
		return nil
	}

	cs, defNS, err := newClient(st.kubeconfig)
	if err != nil {
		return nil
	}
	ns := st.namespace
	if ns == "" {
		ns = defNS
	}
	list, err := cs.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(list.Items))
	for _, s := range list.Items {
		names = append(names, s.Name)
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

// runComplete implements the hidden `ksd __complete WORDS...` command used by
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
		fmt.Print(`_ksd_completions() {
    local cur="${COMP_WORDS[COMP_CWORD]}"
    local out
    out="$("${COMP_WORDS[0]}" __complete "${COMP_WORDS[@]:1:COMP_CWORD}" 2>/dev/null)"
    if [[ "${out%%$'\n'*}" == ":files" ]]; then
        COMPREPLY=($(compgen -f -- "${cur}"))
        return
    fi
    COMPREPLY=($(compgen -W "${out}" -- "${cur}"))
}
complete -F _ksd_completions ksd
`)
	case "zsh":
		fmt.Print(`#compdef ksd
_ksd() {
    local -a out
    out=("${(@f)$(${words[1]} __complete "${(@)words[2,CURRENT]}" 2>/dev/null)}")
    if [[ "${out[1]}" == ":files" ]]; then
        _files
        return
    fi
    out=(${out:#})
    compadd -- "${out[@]}"
}
compdef _ksd ksd
`)
	default:
		fmt.Fprintf(os.Stderr, "unknown shell %q: use bash or zsh\n", shell)
		os.Exit(1)
	}
}

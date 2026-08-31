package gitopsmr

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/territory-grounder/grounder/core/config"
)

// TestRendererEditsOneYAMLFieldPreservingCommentsAndOrder: the structured renderer fetches the current file,
// sets EXACTLY the selected scalar, and re-serialises with comments and key order intact — the property that
// makes a gitops-mr diff reviewable and NOT a blind text rewrite.
func TestRendererEditsOneYAMLFieldPreservingCommentsAndOrder(t *testing.T) {
	os.Setenv("TG_TEST_GITOPS_TOKEN", "glpat-test")
	original := "# app config\nspec:\n  replicas: 2 # current count\n  image: nginx\n"
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("PRIVATE-TOKEN")
		io.WriteString(w, original)
	}))
	defer srv.Close()

	rend := NewRenderer(srv.Client())
	pol := RepoPolicy{
		BaseURL: srv.URL, ProjectPath: "infra/prod", TargetBranch: "main",
		TokenRef:   config.SecretRef("env:TG_TEST_GITOPS_TOKEN"),
		FieldRules: []FieldRule{{RuleID: "reps", File: "argocd-apps/x.yaml", Selector: "spec.replicas"}},
	}
	files, err := rend.Render(pol, []FieldEdit{{FieldRuleID: "reps", NewValue: "5"}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if gotToken != "glpat-test" {
		t.Fatalf("PAT not sent on the fetch: %q", gotToken)
	}
	got := string(files["argocd-apps/x.yaml"])
	if !strings.Contains(got, "replicas: 5") {
		t.Fatalf("replicas not set to 5:\n%s", got)
	}
	for _, want := range []string{"# app config", "current count", "image: nginx"} {
		if !strings.Contains(got, want) {
			t.Fatalf("did not preserve %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "replicas") > strings.Index(got, "image") {
		t.Fatalf("key order not preserved (replicas must precede image):\n%s", got)
	}
	// The OTHER field is untouched — exactly one field changed.
	if !strings.Contains(got, "image: nginx") || strings.Contains(got, "replicas: 2") {
		t.Fatalf("expected only replicas changed:\n%s", got)
	}
}

// TestRendererFailsClosed: an unknown rule, a .tf target (hclwrite follow-on), and a selector that resolves to
// a non-scalar are all refused — never a silent or blind edit.
func TestRendererFailsClosed(t *testing.T) {
	rend := NewRenderer(nil)
	if _, err := rend.Render(RepoPolicy{}, []FieldEdit{{FieldRuleID: "nope"}}); err == nil {
		t.Fatal("want refusal for an unknown field_rule_id")
	}
	tfPol := RepoPolicy{FieldRules: []FieldRule{{RuleID: "tf", File: "k8s/main.tf", Selector: "x"}}}
	if _, err := rend.Render(tfPol, []FieldEdit{{FieldRuleID: "tf", NewValue: "1"}}); err == nil {
		t.Fatal("want refusal for a .tf target (hclwrite is the follow-on)")
	}
	if _, err := rend.Render(RepoPolicy{}, nil); err == nil {
		t.Fatal("want refusal for zero edits")
	}
}

// TestSetYAMLFieldOnlyEditsASingleScalar: the field setter refuses a path to a mapping (a ≠1-field edit) or a
// missing key, and sets a genuine scalar leaf.
func TestSetYAMLFieldOnlyEditsASingleScalar(t *testing.T) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte("spec:\n  replicas: 2\n"), &root); err != nil {
		t.Fatal(err)
	}
	if err := setYAMLField(&root, "spec", "x"); err == nil {
		t.Fatal("want refusal: 'spec' is a mapping, not a single scalar field")
	}
	if err := setYAMLField(&root, "spec.missing", "x"); err == nil {
		t.Fatal("want refusal: missing path")
	}
	if err := setYAMLField(&root, "spec.replicas", "9"); err != nil {
		t.Fatalf("a valid single-scalar edit must succeed: %v", err)
	}
	out, _ := yaml.Marshal(&root)
	if !strings.Contains(string(out), "replicas: 9") {
		t.Fatalf("value not set:\n%s", out)
	}
}

// --- Terraform (hclwrite) path (TG-122 slice 4b) ---

const sampleTF = `# core service
resource "helm_release" "foo" {
  name      = "foo"
  namespace = "core" # pinned

  set {
    name  = "replicas"
    value = "2"
  }
  set {
    name  = "image.tag"
    value = "v1.0"
  }
}
`

// TestRenderTFEditsHelmSetValuePreservingLayout: the hclwrite path edits EXACTLY the named set block's value,
// preserving comments, other set blocks, and layout.
func TestRenderTFEditsHelmSetValuePreservingLayout(t *testing.T) {
	out, err := renderTF([]byte(sampleTF), []resolvedEdit{{selector: "resource.helm_release.foo.set.replicas", value: "5"}})
	if err != nil {
		t.Fatalf("renderTF: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `value = "5"`) {
		t.Fatalf("replicas set value not updated:\n%s", got)
	}
	for _, want := range []string{"# core service", "# pinned", `value = "v1.0"`, `name  = "image.tag"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("did not preserve %q:\n%s", want, got)
		}
	}
	if strings.Count(got, `value = "5"`) != 1 {
		t.Fatalf("exactly one set value must change:\n%s", got)
	}
}

// TestRenderTFEditsBlockAttribute: the plain block-attribute selector edits an EXISTING attribute and refuses
// to create one that does not exist (an attribute-create is not a single-field edit of the reviewed shape).
func TestRenderTFEditsBlockAttribute(t *testing.T) {
	out, err := renderTF([]byte(sampleTF), []resolvedEdit{{selector: "resource.helm_release.foo.namespace", value: "edge"}})
	if err != nil {
		t.Fatalf("renderTF: %v", err)
	}
	if !strings.Contains(string(out), `namespace = "edge"`) {
		t.Fatalf("namespace not updated:\n%s", out)
	}
	if _, err := renderTF([]byte(sampleTF), []resolvedEdit{{selector: "resource.helm_release.foo.nonexistent", value: "x"}}); err == nil {
		t.Fatal("want refusal: creating a new attribute is not a single-field edit")
	}
}

// TestRenderTFFailsClosed: unknown block, unknown set name, malformed tf, short selector — all refused.
func TestRenderTFFailsClosed(t *testing.T) {
	cases := map[string]string{
		"unknown block":  "resource.helm_release.bar.set.replicas",
		"unknown set":    "resource.helm_release.foo.set.cpu",
		"short selector": "replicas",
	}
	for name, sel := range cases {
		if _, err := renderTF([]byte(sampleTF), []resolvedEdit{{selector: sel, value: "1"}}); err == nil {
			t.Errorf("%s: must fail closed", name)
		}
	}
	if _, err := renderTF([]byte("resource {{{"), []resolvedEdit{{selector: "a.b", value: "1"}}); err == nil {
		t.Error("malformed tf: must fail closed")
	}
}

// TestRenderYAMLPreservesSourceIndent: a single-field edit on a 2-space manifest (the k8s/argocd-apps
// convention) must NOT re-indent the whole file — yaml.v3's 4-space Marshal default did, so a one-value change
// touched every line and the MR was un-reviewable (caught live by the TG-122 e2e, MR !533). Exactly one line
// changes now.
func TestRenderYAMLPreservesSourceIndent(t *testing.T) {
	src := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: bentopdf\n  namespace: bentopdf\nspec:\n  replicas: 1\n  selector:\n    matchLabels:\n      app: bentopdf\n"
	out, err := renderYAML([]byte(src), []resolvedEdit{{selector: "spec.replicas", value: "2"}})
	if err != nil {
		t.Fatalf("renderYAML: %v", err)
	}
	if strings.HasPrefix(string(out), "---") {
		t.Fatalf("emitted a document separator:\n%s", out)
	}
	sl := strings.Split(strings.TrimRight(src, "\n"), "\n")
	ol := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(sl) != len(ol) {
		t.Fatalf("line count changed %d -> %d\nout:\n%s", len(sl), len(ol), out)
	}
	var changed []int
	for i := range sl {
		if sl[i] != ol[i] {
			changed = append(changed, i)
		}
	}
	if len(changed) != 1 || ol[changed[0]] != "  replicas: 2" {
		t.Fatalf("want exactly one changed line \"  replicas: 2\", got changed=%v\nout:\n%s", changed, out)
	}
}

// TestRenderYAMLPreservesComments: the node round-trip keeps comments (the other half of a reviewable diff).
func TestRenderYAMLPreservesComments(t *testing.T) {
	out, err := renderYAML([]byte("spec:\n  # keep me\n  replicas: 1\n"), []resolvedEdit{{selector: "spec.replicas", value: "3"}})
	if err != nil {
		t.Fatalf("renderYAML: %v", err)
	}
	if !strings.Contains(string(out), "# keep me") {
		t.Errorf("comment dropped:\n%s", out)
	}
	if !strings.Contains(string(out), "replicas: 3") {
		t.Errorf("value not set:\n%s", out)
	}
}

// TestDetectYAMLIndent: the source indent step drives the encoder (2-space default for flat/comment-led files).
func TestDetectYAMLIndent(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want int
	}{
		{"two-space", "a:\n  b: 1\n", 2},
		{"four-space", "a:\n    b: 1\n", 4},
		{"flat", "a: 1\nb: 2\n", 2},
		{"leading-comment", "# hdr\na:\n  b: 1\n", 2},
	} {
		if got := detectYAMLIndent([]byte(tc.src)); got != tc.want {
			t.Errorf("%s: detectYAMLIndent = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestRenderDispatchesTFThroughTheFullPath: a .tf FieldRule now renders (the slice-4a refusal is lifted), via
// the same fetch-once + closed-rule discipline as YAML.
func TestRenderDispatchesTFThroughTheFullPath(t *testing.T) {
	os.Setenv("TG_TEST_GITOPS_TOKEN", "glpat-test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, sampleTF)
	}))
	defer srv.Close()
	rend := NewRenderer(srv.Client())
	pol := RepoPolicy{
		BaseURL: srv.URL, ProjectPath: "infrastructure/dc1/production", TargetBranch: "main",
		TokenRef:   config.SecretRef("env:TG_TEST_GITOPS_TOKEN"),
		FieldRules: []FieldRule{{RuleID: "reps", File: "k8s/_core/foo/main.tf", Selector: "resource.helm_release.foo.set.replicas"}},
	}
	files, err := rend.Render(pol, []FieldEdit{{FieldRuleID: "reps", NewValue: "7"}})
	if err != nil {
		t.Fatalf("Render(.tf): %v", err)
	}
	if !strings.Contains(string(files["k8s/_core/foo/main.tf"]), `value = "7"`) {
		t.Fatalf("tf not edited through the full path:\n%s", files["k8s/_core/foo/main.tf"])
	}
}

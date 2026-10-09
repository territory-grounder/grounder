package gitopsmr

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	"gopkg.in/yaml.v3"
)

// httpRenderer is the concrete structured Renderer (TG-122 slice 4). It edits EXACTLY ONE field of an EXISTING
// repo file and returns the file's full new content, preserving comments and key order — NEVER regex/sed. It
// fetches the current file (a read, distinct from the Opener's two writes) from the target branch, applies the
// typed edit through a structured, file-type-DISPATCHED document model, and re-serialises:
//
//   - argocd-apps/*.yaml and values.yaml(.tpl) → gopkg.in/yaml.v3's comment/order-preserving Node model.
//   - *.tf / helm_release.set → hashicorp hclwrite (token-level: ParseConfig → block → SetAttributeValue →
//     Bytes; preserves comments/order/indent).
//
// Any other file type is refused fail-closed (never a blind text edit).
type httpRenderer struct {
	http *http.Client
}

// NewRenderer builds the structured Renderer over a bounded-timeout client. A nil client gets a 20s default.
func NewRenderer(client *http.Client) Renderer {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &httpRenderer{http: client}
}

// resolvedEdit is one (selector, new value) after a FieldEdit is bound to its FieldRule — the internal shape the
// per-file renderers apply.
type resolvedEdit struct {
	selector string
	value    string
}

// Render resolves each typed edit to its closed FieldRule, groups the edits by file, fetches each file once,
// applies the single-field edits through the structured model for that file type, and returns each edited
// file's full new content. It fails closed on an unknown FieldRuleID, a non-repo-relative or unsupported file,
// a fetch/parse failure, or a selector that does not resolve to EXACTLY ONE field.
func (r *httpRenderer) Render(pol RepoPolicy, edits []FieldEdit) (map[string][]byte, error) {
	if len(edits) == 0 {
		return nil, ErrNoEdits
	}
	rules := make(map[string]FieldRule, len(pol.FieldRules))
	for _, fr := range pol.FieldRules {
		rules[fr.RuleID] = fr
	}
	byFile := map[string][]resolvedEdit{}
	order := []string{}
	for _, e := range edits {
		fr, ok := rules[e.FieldRuleID]
		if !ok {
			return nil, fmt.Errorf("gitopsmr render: unknown field_rule_id %q (not on the repo's closed rule set)", e.FieldRuleID)
		}
		if fr.File == "" || strings.HasPrefix(fr.File, "/") || strings.Contains(fr.File, "..") {
			return nil, fmt.Errorf("gitopsmr render: field rule %q has a non-repo-relative file %q", fr.RuleID, fr.File)
		}
		if !isYAML(fr.File) && !isTF(fr.File) {
			return nil, fmt.Errorf("gitopsmr render: field rule %q targets %q — only *.tf and argocd/values YAML are rendered (refusing, fail-closed)", fr.RuleID, fr.File)
		}
		if _, seen := byFile[fr.File]; !seen {
			order = append(order, fr.File)
		}
		byFile[fr.File] = append(byFile[fr.File], resolvedEdit{selector: fr.Selector, value: e.NewValue})
	}
	out := make(map[string][]byte, len(byFile))
	for _, f := range order {
		raw, err := r.fetch(pol, f)
		if err != nil {
			return nil, err
		}
		var rendered []byte
		if isYAML(f) {
			rendered, err = renderYAML(raw, byFile[f])
		} else {
			rendered, err = renderTF(raw, byFile[f])
		}
		if err != nil {
			return nil, fmt.Errorf("gitopsmr render %q: %w", f, err)
		}
		out[f] = rendered
	}
	return out, nil
}

// fetch reads the current bytes of a repo-relative file from the target branch (GitLab raw Files API — a READ,
// separate from the Opener's writes). The api-scoped PAT is resolved at read time (INV-13), never logged.
func (r *httpRenderer) fetch(pol RepoPolicy, file string) ([]byte, error) {
	token, err := pol.TokenRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("gitopsmr render: token for %q unresolvable (INV-13): %w", pol.ProjectPath, err)
	}
	ref := pol.TargetBranch
	if ref == "" {
		ref = "main"
	}
	u := strings.TrimRight(pol.BaseURL, "/") + "/api/v4/projects/" + url.PathEscape(pol.ProjectPath) +
		"/repository/files/" + url.PathEscape(file) + "/raw?ref=" + url.QueryEscape(ref)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("gitopsmr render: GET %q → %d: %s", file, resp.StatusCode, msg)
	}
	return b, nil
}

func isYAML(file string) bool {
	return strings.HasSuffix(file, ".yaml") || strings.HasSuffix(file, ".yml") ||
		strings.HasSuffix(file, ".yaml.tpl") || strings.HasSuffix(file, ".yml.tpl")
}

func isTF(file string) bool { return strings.HasSuffix(file, ".tf") }

// --- YAML (argocd-apps / values) ---

// renderYAML parses the file into a comment/order-preserving Node, applies each single-field edit, and
// re-serialises. A parse failure or a selector resolving to ≠1 scalar fails the whole file closed.
func renderYAML(raw []byte, edits []resolvedEdit) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	for _, e := range edits {
		if err := setYAMLField(&root, e.selector, e.value); err != nil {
			return nil, fmt.Errorf("selector %q: %w", e.selector, err)
		}
	}
	// Re-serialise at the SOURCE file's indent, not yaml.v3's 4-space default. yaml.Marshal hardcodes 4, so a
	// single-field edit on a 2-space file (the k8s/argocd-apps convention) would re-indent every line and the MR
	// would not be diff-minimal (the review contract). Only a configured Encoder can match the source.
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(detectYAMLIndent(raw))
	if err := enc.Encode(&root); err != nil {
		return nil, fmt.Errorf("encode yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close yaml encoder: %w", err)
	}
	return buf.Bytes(), nil
}

// detectYAMLIndent infers the source document's indent step (in spaces) so a re-serialised single-field edit
// matches it and stays diff-minimal. It returns the leading-space count of the first line indented deeper than
// its predecessor — the indent unit of a well-formed document — clamped to yaml.v3's supported 1..9, and
// defaults to 2 (the k8s/argocd-apps convention) for a flat file or one that is tab-indented (unsupported).
func detectYAMLIndent(raw []byte) int {
	const fallback = 2
	prev := 0
	for _, line := range bytes.Split(raw, []byte("\n")) {
		body := bytes.TrimLeft(line, " ")
		if len(body) == 0 || body[0] == '#' || body[0] == '\t' {
			continue // blank, comment, or tab-indented — not a space-indent sample
		}
		indent := len(line) - len(body)
		if indent > prev {
			switch step := indent - prev; {
			case step < 1:
				return fallback
			case step > 9:
				return 9
			default:
				return step
			}
		}
		prev = indent
	}
	return fallback
}

// setYAMLField navigates a dotted selector (e.g. "spec.source.helm.replicas") to EXACTLY ONE scalar node and
// sets its value, preserving surrounding comments and order. Fails closed if the path does not resolve, resolves
// to a non-scalar (a map/sequence — a ≠1-field edit), or is empty.
func setYAMLField(root *yaml.Node, selector, value string) error {
	path := strings.Split(strings.TrimSpace(selector), ".")
	if selector == "" || len(path) == 0 {
		return fmt.Errorf("empty selector")
	}
	n := root
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) != 1 {
			return fmt.Errorf("document does not have a single root")
		}
		n = n.Content[0]
	}
	for i, key := range path {
		if n.Kind != yaml.MappingNode {
			return fmt.Errorf("path segment %q: parent is not a mapping (selector does not resolve to a single field)", key)
		}
		val := mappingValue(n, key)
		if val == nil {
			return fmt.Errorf("path segment %q not found", key)
		}
		if i == len(path)-1 {
			if val.Kind != yaml.ScalarNode {
				return fmt.Errorf("selector resolves to a %v, not a single scalar field", val.Kind)
			}
			val.Value = value
			val.Tag = "" // re-infer the scalar tag from the new value
			return nil
		}
		n = val
	}
	return fmt.Errorf("selector did not resolve to a leaf")
}

func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// --- Terraform (hclwrite) ---

// renderTF parses the .tf, applies each single-field edit token-level (preserving comments/order/indent), and
// re-serialises. Selectors:
//   - block attribute:  "resource.helm_release.foo.replicas" → block resource "helm_release" "foo", attr replicas.
//   - helm set value:   "resource.helm_release.foo.set.replicas" → the `set { name = "replicas" … }` block, its
//     `value` — the shape helm_release uses for replicas/image/values.
//
// It fails closed on a parse error, an unresolved block/attribute, or a set name that matches no block. It never
// CREATES an attribute (GetAttribute must already exist) and never routes a ForceNew/replace shape here.
func renderTF(raw []byte, edits []resolvedEdit) ([]byte, error) {
	f, diags := hclwrite.ParseConfig(raw, "edit.tf", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse tf: %s", diags.Error())
	}
	for _, e := range edits {
		if err := setTFField(f.Body(), e.selector, e.value); err != nil {
			return nil, fmt.Errorf("selector %q: %w", e.selector, err)
		}
	}
	return f.Bytes(), nil
}

func setTFField(body *hclwrite.Body, selector, value string) error {
	path := strings.Split(strings.TrimSpace(selector), ".")
	if selector == "" || len(path) < 2 {
		return fmt.Errorf("a tf selector must name at least a block and a field")
	}
	// helm set case: "<block path>.set.<name>" → the set block whose name attribute is <name>, set its value.
	if len(path) >= 4 && path[len(path)-2] == "set" {
		blk := findBlock(body, path[:len(path)-2])
		if blk == nil {
			return fmt.Errorf("block %q not found", strings.Join(path[:len(path)-2], "."))
		}
		setName := path[len(path)-1]
		target := findSetBlockByName(blk.Body(), setName)
		if target == nil {
			return fmt.Errorf("no set block with name %q", setName)
		}
		target.Body().SetAttributeValue("value", cty.StringVal(value))
		return nil
	}
	// block attribute case: last segment is the attribute, the rest is the block path.
	attr := path[len(path)-1]
	blk := findBlock(body, path[:len(path)-1])
	if blk == nil {
		return fmt.Errorf("block %q not found", strings.Join(path[:len(path)-1], "."))
	}
	if blk.Body().GetAttribute(attr) == nil {
		return fmt.Errorf("block has no attribute %q (refusing to create a field)", attr)
	}
	blk.Body().SetAttributeValue(attr, cty.StringVal(value))
	return nil
}

// findBlock resolves a block path where path[0] is the block type and path[1:] are its labels
// (["resource","helm_release","foo"] → resource "helm_release" "foo").
func findBlock(body *hclwrite.Body, path []string) *hclwrite.Block {
	if len(path) == 0 {
		return nil
	}
	return body.FirstMatchingBlock(path[0], path[1:])
}

// findSetBlockByName returns the `set { name = "<name>" … }` block whose name attribute is the given literal
// string, or nil. It compares the rendered name-attribute tokens against a quoted literal — helm set names are
// plain string literals — so a computed/interpolated name simply does not match (fail-closed, never a wrong edit).
func findSetBlockByName(body *hclwrite.Body, name string) *hclwrite.Block {
	want := `"` + name + `"`
	for _, b := range body.Blocks() {
		if b.Type() != "set" {
			continue
		}
		nameAttr := b.Body().GetAttribute("name")
		if nameAttr == nil {
			continue
		}
		if strings.TrimSpace(string(nameAttr.Expr().BuildTokens(nil).Bytes())) == want {
			return b
		}
	}
	return nil
}

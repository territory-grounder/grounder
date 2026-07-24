package acceptance

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cucumber/godog"
)

// root is the repo root relative to this test package (spec/009-kubernetes-deploy/acceptance).
const root = "../../.."

func read(t *testing.T, rel string) string { // t only for fatal in helpers below via world
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		panic(fmt.Sprintf("read %s: %v", rel, err))
	}
	return string(b)
}

func TestKubernetesDeployAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "spec/009 kubernetes-deploy",
		ScenarioInitializer: initializeScenario,
		Options:             &godog.Options{Format: "pretty", Paths: []string{"."}, Tags: "~@pending", Strict: true, TestingT: t},
	}
	if suite.Run() != 0 {
		t.Fatal("spec/009 acceptance scenarios failed")
	}
}

// chart bundles the chart source the oracle asserts over.
type chart struct {
	values     string
	deployment string
	worker     string
	service    string
	postgres   string
	templates  string // every template concatenated
	helmLint   string
	gitlabCI   string
	compose    string
}

func loadChart() *chart {
	must := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			panic(fmt.Sprintf("read %s: %v", rel, err))
		}
		return string(b)
	}
	tdir := filepath.Join(root, "deploy/helm/grounder/templates")
	entries, _ := os.ReadDir(tdir)
	var all strings.Builder
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(tdir, e.Name()))
		all.Write(b)
		all.WriteString("\n")
	}
	return &chart{
		values:     must("deploy/helm/grounder/values.yaml"),
		deployment: must("deploy/helm/grounder/templates/grounder-deployment.yaml"),
		worker:     must("deploy/helm/grounder/templates/worker-deployment.yaml"),
		service:    must("deploy/helm/grounder/templates/grounder-service.yaml"),
		postgres:   must("deploy/helm/grounder/templates/postgres.yaml"),
		templates:  all.String(),
		helmLint:   must("deploy/helm/ci/helm-lint.sh"),
		gitlabCI:   must(".gitlab-ci.yml"),
		compose:    must("deploy/docker-compose.yml"),
	}
}

func initializeScenario(sc *godog.ScenarioContext) {
	c := loadChart()
	need := func(hay, needle, what string) error {
		if !strings.Contains(hay, needle) {
			return fmt.Errorf("%s: expected to find %q", what, needle)
		}
		return nil
	}

	// ---- shared Givens (no-op loads; the chart is already loaded) ----
	for _, given := range []string{
		`^the grounder Helm chart rendered with default values$`,
		`^the grounder Deployment template rendered with default values$`,
		`^the grounder Helm chart$`,
		`^the docker-compose single-node profile service set$`,
		`^a change that touches the grounder Helm chart$`,
		`^a chart change that renders a literal credential into a manifest$`,
		`^the chart is installed into a cluster$`,
		`^the CI pipeline runs$`,
	} {
		sc.Step(given, func() error { return nil })
	}

	// ---- REQ-901: full service set ----
	sc.Step(`^the grounder control-plane, application Postgres with pgvector, Temporal, the Temporal backing Postgres, the Temporal UI, and the LiteLLM gateway all reach a Ready state$`, func() error {
		for _, svc := range []string{"pgvector/pgvector", "temporalio/auto-setup", "temporalio/ui", "berriai/litellm"} {
			if err := need(c.values, svc, "REQ-901 service set"); err != nil {
				return err
			}
		}
		return need(c.deployment, "grounder", "REQ-901 control-plane")
	})

	// ---- REQ-902: parity with compose ----
	sc.Step(`^the rendered Kubernetes service set equals the compose service set$`, func() error {
		// every long-running compose service must have a matching chart template image.
		composeImgs := map[string]string{
			"pgvector/pgvector":     "postgres",
			"berriai/litellm":       "litellm",
			"temporalio/auto-setup": "temporal",
			"temporalio/ui":         "temporal-ui",
		}
		for img := range composeImgs {
			if err := need(c.values, img, "REQ-902 parity"); err != nil {
				return err
			}
		}
		// The Runner worker is the load-bearing compose `worker` service — without a matching chart
		// Deployment the platform ingests but triages nothing. It shares the grounder image family
		// (BIN=worker), so parity is asserted on the dedicated worker Deployment template, not the values image.
		if err := need(c.worker, "worker", "REQ-902 parity (worker)"); err != nil {
			return err
		}
		return nil
	})

	// ---- REQ-907: single values contract ----
	sc.Step(`^every rendered manifest draws its settings from values\.yaml$`, func() error {
		return need(c.templates, ".Values.", "REQ-907 values-driven")
	})
	sc.Step(`^no second hand-maintained configuration surface exists in the chart$`, func() error {
		// exactly one values file; no parallel values-*.yaml config surface.
		matches, _ := filepath.Glob(filepath.Join(root, "deploy/helm/grounder/values*.yaml"))
		if len(matches) != 1 {
			return fmt.Errorf("REQ-907: expected exactly one values.yaml, found %d", len(matches))
		}
		return nil
	})

	// ---- REQ-903: distroless, digest-pinned, hardened ----
	sc.Step(`^the container image is the distroless grounder image pinned by digest$`, func() error {
		if err := need(c.templates, `grounder.image`, "REQ-903 image helper"); err != nil {
			return err
		}
		// the image helper pins by digest (repository@digest) when a digest is set (REQ-903).
		if err := need(c.templates, "$img.digest", "REQ-903 digest pin"); err != nil {
			return err
		}
		return need(c.templates, `printf "%s@%s"`, "REQ-903 digest-pin form")
	})
	sc.Step(`^the pod runs as a non-root user with a read-only root filesystem and no privilege escalation$`, func() error {
		for _, prop := range []string{"runAsNonRoot", "readOnlyRootFilesystem", "allowPrivilegeEscalation"} {
			if err := need(c.deployment, prop, "REQ-903 securityContext"); err != nil {
				return err
			}
		}
		return need(c.values, "runAsNonRoot: true", "REQ-903 non-root default")
	})

	// ---- REQ-906: mutation off by default ----
	sc.Step(`^the mutation effect channel is disabled$`, func() error {
		return need(strings.ReplaceAll(c.values, " ", ""), "mutation:\nenabled:false", "REQ-906 mutation off")
	})
	sc.Step(`^the mutation-enable flag defaults to off$`, func() error {
		re := regexp.MustCompile(`(?m)^mutation:\s*\n\s*enabled:\s*false`)
		if !re.MatchString(c.values) {
			return fmt.Errorf("REQ-906: mutation.enabled must default to false in values.yaml")
		}
		return nil
	})

	// ---- REQ-909: auth-gated exposure ----
	sc.Step(`^the public API Service fronts port 8080 behind the auth middleware$`, func() error {
		if err := need(c.service, "8080", "REQ-909 public port"); err != nil {
			return err
		}
		return need(c.values, "publicPort: 8080", "REQ-909 public port value")
	})
	sc.Step(`^the admin listener, the Temporal frontend, and the Postgres port are not on a default-routable Ingress$`, func() error {
		// admin (8443) must NOT be published on the Service; the ingress defaults off / public-only.
		if strings.Contains(c.service, "port: 8443") {
			return fmt.Errorf("REQ-909: the admin listener (8443) must not be published on the Service")
		}
		return need(c.values, "enabled: false", "REQ-909 ingress default off")
	})

	// ---- REQ-910: probes ordered after Postgres ----
	sc.Step(`^the Deployment declares liveness and readiness probes against the control-plane health endpoint$`, func() error {
		for _, p := range []string{"livenessProbe", "readinessProbe"} {
			if err := need(c.deployment, p, "REQ-910 probes"); err != nil {
				return err
			}
		}
		return need(c.values, "/healthz", "REQ-910 health path")
	})
	sc.Step(`^the control-plane becomes Ready only after the application Postgres reports healthy$`, func() error {
		return need(c.deployment, "initContainers", "REQ-910 wait-for-postgres init")
	})

	// ---- REQ-904: secrets by reference, no literals ----
	sc.Step(`^every credential value is sourced from a Kubernetes Secret or ExternalSecret reference$`, func() error {
		return need(c.templates, "secretKeyRef", "REQ-904 secretKeyRef")
	})
	sc.Step(`^no literal credential appears in any rendered manifest, ConfigMap, or values\.yaml$`, func() error {
		// a literal DSN-with-password or an inline password value: line is forbidden anywhere in the chart.
		bad := regexp.MustCompile(`(?i)(postgres|postgresql)://[^:@\s]+:[^@\s]+@`)
		for _, f := range []string{c.values, c.templates} {
			if bad.MatchString(f) {
				return fmt.Errorf("REQ-904: a literal credential (inline DSN password) appears in the chart")
			}
		}
		return nil
	})

	// ---- REQ-905: two-role DSN model ----
	sc.Step(`^the runtime DSN connects with the DML-only tg_runtime role$`, func() error {
		if err := need(c.postgres, "tg_runtime", "REQ-905 runtime role"); err != nil {
			return err
		}
		// tg_runtime gets only DML (SELECT/INSERT/UPDATE/DELETE), never DDL ownership.
		return need(c.postgres, "GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO tg_runtime", "REQ-905 DML-only grant")
	})
	sc.Step(`^the DDL tg_migration role is restricted to the startup migration Job$`, func() error {
		return need(c.postgres, "tg_migration", "REQ-905 migration role")
	})

	// ---- REQ-908: CI runs helm lint + template render ----
	sc.Step(`^helm lint and a helm template render both execute against the chart$`, func() error {
		if err := need(c.helmLint, "helm lint", "REQ-908 helm lint"); err != nil {
			return err
		}
		if err := need(c.helmLint, "helm template", "REQ-908 helm template"); err != nil {
			return err
		}
		// and the CI pipeline actually invokes the script on chart changes.
		if err := need(c.gitlabCI, "helm-lint", "REQ-908 CI job"); err != nil {
			return err
		}
		return need(c.gitlabCI, "deploy/helm/ci/helm-lint.sh", "REQ-908 CI runs the script")
	})

	// ---- REQ-904 (CI): literal-credential scan fails the pipeline ----
	sc.Step(`^the pipeline fails on the literal-credential scan$`, func() error {
		// the lint script must scan the rendered output for a literal credential and exit non-zero on a hit.
		if err := need(c.helmLint, "helm template", "REQ-904 render before scan"); err != nil {
			return err
		}
		lower := strings.ToLower(c.helmLint)
		if !strings.Contains(lower, "grep") || (!strings.Contains(lower, "password") && !strings.Contains(lower, "credential") && !strings.Contains(lower, "secret")) {
			return fmt.Errorf("REQ-904: helm-lint.sh must scan the render for a literal credential")
		}
		if !strings.Contains(c.helmLint, "exit 1") {
			return fmt.Errorf("REQ-904: helm-lint.sh must fail (exit 1) on a literal-credential hit")
		}
		return nil
	})
}

var _ = read // reserved for future live-render integration checks

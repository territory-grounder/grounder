# spec/009 — Kubernetes / Helm first-class deploy target acceptance oracles.
# Every scenario is: the chart is authored in this spec but the godog step definitions that
# render and assert against it are not written yet. They are tracked as declared debt in
# acceptance/_test_mapping.json and skipped by the runner (Tags: "~") until bound to real code.
Feature: Kubernetes / Helm is a first-class deploy target at parity with docker-compose

  A helm install brings up the same service set as the compose single-node profile, from one
  authoritative values contract, with credentials by reference and mutation held off in Phase 0/1.

  @REQ-901
  Scenario: A helm install brings up the full service set
    Given the grounder Helm chart rendered with default values
    When the chart is installed into a cluster
    Then the grounder control-plane, application Postgres with pgvector, Temporal, the Temporal backing Postgres, the Temporal UI, and the LiteLLM gateway all reach a Ready state

  @REQ-902
  Scenario: The Helm chart is a first-class target at parity with compose
    Given the docker-compose single-node profile service set
    And the grounder Helm chart rendered with default values
    Then the rendered Kubernetes service set equals the compose service set

  @REQ-907
  Scenario: The values file is the single authoritative configuration contract
    Given the grounder Helm chart
    Then every rendered manifest draws its settings from values.yaml
    And no second hand-maintained configuration surface exists in the chart

  @REQ-903
  Scenario: The grounder Deployment runs the same distroless binary image
    Given the grounder Deployment template rendered with default values
    Then the container image is the distroless grounder image pinned by digest
    And the pod runs as a non-root user with a read-only root filesystem and no privilege escalation

  @REQ-906
  Scenario: Mutation stays disabled by default under the Phase 0/1 foundation
    Given the grounder Helm chart rendered with default values
    Then the mutation effect channel is disabled
    And the mutation-enable flag defaults to off

  @REQ-909
  Scenario: The public Service is auth-gated and the admin listener is not default-exposed
    Given the grounder Helm chart rendered with default values
    Then the public API Service fronts port 8080 behind the auth middleware
    And the admin listener, the Temporal frontend, and the Postgres port are not on a default-routable Ingress

  @REQ-910
  Scenario: The grounder Deployment declares liveness and readiness probes ordered after Postgres
    Given the grounder Deployment template rendered with default values
    Then the Deployment declares liveness and readiness probes against the control-plane health endpoint
    And the control-plane becomes Ready only after the application Postgres reports healthy

  @REQ-904
  Scenario: Credentials are sourced from Kubernetes Secrets and never rendered as literals
    Given the grounder Helm chart rendered with default values
    Then every credential value is sourced from a Kubernetes Secret or ExternalSecret reference
    And no literal credential appears in any rendered manifest, ConfigMap, or values.yaml

  @REQ-905
  Scenario: The runtime DSN uses the DML-only role and migration uses the DDL role
    Given the grounder Helm chart rendered with default values
    Then the runtime DSN connects with the DML-only tg_runtime role
    And the DDL tg_migration role is restricted to the startup migration Job

  @REQ-908
  Scenario: CI runs helm lint and template render on a chart change
    Given a change that touches the grounder Helm chart
    When the CI pipeline runs
    Then helm lint and a helm template render both execute against the chart

  @REQ-908 @REQ-904
  Scenario: CI fails when a rendered manifest contains a literal credential
    Given a chart change that renders a literal credential into a manifest
    When the CI pipeline runs
    Then the pipeline fails on the literal-credential scan

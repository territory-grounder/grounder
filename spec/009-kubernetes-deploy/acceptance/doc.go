// Package acceptance holds the runnable godog acceptance oracles for spec/009 (Kubernetes / Helm as a
// first-class deploy target). The scenarios drive the REAL chart: they read deploy/helm/grounder/* , the
// helm-lint CI script, and the compose profile, and assert the governed properties (mutation OFF,
// distroless+hardened grounder pod, secrets by reference only, the two-role DSN model, auth-gated
// exposure, probes ordered after Postgres, and the CI lint/literal-scan gate). The checks are pure-Go
// text/structure assertions over the chart source, so they run in CI without a Helm binary or a cluster.
package acceptance

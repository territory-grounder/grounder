// tools/seed-knowledge is an ISOLATED dev-only module (TG-125). It is deliberately NOT part of the
// runtime module (../../go.mod): it depends on a pure-Go sqlite driver (modernc.org/sqlite) purely to
// read the predecessor's incident_knowledge one-shot at build time, and that dependency must never enter
// the distroless runtime's supply chain. `go build ./...` / `go vet ./...` / `go test ./...` from the repo
// root SKIP this nested module, so `make all` stays light. Build/run it explicitly:
//
//	cd tools/seed-knowledge && go run . --out ../../deploy/knowledge/corpus.seed.json
//
// The `replace` below points the parent module at the working tree so the tool keys the corpus through the
// EXACT SAME librenms.SlugifyRule + core/screen.Redact the live ingester and the round-trip guard use.
module github.com/territory-grounder/grounder/tools/seed-knowledge

go 1.25.0

require (
	github.com/territory-grounder/grounder v0.0.0
	modernc.org/sqlite v1.54.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	modernc.org/libc v1.74.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

replace github.com/territory-grounder/grounder => ../..

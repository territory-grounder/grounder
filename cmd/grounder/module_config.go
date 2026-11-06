package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.temporal.io/sdk/client"

	"log"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/secretwrite"
	"github.com/territory-grounder/grounder/modules/catalog"
	"github.com/territory-grounder/grounder/modules/credsource/vault"
	"github.com/territory-grounder/grounder/modules/desc"
	tg "github.com/territory-grounder/grounder/temporal"
	"github.com/territory-grounder/grounder/temporal/moduletest"
)

// catalogSchema renders the module catalog as the console's dialog schema.
//
// It reports UNDESCRIBED modules alongside the described ones. A console listing only what it has a form
// for cannot distinguish "this is everything" from "this is what somebody got round to describing", and
// that distinction is the entire reason the descriptor backlog is an explicit list rather than an absence.
type catalogSchema struct {
	// fleet is the ONE merged view — local registry UNION the worker's projection through its staleness
	// cutoff. Shared with /v1/capabilities (TG-268) rather than duplicated: this page renders both, and a
	// second copy of the merge is how they came to disagree in the first place.
	fleet fleetView
}

func (c catalogSchema) Schema(ctx context.Context) (httpapi.ModuleSchemaPage, error) {
	all, err := catalog.All()
	if err != nil {
		return httpapi.ModuleSchemaPage{}, err
	}
	// Which modules are actually registered and enabled right now — AND which this process can see at all.
	//
	// The registry is keyed by the pair, so a module missing from it is genuinely unknown here rather than
	// off. Reporting the two as one bool made every worker-resident connector (all the notifiers,
	// trackers, cmdb, credsource, discovery and knowledge sources) render as disabled. `observed` is what
	// separates "this process looked and the answer was no" from "this process cannot see it".
	// Which modules are registered and enabled right now — AND which this process can see at all.
	//
	// `known` separates "this process looked and the answer was no" from "this process cannot see it".
	// Reporting the two as one bool made every worker-resident connector render as disabled (TG-251).
	enabled, observed := map[string]bool{}, map[string]bool{}
	for k, e := range c.fleet.entries(ctx) {
		observed[k] = e.known
		if e.enabled {
			enabled[k] = true
		}
	}
	page := httpapi.ModuleSchemaPage{}
	for _, d := range all {
		dto := httpapi.ModuleSchemaDTO{
			Surface: d.Surface, SourceType: d.SourceType, Title: d.Title, Summary: d.Summary,
			HasSecret: d.Secret.KVPath != "", TestVerb: d.Test.Verb,
			Enabled:      enabled[d.Surface+"/"+d.SourceType],
			EnabledKnown: observed[d.Surface+"/"+d.SourceType],
		}
		for _, f := range d.Fields {
			fd := httpapi.ModuleFieldDTO{
				Name: f.Name, EnvKey: f.EnvKey, Label: f.Label, Help: f.Help,
				Type: string(f.Type), Security: string(f.Security), Effect: string(f.Effect),
				Required: f.Required, Pattern: f.Pattern, MaxItems: f.MaxItems, MaxLen: f.MaxLen,
			}
			// A secret VALUE has no config key by construction — it never enters the config store.
			if f.Type != desc.TypeSecretValue && f.EnvKey != "" && f.Effect != desc.EffectReadOnly {
				fd.ConfigKey = catalog.ConfigKeyName(d.Surface, d.SourceType, f.Name)
			}
			dto.Fields = append(dto.Fields, fd)
		}
		page.Modules = append(page.Modules, dto)
	}
	// Each entry carries the REASON it has no dialog, so the console can say "nothing to configure"
	// where that is true instead of implying a form somebody still owes.
	for _, pkg := range catalog.Undescribed() {
		page.Undescribed = append(page.Undescribed, httpapi.ModuleUndescribedDTO{
			Package: pkg, Reason: catalog.UndescribedReason(pkg),
		})
	}
	return page, nil
}

// capabilityStaleWindow is the reader's freshness contract: 3x the worker's publish interval, so one
// missed heartbeat is jitter and two are a dead publisher. Reads the same env key the worker's loop
// reads (compose forwards it to both), keeping the two halves of the contract in one knob.
func capabilityStaleWindow() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("TG_CAPABILITY_PROJECTION_INTERVAL")); err == nil && d > 0 {
		return 3 * d
	}
	return 3 * time.Minute
}

// temporalModuleTest starts the worker-side test lane. The modules live in the worker; this process has
// none, so the request has to cross that gap rather than pretend it can call a module directly.
type temporalModuleTest struct{ c client.Client }

func (t temporalModuleTest) TestModule(ctx context.Context, surface, sourceType, operator string) (httpapi.ModuleTestOutcome, error) {
	run, err := t.c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		// Keyed per module so two operators pressing Test at once do not queue behind each other, and a
		// stuck probe cannot wedge every other module's test.
		ID:        fmt.Sprintf("tg-module-test-%s-%s", surface, sourceType),
		TaskQueue: tg.TaskQueueRunner,
	}, moduletest.TestModuleWorkflow, moduletest.Request{
		Surface: surface, SourceType: sourceType, Operator: operator,
	})
	if err != nil {
		return httpapi.ModuleTestOutcome{}, err
	}
	var res moduletest.Result
	if err := run.Get(ctx, &res); err != nil {
		return httpapi.ModuleTestOutcome{}, err
	}
	return httpapi.ModuleTestOutcome{
		Surface: res.Surface, SourceType: res.SourceType, OK: res.OK,
		Summary: res.Summary, Detail: res.Detail, ElapsedMS: res.ElapsedMS,
	}, nil
}

// baoSecretWriter builds the module-secret writer from the CONSOLE WRITER AppRole.
//
// It is a SEPARATE OpenBao identity from TG's own read credential, and that separation is the security
// property the whole feature rests on: TG's identity can read every operational secret, and a settings
// dialog must not inherit that just because it needs to set one. The writer's policy grants create/update
// on the module lanes and nothing else — verified live: it cannot read back even the secret it just wrote.
//
// Unconfigured ⇒ nil ⇒ the /secret route stays 503. That is the honest state, and it is strictly better
// than falling back to a credential that happens to work.
func baoSecretWriter(addr, roleIDRef, secretIDRef, caPath string, logf func(string, ...any)) httpapi.ModuleSecretWriter {
	if addr == "" || roleIDRef == "" || secretIDRef == "" {
		logf("module secrets: writer NOT configured (TG_OPENBAO_WRITER_ROLE_ID_REF / _SECRET_ID_REF unset) " +
			"— POST /v1/modules/{surface}/{source}/secret stays 503 and the dialog renders its secret field disabled")
		return nil
	}
	c, err := vault.New(vault.Config{
		BaseURL: addr,
		Auth: vault.AppRole{
			RoleIDRef:   config.SecretRef(roleIDRef),
			SecretIDRef: config.SecretRef(secretIDRef),
		},
		CACertPath: caPath,
	})
	if err != nil {
		logf("module secrets: writer could not be built (%v) — the secret route stays 503", err)
		return nil
	}
	logf("module secrets: writer armed against %s — the console can ROTATE a module credential and cannot READ one", addr)
	return moduleSecretBackend{w: secretwrite.Writer{Lanes: catalog.LaneResolverFunc{}, Backend: c}}
}

// moduleSecretBackend adapts core/secretwrite to the HTTP surface.
type moduleSecretBackend struct{ w secretwrite.Writer }

func (b moduleSecretBackend) WriteModuleSecret(ctx context.Context, surface, sourceType, value, operator string) (httpapi.ModuleSecretOutcome, error) {
	out, err := b.w.Write(ctx, surface, sourceType, value)
	if err != nil {
		return httpapi.ModuleSecretOutcome{}, err
	}
	// AUDITED, and the record names the act and the destination — never the material. INV-19 wants the
	// decision on the record; it does not want the credential on it.
	log.Printf("module secrets: %s rotated the %s/%s credential at %s#%s",
		operator, out.Surface, out.SourceType, out.KVPath, out.Field)
	return httpapi.ModuleSecretOutcome{
		Surface: out.Surface, SourceType: out.SourceType, KVPath: out.KVPath, Field: out.Field,
	}, nil
}

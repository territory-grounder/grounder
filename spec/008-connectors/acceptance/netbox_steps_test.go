package acceptance

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/cucumber/godog"

	cmdb "github.com/territory-grounder/grounder/adapters/cmdb"
	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/cmdb/netbox"
)

// NetBox CMDB (REQ-810): re-read the canonical entity by id + expose its changelog to triage.
func init() {
	moduleStepRegistrars = append(moduleStepRegistrars, registerNetboxSteps)
}

type netboxFakeDoer struct{}

func (netboxFakeDoer) Do(req *http.Request) (*http.Response, error) {
	body := "{}"
	switch {
	case strings.Contains(req.URL.Path, "/api/dcim/devices/42/"):
		body = `{"id":42,"name":"sw-core-01","display":"sw-core-01","status":{"value":"active"},"site":{"name":"NL"}}`
	case strings.Contains(req.URL.Path, "/api/extras/object-changes/"):
		body = `{"results":[{"id":9,"action":"updated","time":"2026-07-15T12:00:00Z","user_name":"ops"}]}`
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

type netboxWorld struct {
	reg     *modules.Registry
	entity  cmdb.Entity
	changes int
	err     error
}

func registerNetboxSteps(sc *godog.ScenarioContext) {
	w := &netboxWorld{}

	sc.Step(`^the NetBox CMDB module is registered and enabled$`, func() error {
		_ = os.Setenv("TG_NETBOX_ACCEPT_TOKEN", "tok")
		w.reg = modules.NewRegistry()
		mod := netbox.New("https://netbox.test", "env:TG_NETBOX_ACCEPT_TOKEN", netbox.WithHTTPClient(netboxFakeDoer{}))
		return w.reg.Register(modules.Registration{
			Surface: modules.SurfaceCMDB, SourceType: netbox.SourceType, Capability: "cmdb.netbox", Enabled: true, Adapter: mod,
		})
	})

	sc.Step(`^an ingested payload names a device$`, func() error {
		reg, err := w.reg.Resolve(modules.SurfaceCMDB, netbox.SourceType)
		if err != nil {
			return fmt.Errorf("the enabled module must resolve: %w", err)
		}
		c, ok := reg.Adapter.(cmdb.CMDB)
		if !ok {
			return fmt.Errorf("the registered adapter must satisfy adapters/cmdb.CMDB")
		}
		// re-read the canonical entity by id (INV-05), then expose its changelog.
		if w.entity, err = c.Resolve(context.Background(), "device", "42"); err != nil {
			w.err = err
			return nil
		}
		nb, ok := reg.Adapter.(*netbox.Module)
		if !ok {
			return fmt.Errorf("expected the concrete NetBox module for its changelog")
		}
		changes, err := nb.Changelog(context.Background(), "42")
		if err != nil {
			w.err = err
			return nil
		}
		w.changes = len(changes)
		return nil
	})

	sc.Step(`^the canonical entity is re-read from NetBox by id before dispatch and its changelog is exposed to triage context$`, func() error {
		if w.err != nil {
			return fmt.Errorf("resolve + changelog must succeed: %w", w.err)
		}
		if w.entity.ID != "42" || w.entity.Name != "sw-core-01" {
			return fmt.Errorf("the canonical entity must be re-read by id, got %+v", w.entity)
		}
		if w.changes == 0 {
			return fmt.Errorf("the entity changelog must be exposed to triage, got 0 changes")
		}
		return nil
	})
}

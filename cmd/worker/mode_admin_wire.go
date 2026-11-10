package main

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/territory-grounder/grounder/core/credential/dyndb"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/temporal/modetransition"
	"github.com/territory-grounder/grounder/temporal/runner"
	"go.temporal.io/sdk/worker"
)

// modeAdminDB is the pool the two GUARDED writes go through (TG-661): raising the autonomy mode into an actuating
// one, and re-arming the safety breaker. migration 0135 refuses both from every runtime role (tg_runtime /
// tg_triage / tg_actuate keep their UPDATE grants for the kill direction — a safety kill from any plane must never
// wait on a credential). Only the tg_mode_admin role may raise/re-arm, and this process reaches it through a
// dyndb lease (TG_MODE_ADMIN_DSN=dyn:tg_mode_admin): a short-lived login no other plane holds, minted by the
// OpenBao database engine, revoked at shutdown.
//
// nil (unset, or the lease could not be minted) ⇒ the stores fall back to the plane pool and the database guard
// refuses every raise/re-arm with 42501. That is the honest unarmed state and the boot log says so; it is NOT
// fatal, because the kill direction keeps working and a worker that cannot raise the mode is safe, while a worker
// that refuses to boot takes triage down with it.
var modeAdminDB *db.Pool

// wireModeAdminPool reads TG_MODE_ADMIN_DSN with os.Getenv DIRECTLY (it names a database login — the same cycle
// the plane DSNs are exempt for; exempted by name in boot_config_test.go) and connects. A `dyn:<role>` DSN needs
// the dyndb engine; a static DSN is accepted for a deployment without the engine.
func wireModeAdminPool(dynProvider *dyndb.Provider, dynDSNTmpl string) {
	dsn := strings.TrimSpace(os.Getenv("TG_MODE_ADMIN_DSN"))
	if dsn == "" {
		log.Print("mode admin pool: TG_MODE_ADMIN_DSN unset — the database guard (migration 0135) REFUSES every raise of " +
			"the autonomy mode and every breaker re-arm from this worker; lowering and tripping keep working (TG-661)")
		return
	}
	var pool *db.Pool
	var err error
	if role, isDyn := strings.CutPrefix(dsn, dyndb.Scheme+":"); isDyn {
		if dynProvider == nil {
			log.Printf("mode admin pool: TG_MODE_ADMIN_DSN is dyn:%s but dynamic credentials are OFF (TG_DYNDB_ADDR unset) — "+
				"raises and re-arms will be refused by the database guard until the engine is armed (TG-661)", role)
			return
		}
		pool, err = db.ConnectDynamic(context.Background(), dynDSNTmpl, dynProvider.Credentials(role))
		if err == nil {
			dyndb.ArmRotationEviction(dynProvider, role, pool.Reset, log.Printf)
		}
	} else {
		pool, err = db.Connect(context.Background(), dsn)
	}
	if err != nil {
		log.Printf("mode admin pool: connect failed (%v) — raises and re-arms will be refused by the database guard; the kill "+
			"direction is unaffected (TG-661)", err)
		return
	}
	modeAdminDB = pool
	log.Print("mode admin pool: ARMED — mode raises and safety-breaker re-arms go through the tg_mode_admin lease; every other " +
		"plane may only lower / trip (TG-661, migration 0135)")
}

// registerActuationPlane registers everything the actuation queue serves. The mode write lives here so that the
// tg_mode_admin login is held by the actuation plane and never by the triage plane, which handles untrusted content.
func registerActuationPlane(aw worker.Worker, acts *runner.Activities, modeTransitionActs *modetransition.Activities) {
	runner.RegisterActuationActivities(aw, acts)
	registerModeWrite(aw, modeTransitionActs)
}

// registerModeTransitionRunner registers the operator-invoked autonomy-mode transition workflow on the runner queue
// (spec/015 REQ-1502, the last gate before the mutation flip), and the mode write itself when this plane holds the login.
func registerModeTransitionRunner(w planeWorker, modeTransitionActs *modetransition.Activities) {
	if modeTransitionActs == nil {
		return
	}
	w.RegisterWorkflow(modetransition.ModeTransitionWorkflow)
	registerModeWrite(w, modeTransitionActs)
}

// registerModeWrite registers the mode write on this queue whether or not the mode-admin login was obtained, because
// lowering the mode must keep working when it was not. The grounder's queue setting picks which plane is used, and the
// database guard still refuses a raise from any plane without the login.
func registerModeWrite(r runner.ActivityRegistry, modeTransitionActs *modetransition.Activities) {
	if modeTransitionActs != nil {
		r.RegisterActivity(modeTransitionActs.ApplyModeTransitionActivity)
	}
}

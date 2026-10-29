package postgres

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
)

// serverWorkload is the key of the workload the definition contributes.
const serverWorkload = "server"

// notRunning is the exit code the runtime gives a probe whose target container is not running.
const notRunning = -1

// probeBudgetSeconds keeps the readiness wait under the runtime's two-minute probe bound.
const probeBudgetSeconds = 100

// Probe IDs name the round a result belongs to.
const (
	probeReady     = "ready"
	probeCluster1  = "catalog-1"
	probeCluster2  = "catalog-2"
	probeDatabases = "catalog-dbs"
	probeCheck     = "check"
	probeMutate    = "mutate"
	probeMark      = "mark"
	probeLock      = "lock"
)

// access is what an inspection learned about the server.
type access string

const (
	accessOK      access = "present"
	accessAbsent  access = "absent"
	accessDenied  access = "denied"
	accessUnknown access = "unknown"
)

func target(i module.Instance) string { return i.Key + "--" + serverWorkload }

// adminProbe runs SQL as the postgres superuser over the in-container socket with peer authentication.
// The SQL is the probe's stdin; no port and no stored password is involved.
func adminProbe(i module.Instance, c adminCall) module.Probe {
	return module.Probe{
		ID: c.ID, Kind: module.ProbeExec, Target: target(i), User: superuser,
		Argv:  []string{"psql", "-X", "-v", "ON_ERROR_STOP=1", "-v", "VERBOSITY=sqlstate", "-At", "-q", "-d", c.Database},
		Stdin: c.SQL, SecretEnv: c.Env,
	}
}

// adminCall is one admin SQL probe: its round, the database it connects to, its script and the
// secrets it binds.
type adminCall struct {
	ID, Database, SQL string
	Env               []module.SecretBinding
}

// superuser is the role the driver connects as, over the socket with peer authentication, and
// adminDatabase the database it connects to first.
const (
	superuser     = "postgres"
	adminDatabase = "postgres"
)

// readyProbe waits, bounded, until the server accepts TCP connections. The temporary server the image
// starts while it initialises does not listen on TCP, so this ignores it.
func readyProbe(i module.Instance, timeoutSeconds int) module.Probe {
	seconds := min(timeoutSeconds, probeBudgetSeconds)
	loop := `n=0; while [ "$n" -lt "$1" ]; do pg_isready -q -h 127.0.0.1 -p 5432 && exit 0; n=$((n+1)); sleep 1; done; exit 1`
	return module.Probe{
		ID: probeReady, Kind: module.ProbeExec, Target: target(i), User: superuser,
		Argv: []string{"sh", "-c", loop, "sh", strconv.Itoa(seconds)},
	}
}

// classify tells absent, denied and unknown apart from one probe result.
func classify(res module.ProbeResult) access {
	switch {
	case res.ExitCode == 0:
		return accessOK
	case res.ExitCode == notRunning:
		return accessAbsent
	}
	// psql runs with VERBOSITY=sqlstate, so an error line carries the SQLSTATE and no message text,
	// which depends on the server's language.
	if deniedState().MatchString(res.Stderr) {
		return accessDenied
	}
	return accessUnknown
}

// deniedState matches an error line of the SQLSTATEs that mean the role may not do this: insufficient
// privilege, invalid authorization and invalid password.
func deniedState() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\S+:\s+(?:42501|28000|28P01)\s*$`)
}

// probeError describes a failed probe without echoing a verifier or a bound value.
func probeError(what string, res module.ProbeResult) error {
	return fmt.Errorf("%s failed with exit code %d: %s", what, res.ExitCode, strings.TrimSpace(redact(res.Stderr)))
}

// resultFor returns the result with the given ID.
func resultFor(results []module.ProbeResult, id string) (module.ProbeResult, bool) {
	for _, r := range results {
		if r.ID == id {
			return r, true
		}
	}
	return module.ProbeResult{}, false
}

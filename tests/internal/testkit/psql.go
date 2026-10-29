package testkit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ErrProbeTimedOut marks a connection attempt that hung instead of ending in a result.
var ErrProbeTimedOut = errors.New("testkit: the connection attempt did not end within its budget")

// ProbeBudget bounds one connection attempt: a rejection is a prompt error result, never a wait.
const ProbeBudget = 60 * time.Second

var oneShots atomic.Int64

// Connection is one psql attempt from a throwaway container on the scope's network. The CA and the
// password reach the container by standard input and the process environment only, never by a bind
// mount or the command line.
type Connection struct {
	// Name prefixes the throwaway container's name; it carries the rig's name so cleanup removes it.
	Name string
	// Network is the Compose network of the scope, "<compose project>_default".
	Network string
	// Host, User, Database and Password are the endpoint and identity.
	Host, User, Database, Password string
	// SSLMode is a libpq sslmode.
	SSLMode string
	// CA is the PEM the attempt trusts; empty trusts none.
	CA []byte
}

// Run runs sql and returns psql's output. A rejection is an error whose text holds psql's message;
// ErrProbeTimedOut is returned when the attempt does not end within ProbeBudget.
func (c Connection) Run(ctx context.Context, sql string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeBudget)
	defer cancel()
	name := c.Name + "-psql-" + strconv.FormatInt(oneShots.Add(1), 10)
	script := `cat > /tmp/ca.crt; exec psql -X -v ON_ERROR_STOP=1 -v VERBOSITY=sqlstate -At -h "$SP_HOST" -U "$SP_USER" -d "$SP_DB" -c "$SP_SQL"`
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "--name", name, "--network", c.Network,
		"-e", "PGPASSWORD", "-e", "PGSSLMODE="+c.SSLMode, "-e", "PGSSLROOTCERT=/tmp/ca.crt", "-e", "PGCONNECT_TIMEOUT=20",
		"-e", "SP_HOST="+c.Host, "-e", "SP_USER="+c.User, "-e", "SP_DB="+c.Database, "-e", "SP_SQL="+sql,
		PostgresImage, "sh", "-c", script)
	cmd.Env = append(cmd.Environ(), "PGPASSWORD="+c.Password)
	cmd.Stdin = bytes.NewReader(c.CA)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	text := strings.TrimSpace(out.String())
	if ctx.Err() != nil {
		_, _ = DockerErr("rm", "-f", name)
		return text, fmt.Errorf("%w: %w", ErrProbeTimedOut, ctx.Err())
	}
	if err != nil {
		return text, fmt.Errorf("psql as %s to %s: %w: %s", c.User, c.Host, err, text)
	}
	return text, nil
}

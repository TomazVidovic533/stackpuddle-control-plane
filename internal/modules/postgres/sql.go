package postgres

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ErrUnsafeText marks a name or value the SQL builders refuse to place in a statement.
var ErrUnsafeText = errors.New("text is not safe to place in a statement")

// transactionHeader starts every provisioning transaction. The settings are local to it, so a statement
// that carries a verifier is never written to the server log, and an error does not repeat it.
const transactionHeader = "BEGIN;\nSET LOCAL log_statement = 'none';\nSET LOCAL log_min_error_statement = 'panic';\n" +
	"SET LOCAL log_min_duration_statement = -1;\nSET LOCAL log_min_duration_sample = -1;\nSET LOCAL log_transaction_sample_rate = 0;\n"

// verifierEnv is the name of the probe environment variable that holds a role's SCRAM verifier.
const verifierEnv = "SP_PG_VERIFIER"

func identifierPattern() *regexp.Regexp { return regexp.MustCompile(`^[a-z][a-z0-9_]*$`) }

// markerTextPattern limits the scope uid and instance key a marker is built from.
func markerTextPattern() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`) }

// quoteIdent quotes name as a SQL identifier: double quotes, with embedded quotes doubled. A NUL byte
// or an empty name is refused, because PostgreSQL cannot hold either in an identifier.
func quoteIdent(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: an identifier is empty or holds a NUL byte", ErrUnsafeText)
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`, nil
}

// managedName checks a name the driver was handed in a step and returns its quoted form. Only names
// the module's own schema allows are acted on, so a forged step cannot reach another object.
func managedName(name string) (string, error) {
	if !identifierPattern().MatchString(name) {
		return "", fmt.Errorf("%w: a name does not match the module's identifier rule", ErrUnsafeText)
	}
	return quoteIdent(name)
}

// psqlLiteral returns text as a single-quoted psql argument for \set, with backslash and quote
// escaped. Control characters are refused.
func psqlLiteral(text string) (string, error) {
	for _, r := range text {
		if r < ' ' || r == 0x7f {
			return "", fmt.Errorf("%w: a value holds a control character", ErrUnsafeText)
		}
	}
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(text) + "'", nil
}

// markerFor returns the comment that marks an object as created by this scope's instance.
func markerFor(scope, instance string) (string, error) {
	if !markerTextPattern().MatchString(scope) || !markerTextPattern().MatchString(instance) {
		return "", fmt.Errorf("%w: the scope or instance of a marker", ErrUnsafeText)
	}
	return "stackpuddle:" + scope + "/" + instance, nil
}

// script builds one psql script. The marker travels as a psql variable, so it is quoted once, by psql.
type script struct {
	b   strings.Builder
	err error
}

func (s *script) raw(text string) { s.b.WriteString(text) }

func (s *script) line(format string, args ...any) {
	fmt.Fprintf(&s.b, format, args...)
	s.b.WriteByte('\n')
}

// ident writes the quoted names into a format with %s verbs.
func (s *script) ident(format string, names ...string) {
	quoted := make([]any, 0, len(names))
	for _, n := range names {
		q, err := managedName(n)
		if err != nil {
			s.err = errors.Join(s.err, err)
			return
		}
		quoted = append(quoted, q)
	}
	s.line(format, quoted...)
}

func (s *script) setMarker(marker string) {
	lit, err := psqlLiteral(marker)
	if err != nil {
		s.err = errors.Join(s.err, err)
		return
	}
	s.line(`\set marker %s`, lit)
}

func (s *script) setName(variable, value string) {
	lit, err := psqlLiteral(value)
	if err != nil {
		s.err = errors.Join(s.err, err)
		return
	}
	s.line(`\set %s %s`, variable, lit)
}

func (s *script) String() (string, error) { return s.b.String(), s.err }

// roleAttributes are the attributes every managed role lacks.
const roleAttributes = "NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS INHERIT"

// membershipOptions is what a runtime membership allows: privilege inheritance, no role switching and
// no membership administration.
const membershipOptions = "WITH INHERIT TRUE, SET FALSE, ADMIN FALSE"

// roleCheckSQL reports whether the role :name exists and carries which comment.
const roleCheckSQL = `SELECT json_build_array(count(*) > 0, coalesce(max(shobj_description(oid, 'pg_authid')), '')) FROM pg_roles WHERE rolname = :'name';`

// databaseCheckSQL reports whether the database :name exists, its owner and its comment.
const databaseCheckSQL = `SELECT json_build_array(count(*) > 0, coalesce(max(pg_get_userbyid(datdba)), ''), coalesce(max(shobj_description(oid, 'pg_database')), '')) FROM pg_database WHERE datname = :'name';`

// schemaCheckSQL reports whether the schema :name exists in the connected database, its owner, its
// comment and the comment of the database itself.
const schemaCheckSQL = `SELECT json_build_array(count(*) > 0, coalesce(max(pg_get_userbyid(nspowner)), ''), coalesce(max(obj_description(oid, 'pg_namespace')), ''),
  coalesce((SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = current_database()), '')) FROM pg_namespace WHERE nspname = :'name';`

// createRoleScript creates a role and its marker in one transaction, then its memberships. A user
// carries the verifier from the probe environment; the plaintext never reaches the driver.
func createRoleScript(p roleParams, marker string) (string, error) {
	name, login := p.Name, p.Login
	var s script
	s.setMarker(marker)
	if login {
		s.line(`\getenv verifier %s`, verifierEnv)
	}
	s.raw(transactionHeader)
	if login {
		s.ident("CREATE ROLE %s LOGIN "+roleAttributes+" PASSWORD :'verifier';", name)
	} else {
		s.ident("CREATE ROLE %s NOLOGIN "+roleAttributes+";", name)
	}
	s.ident("COMMENT ON ROLE %s IS :'marker';", name)
	for _, parent := range p.MemberOf {
		s.ident("GRANT %s TO %s "+membershipOptions+";", parent, name)
	}
	s.line("COMMIT;")
	return s.String()
}

// reconcileRoleScript brings a role this scope created back to its attributes and memberships. It
// never touches the password, so a repeat run rotates no credential.
func reconcileRoleScript(p roleParams) (string, error) {
	name, memberOf := p.Name, p.MemberOf
	var s script
	s.raw(transactionHeader)
	loginWord := "NOLOGIN"
	if p.Login {
		loginWord = "LOGIN"
	}
	s.ident("ALTER ROLE %s "+loginWord+" "+roleAttributes+";", name)
	for _, other := range p.Declared {
		if other == name || contains(memberOf, other) {
			continue
		}
		s.ident("REVOKE %s FROM %s;", other, name)
	}
	for _, parent := range memberOf {
		s.ident("GRANT %s TO %s "+membershipOptions+";", parent, name)
	}
	s.line("COMMIT;")
	return s.String()
}

// Privileges the module's schema allows on tables and on sequences.
func tablePrivileges() []string    { return []string{"SELECT", "INSERT", "UPDATE", "DELETE"} }
func sequencePrivileges() []string { return []string{"USAGE", "SELECT"} }

func checkPrivileges(g grant) error {
	for _, p := range g.Tables {
		if !contains(tablePrivileges(), p) {
			return fmt.Errorf("%w: a table privilege", ErrUnsafeText)
		}
	}
	for _, p := range g.Sequences {
		if !contains(sequencePrivileges(), p) {
			return fmt.Errorf("%w: a sequence privilege", ErrUnsafeText)
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func contains(list []string, item string) bool { return slices.Contains(list, item) }

// createDatabaseScript creates a database. PostgreSQL refuses CREATE DATABASE inside a transaction
// block, so the marker follows in the next script.
func createDatabaseScript(name, owner string) (string, error) {
	var s script
	s.ident("CREATE DATABASE %s OWNER %s TEMPLATE template0;", name, owner)
	return s.String()
}

// markDatabaseScript marks a database and applies its connection policy: no PUBLIC access, connect
// only for the declared roles, the connection limit and the timeout settings.
func markDatabaseScript(name, marker string, d database, owner string) (string, error) {
	var s script
	s.setMarker(marker)
	s.raw(transactionHeader)
	s.ident("COMMENT ON DATABASE %s IS :'marker';", name)
	s.ident("REVOKE ALL ON DATABASE %s FROM PUBLIC;", name)
	s.ident("GRANT CONNECT ON DATABASE %s TO %s;", name, owner)
	for _, role := range d.ConnectRoles {
		s.ident("GRANT CONNECT ON DATABASE %s TO %s;", name, role)
	}
	limit := -1
	if d.ConnectionLimit != nil {
		limit = *d.ConnectionLimit
	}
	s.ident("ALTER DATABASE %s CONNECTION LIMIT "+strconv.Itoa(limit)+";", name)
	settingsFor(&s, "ALTER DATABASE %s SET ", []string{name}, d.Settings)
	for _, user := range sortedKeys(d.UserSettings) {
		settingsFor(&s, "ALTER ROLE %s IN DATABASE %s SET ", []string{user, name}, d.UserSettings[user])
	}
	s.line("COMMIT;")
	return s.String()
}

func settingsFor(s *script, prefix string, names []string, t timeouts) {
	if t.StatementTimeoutMs != nil {
		s.ident(prefix+"statement_timeout = "+strconv.Itoa(*t.StatementTimeoutMs)+";", names...)
	}
	if t.LockTimeoutMs != nil {
		s.ident(prefix+"lock_timeout = "+strconv.Itoa(*t.LockTimeoutMs)+";", names...)
	}
}

// createSchemaScript creates a schema for the migration identity and marks it in the same transaction.
func createSchemaScript(name, owner, marker string) (string, error) {
	return schemaScript("CREATE SCHEMA %s AUTHORIZATION %s;", name, owner, marker)
}

// claimSchemaScript takes over a schema that already exists in a database this scope created. public
// exists in every new database, so it is claimed rather than created.
func claimSchemaScript(name, owner, marker string) (string, error) {
	return schemaScript("ALTER SCHEMA %s OWNER TO %s;", name, owner, marker)
}

func schemaScript(statement, name, owner, marker string) (string, error) {
	var s script
	s.setMarker(marker)
	s.raw(transactionHeader)
	s.ident(statement, name, owner)
	s.ident("COMMENT ON SCHEMA %s IS :'marker';", name)
	s.ident("REVOKE ALL ON SCHEMA %s FROM PUBLIC;", name)
	s.ident("REVOKE EXECUTE ON ALL ROUTINES IN SCHEMA %s FROM PUBLIC;", name)
	s.line("COMMIT;")
	return s.String()
}

// grantScript applies one role's grants on the existing objects of a schema.
func grantScript(schemaName, role string, g grant) (string, error) {
	if err := checkPrivileges(g); err != nil {
		return "", err
	}
	var s script
	s.raw(transactionHeader)
	s.ident("GRANT USAGE ON SCHEMA %s TO %s;", schemaName, role)
	if len(g.Tables) > 0 {
		s.ident("GRANT "+strings.Join(g.Tables, ", ")+" ON ALL TABLES IN SCHEMA %s TO %s;", schemaName, role)
	}
	if len(g.Sequences) > 0 {
		s.ident("GRANT "+strings.Join(g.Sequences, ", ")+" ON ALL SEQUENCES IN SCHEMA %s TO %s;", schemaName, role)
	}
	s.line("COMMIT;")
	return s.String()
}

// defaultPrivilegeScript applies one role's grants on the future objects the creator makes in a schema.
func defaultPrivilegeScript(schemaName, creator, role string, g grant) (string, error) {
	if err := checkPrivileges(g); err != nil {
		return "", err
	}
	var s script
	s.raw(transactionHeader)
	if len(g.Tables) > 0 {
		s.ident("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA %s GRANT "+strings.Join(g.Tables, ", ")+" ON TABLES TO %s;", creator, schemaName, role)
	}
	if len(g.Sequences) > 0 {
		s.ident("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA %s GRANT "+strings.Join(g.Sequences, ", ")+" ON SEQUENCES TO %s;", creator, schemaName, role)
	}
	s.line("COMMIT;")
	return s.String()
}

// publicRoutinesScript stops routines the creator makes from being executable by PUBLIC.
func publicRoutinesScript(creator string) (string, error) {
	var s script
	s.raw(transactionHeader)
	s.ident("ALTER DEFAULT PRIVILEGES FOR ROLE %s REVOKE EXECUTE ON ROUTINES FROM PUBLIC;", creator)
	s.line("COMMIT;")
	return s.String()
}

// lockAdminScript clears the superuser's password. The engine reaches the server over the in-container
// socket with peer authentication, so no stored admin password exists.
func lockAdminScript() string {
	return transactionHeader + "ALTER ROLE postgres PASSWORD NULL;\nCOMMIT;\n"
}

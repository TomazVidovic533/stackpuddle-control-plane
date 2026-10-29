package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
)

// ExportSQL is the Export a server workload offers consumers.
const ExportSQL = "sql"

// ErrInvalidParameters marks binding parameters the instance's declaration does not allow. The error
// names the parameter and the declared object, never a secret.
var ErrInvalidParameters = errors.New("postgres binding parameters are invalid")

// Value names of the sql Export, the contract of module.yaml.
const (
	valueDatabase = "database"
	valueUsername = "username"
	valuePassword = "password"
	valueCA       = "ca"
)

// bindParameters are the binding parameters: the login user and the database it connects to.
type bindParameters struct {
	User     string `json:"user"`
	Database string `json:"database"`
}

// Bind maps the parameters of one binding to the values of the sql Export: the database and the user
// as text, the user's password as a reference to the secret the instance already owns, and the CA the
// engine delivers. The administrator role and any user or database the options do not declare are
// refused, and a user that cannot connect to the database is refused too, so a runtime binding can
// never become an administrative one. It runs no probe.
func (Driver) Bind(_ context.Context, r module.BindRequest) (module.BindResponse, error) {
	if r.Export != ExportSQL {
		return module.BindResponse{}, fmt.Errorf("%w: the export %q is not offered", ErrInvalidParameters, r.Export)
	}
	p, err := decodeBindParameters(r.Parameters)
	if err != nil {
		return module.BindResponse{}, err
	}
	o, err := parseOptions(r.Instance.Options)
	if err != nil {
		return module.BindResponse{}, err
	}
	u, declared := o.Users[p.User]
	if p.User == superuser || !declared {
		return module.BindResponse{}, fmt.Errorf("%w: user %q is not a declared login user", ErrInvalidParameters, p.User)
	}
	db, declared := o.Databases[p.Database]
	if !declared {
		return module.BindResponse{}, fmt.Errorf("%w: database %q is not declared", ErrInvalidParameters, p.Database)
	}
	if !canConnect(p.User, u, db) {
		return module.BindResponse{}, fmt.Errorf("%w: user %q cannot connect to database %q", ErrInvalidParameters, p.User, p.Database)
	}
	return module.BindResponse{Values: []module.BoundValue{
		{Name: valueCA, Material: module.MaterialTLSCA},
		{Name: valueDatabase, Text: p.Database},
		{Name: valuePassword, SecretRef: r.Instance.Key + "/" + u.PasswordSecretRef},
		{Name: valueUsername, Text: p.User},
	}}, nil
}

// canConnect reports whether the declaration lets the user connect to the database: it owns it, or
// is, or is a member of, one of its connect roles.
func canConnect(name string, u user, db database) bool {
	if db.Owner == name || slices.Contains(db.ConnectRoles, name) {
		return true
	}
	return slices.ContainsFunc(u.MemberOf, func(role string) bool { return slices.Contains(db.ConnectRoles, role) })
}

func decodeBindParameters(raw json.RawMessage) (bindParameters, error) {
	var p bindParameters
	if len(raw) == 0 {
		return p, fmt.Errorf("%w: user and database are required", ErrInvalidParameters)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return bindParameters{}, fmt.Errorf("%w: %w", ErrInvalidParameters, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return bindParameters{}, fmt.Errorf("%w: trailing data after the parameters object", ErrInvalidParameters)
	}
	if p.User == "" || p.Database == "" {
		return bindParameters{}, fmt.Errorf("%w: user and database are required", ErrInvalidParameters)
	}
	return p, nil
}

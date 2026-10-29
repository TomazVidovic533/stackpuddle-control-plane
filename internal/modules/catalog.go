// Package modules lists the built-in module drivers the engine runs.
//
// The list is explicit: a driver is one folder under this package and one line in Catalog, and
// nothing registers itself from an init function.
package modules

import (
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/modules/postgres"
)

// Catalog returns the built-in drivers: each is one folder under this package and one line here.
func Catalog() []module.Driver {
	return []module.Driver{
		postgres.Driver{},
	}
}

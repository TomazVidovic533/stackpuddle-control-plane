package operation_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/drivers"
)

type (
	moduleProbe  = module.Probe
	moduleResult = module.ProbeResult
)

func driverCatalog() []module.Driver { return drivers.Catalog() }

func mustJSON(t *testing.T, data json.RawMessage, out any) {
	t.Helper()
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatal(err)
	}
}

// removeFile deletes a file of the environment's state folder, as a lost or damaged file would be.
func removeFile(t *testing.T, h *harness, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(h.root, "environments", "shop.local", name)); err != nil {
		t.Fatal(err)
	}
}

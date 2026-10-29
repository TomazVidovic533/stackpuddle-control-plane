package compose_test

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/compose"
)

func delivered() plan.Workload {
	w := api()
	w.Files = []plan.File{
		{Path: "/run/sp/ca.crt", Mode: 0o444, Source: plan.Source{Kind: plan.SourceMaterial, Ref: "tls/ca"}},
		{Path: "/run/sp/password", Mode: 0o400, Owner: "999:999", Source: plan.Source{Kind: plan.SourceSecret, Ref: "db/app-password"}},
		{Path: "/etc/sp/host", Mode: 0o444, Source: plan.Source{Kind: plan.SourceText, Text: "${HOST:-db}"}},
	}
	return w
}

func TestRenderFiles_EveryFileCarriesItsDeclaredOwnerAndModeAndNoValueIsRendered(t *testing.T) {
	t.Parallel()
	s := scope()
	s.Delivered = map[string]string{"db/app-password": "sv-1", "material:tls/ca": "abc123"}
	out, parsed := render(t, s, delivered())
	files := parsed.Services["api"].Secrets
	if len(files) != 3 {
		t.Fatalf("files = %v", files)
	}
	byTarget := map[string]map[string]any{}
	for _, f := range files {
		byTarget[f["target"].(string)] = f
	}
	password := byTarget["/run/sp/password"]
	if password["uid"] != "999" || password["gid"] != "999" || password["mode"] != float64(0o400) {
		t.Errorf("password file = %v, want owner 999:999 and mode 0400", password)
	}
	if ca := byTarget["/run/sp/ca.crt"]; ca["mode"] != float64(0o444) || ca["uid"] != nil {
		t.Errorf("ca file = %v, want mode 0444 and the default owner", ca)
	}
	for key, def := range parsed.Secrets {
		if _, hasFile := def["file"]; hasFile || !regexp.MustCompile(`^SP_SECRET_[0-9]+$`).MatchString(def["environment"].(string)) {
			t.Errorf("secret %s = %v: every file comes from a child variable, never a host file", key, def)
		}
	}
	for _, leak := range []string{"sv-1", "abc123", "db/app-password", "${HOST:-db}"} {
		if strings.Contains(string(out.Compose), leak) {
			t.Errorf("the rendered project holds %q", leak)
		}
	}
	if len(out.SecretVars) != 3 || out.Literals[plan.Source{Kind: plan.SourceText, Text: "${HOST:-db}"}.Key()] != "${HOST:-db}" {
		t.Errorf("variables %v, literals %v", out.SecretVars, out.Literals)
	}
}

func TestRenderFiles_ATextWithAVariableReferenceReachesTheContainerLiterally(t *testing.T) {
	t.Parallel()
	w := delivered()
	w.Env = append(w.Env, plan.EnvVar{Name: "BOUND", Value: "${VAR:-x}"})
	_, parsed := render(t, scope(), w)
	if got := parsed.Services["api"].Environment["BOUND"]; got != "$${VAR:-x}" {
		t.Errorf("a delivered value with a fallback is %q, want the dollar doubled so Compose never expands it", got)
	}
}

func TestRenderFiles_ANewVersionOfADeliveredSourceChangesTheSpecHashAndNothingElseDoes(t *testing.T) {
	t.Parallel()
	base := scope()
	base.Delivered = map[string]string{"db/app-password": "sv-1", "material:tls/ca": "abc123"}
	hash := func(s plan.ScopeNames) string {
		out, _ := render(t, s, delivered())
		return out.SpecHashes["api"]
	}
	same := scope()
	same.Delivered = map[string]string{"db/app-password": "sv-1", "material:tls/ca": "abc123"}
	if hash(base) != hash(same) {
		t.Error("equal versions gave different hashes")
	}
	rotated := scope()
	rotated.Delivered = map[string]string{"db/app-password": "sv-2", "material:tls/ca": "abc123"}
	reissued := scope()
	reissued.Delivered = map[string]string{"db/app-password": "sv-1", "material:tls/ca": "def456"}
	if hash(rotated) == hash(base) || hash(reissued) == hash(base) {
		t.Error("a rotated secret or a reissued certificate did not change the spec hash")
	}
	out, parsed := render(t, base, delivered())
	if parsed.Services["api"].Labels[plan.LabelFiles] == "" || out.SpecHashes["api"] == "" {
		t.Error("the delivered-versions label is missing")
	}
}

func TestRenderFiles_NumberingDoesNotDependOnTheOrderOfTheFiles(t *testing.T) {
	t.Parallel()
	a, _ := render(t, scope(), delivered())
	reversed := delivered()
	reversed.Files = []plan.File{reversed.Files[2], reversed.Files[1], reversed.Files[0]}
	b, _ := render(t, scope(), reversed)
	for key, variable := range a.SecretVars {
		if b.SecretVars[key] != variable {
			t.Errorf("source %s is %s in one order and %s in the other", key, variable, b.SecretVars[key])
		}
	}
}

func TestRenderFiles_AnOwnerThatIsNotUIDAndGIDIsRefused(t *testing.T) {
	t.Parallel()
	for _, owner := range []string{"999", "root:root", "999:", ":999", "1:2:3"} {
		w := delivered()
		w.Files[1].Owner = owner
		if _, err := compose.Render(scope(), plan.Topology{Workloads: []plan.Workload{w}}); !errors.Is(err, compose.ErrRender) {
			t.Errorf("owner %q: %v, want ErrRender", owner, err)
		}
	}
}

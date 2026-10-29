package operation

import (
	"context"
	"strings"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/tls"
)

// tlsPlan decides which certificates this operation issues. The CA is issued when it is missing or
// near its end; a server certificate when it is missing, its names changed, the CA changed or it is
// near its end. It returns the steps, in order, and the workloads that receive a certificate that is
// about to change, which the diff restarts. It reads only the environment's public files, so a dry run
// plans the same steps.
func (s *session) tlsPlan() (steps []plan.Step, reissued map[string]bool) {
	servers := s.tlsServers()
	if len(servers) == 0 {
		return nil, nil
	}
	now := s.e.now()
	changed := map[string]bool{}
	ca, haveCA := s.material(plan.MaterialCA)
	issueCA := !haveCA || tls.CheckCA(ca, now) == tls.Reissue
	if issueCA {
		steps = append(steps, plan.Step{Action: plan.ActionTLSIssue, Object: plan.MaterialCA})
		changed[plan.MaterialCA] = true
	}
	for _, scoped := range servers {
		ref := plan.ServerMaterial(scoped)
		cert, have := s.material(ref)
		if issueCA || !have || tls.Check(cert, ca, plan.ServerNames(s.top, scoped), now) == tls.Reissue {
			steps = append(steps, plan.Step{Action: plan.ActionTLSIssue, Object: ref})
			changed[ref] = true
		}
	}
	return steps, receiving(s.top, changed)
}

// tlsServers lists the scoped names of the selected workloads whose driver asks for a server certificate.
func (s *session) tlsServers() []string {
	var servers []string
	for _, inst := range s.top.Instances {
		driver, ok := s.e.drivers[inst.Definition]
		if !ok || !s.instanceSelected(inst) {
			continue
		}
		for _, server := range driver.Describe().ServerTLS {
			scoped := inst.Key + "--" + server.Workload
			if _, found := s.top.Find(scoped); found {
				servers = append(servers, scoped)
			}
		}
	}
	return servers
}

// receiving names the workloads that are delivered one of the changed pieces of material.
func receiving(top plan.Topology, changed map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, w := range top.Workloads {
		for _, f := range w.Files {
			if f.Source.Kind == plan.SourceMaterial && changed[f.Source.Ref] {
				out[w.Name] = true
			}
		}
	}
	return out
}

// runTLSStep issues the CA or one server certificate. The private key is stored in the sealed store
// before the certificate is written: a crash between the two leaves the stored key, and the next run
// reuses it and writes the certificate. No private key is written to any other file.
func (s *session) runTLSStep(ctx context.Context, op *state.Operation, i int) (string, error) {
	object := op.Steps[i].Object
	createdBy := op.ID.String() + "/" + op.Steps[i].ID
	now := s.e.now()
	if object == plan.MaterialCA {
		key, err := s.privateKey(ctx, plan.SecretCAKey, createdBy)
		if err != nil {
			return "", err
		}
		cert, err := tls.IssueCA(key, now)
		if err != nil {
			return "", fail(state.CodeEffectFailed, state.CategoryUnexpected, "issue the CA certificate").wrap(err)
		}
		return "issued the CA certificate", s.env.WriteMaterial(plan.MaterialFile(object), cert)
	}
	scoped := strings.TrimPrefix(object, "tls/")
	if _, ok := s.top.Find(scoped); !ok {
		return "", fail(state.CodeEffectFailed, state.CategoryUnexpected, "workload %s is not in the topology", scoped)
	}
	caCert, ok := s.material(plan.MaterialCA)
	if !ok {
		return "", fail(state.CodeEffectFailed, state.CategoryUnexpected, "the CA certificate does not exist yet")
	}
	caKey, err := s.secretValue(ctx, plan.SecretCAKey)
	if err != nil {
		return "", err
	}
	key, err := s.privateKey(ctx, plan.ServerKey(scoped), createdBy)
	if err != nil {
		return "", err
	}
	cert, err := tls.IssueServer(tls.Authority{KeyPEM: caKey.Reveal(), CertPEM: caCert}, key, plan.ServerNames(s.top, scoped), now)
	if err != nil {
		return "", fail(state.CodeEffectFailed, state.CategoryUnexpected, "issue the server certificate of %s", scoped).wrap(err)
	}
	return "issued the server certificate of " + scoped, s.env.WriteMaterial(plan.MaterialFile(object), cert)
}

// privateKey returns the key stored under ref, creating and storing one only when none exists, and
// records its version as the credential. A reissued certificate therefore keeps its key.
func (s *session) privateKey(ctx context.Context, ref, createdBy string) ([]byte, error) {
	parsed, err := secrets.ParseRef(ref)
	if err != nil {
		return nil, err
	}
	if cred, ok := s.st.Accepted.Credentials[ref]; ok {
		value, err := s.store.Resolve(ctx, parsed, cred.Version)
		return value.Reveal(), err
	}
	key, err := tls.GenerateKey()
	if err != nil {
		return nil, fail(state.CodeEffectFailed, state.CategoryUnexpected, "generate the key %s", ref).wrap(err)
	}
	version, err := s.store.CreateValueIfAbsent(ctx, parsed, secrets.NewValue(key), createdBy)
	if err != nil {
		return nil, err
	}
	value, err := s.store.Resolve(ctx, parsed, version.ID)
	if err != nil {
		return nil, err
	}
	s.recordCredential(ref, version)
	return value.Reveal(), nil
}

package provision

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Scheduling is the placement every tenant pod Bifrost provisions carries:
// Ray heads, Ray workers, Serve heads/workers and RayJob submitters alike.
// It is deployment-wide operator configuration (`serve --ray-node-selector`
// / `--ray-tolerations`), not part of a ClusterSpec, because it describes
// where THIS control plane may put tenant pods on THIS cluster — a Nebari
// cluster whose user node group carries a `hub.jupyter.org/dedicated=user`
// NoSchedule taint, say — and no tenant should have to know that, nor be
// able to opt out of it.
//
// It is not part of the owned-spec fingerprint: changing it does not make
// a running cluster drift. A re-apply does change the pod template, and
// KubeRay rolls the pods, which is the intended way to move existing
// clusters onto newly configured nodes.
type Scheduling struct {
	NodeSelector map[string]string
	Tolerations  []corev1.Toleration
}

// IsZero reports whether s places no constraint at all.
func (s Scheduling) IsZero() bool {
	return len(s.NodeSelector) == 0 && len(s.Tolerations) == 0
}

// apply stamps s onto a pod spec. Nil-safe on both sides; an empty
// Scheduling leaves the spec byte-identical (no empty map, no empty
// slice), so manifests built without scheduling are unchanged.
func (s Scheduling) apply(spec *corev1.PodSpec) {
	if spec == nil || s.IsZero() {
		return
	}
	if len(s.NodeSelector) > 0 {
		if spec.NodeSelector == nil {
			spec.NodeSelector = make(map[string]string, len(s.NodeSelector))
		}
		for k, v := range s.NodeSelector {
			spec.NodeSelector[k] = v
		}
	}
	if len(s.Tolerations) > 0 {
		spec.Tolerations = append(spec.Tolerations, s.Tolerations...)
	}
}

// ParseNodeSelector parses the `--ray-node-selector` flag: comma-separated
// `key=value` pairs, blanks ignored. "" yields nil.
func ParseNodeSelector(s string) (map[string]string, error) {
	var out map[string]string
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" {
			return nil, fmt.Errorf("node selector %q: want key=value", pair)
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out, nil
}

// ParseTolerations parses the `--ray-tolerations` flag: a JSON array of
// Kubernetes tolerations, the same shape as a pod's `spec.tolerations`
// (key, operator, value, effect, tolerationSeconds). "" yields nil.
func ParseTolerations(s string) ([]corev1.Toleration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []corev1.Toleration
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("tolerations: want a JSON array of tolerations: %w", err)
	}
	for i, t := range out {
		switch t.Operator {
		case "", corev1.TolerationOpEqual, corev1.TolerationOpExists:
		default:
			return nil, fmt.Errorf("tolerations[%d]: unknown operator %q", i, t.Operator)
		}
		switch t.Effect {
		case "", corev1.TaintEffectNoSchedule, corev1.TaintEffectPreferNoSchedule, corev1.TaintEffectNoExecute:
		default:
			return nil, fmt.Errorf("tolerations[%d]: unknown effect %q", i, t.Effect)
		}
		if t.Key == "" && t.Operator != corev1.TolerationOpExists {
			return nil, fmt.Errorf("tolerations[%d]: an empty key requires operator Exists", i)
		}
	}
	return out, nil
}

// String renders s for a log line: sorted `k=v` pairs and one token per
// toleration, so operators can read back what the control plane applies.
func (s Scheduling) String() string {
	keys := make([]string, 0, len(s.NodeSelector))
	for k := range s.NodeSelector {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)+len(s.Tolerations))
	for _, k := range keys {
		parts = append(parts, k+"="+s.NodeSelector[k])
	}
	for _, t := range s.Tolerations {
		tok := "tolerate:" + t.Key
		if t.Value != "" {
			tok += "=" + t.Value
		}
		if t.Effect != "" {
			tok += ":" + string(t.Effect)
		}
		parts = append(parts, tok)
	}
	return strings.Join(parts, " ")
}

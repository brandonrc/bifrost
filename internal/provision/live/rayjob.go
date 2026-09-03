package live

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"

	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

// JobClient is [Client]'s [provision.JobProvisioner] façade over the same
// connection (requirement 5) — a distinct type for the same reason as
// [ServiceClient]: the List-shaped methods of the three provisioner
// interfaces cannot share one receiver. Thin by the package's rule: every
// method is I/O plus a call into internal/provision's pure translators
// (RayJobFor, ObservedJobFromRayJob).
type JobClient struct {
	*Client
}

var _ provision.JobProvisioner = (*JobClient)(nil)

// NewJobClient returns c's JobProvisioner façade.
func NewJobClient(c *Client) *JobClient { return &JobClient{c} }

// ApplyJob server-side-applies the RayJob for id. The per-cluster allow
// policy goes in first: the RayCluster KubeRay creates for the job
// inherits the RayJob's pod-template labels (ClusterIDLabel=id), so the
// same allow that isolates a managed cluster isolates the job's cluster
// and its submitter pod.
func (j *JobClient) ApplyJob(ctx context.Context, id core.ClusterId, spec *core.RayJobSpec, generation uint64, queue *provision.QueueAssignment) error {
	if err := j.ensureClusterAllow(ctx, string(id), spec.Owner); err != nil {
		return err
	}
	manifest, err := provision.RayJobFor(id, spec, generation, queue)
	if err != nil {
		return provision.ProvisionError{Kind: provision.ProvisionErrBackend, Message: err.Error()}
	}
	manifest.Namespace = j.namespace
	return wrapErr(applySSA(ctx, j.c, manifest, client.FieldOwner(provision.FieldManager), client.ForceOwnership))
}

// ObserveJob reads a RayJob's status. A missing RayJob is
// [provision.ProvisionErrNotFound].
func (j *JobClient) ObserveJob(ctx context.Context, id core.ClusterId) (provision.ObservedJob, error) {
	var rj rayv1.RayJob
	if err := j.c.Get(ctx, client.ObjectKey{Namespace: j.namespace, Name: string(id)}, &rj); err != nil {
		if apierrors.IsNotFound(err) {
			return provision.ObservedJob{}, provision.ProvisionError{Kind: provision.ProvisionErrNotFound, ClusterID: id}
		}
		return provision.ObservedJob{}, wrapErr(err)
	}
	return provision.ObservedJobFromRayJob(&rj), nil
}

// DeleteJob deletes the RayJob (KubeRay deletes the RayCluster it owns)
// and reaps the per-cluster allow policy. Idempotent: already-gone is
// success.
func (j *JobClient) DeleteJob(ctx context.Context, id core.ClusterId) error {
	rj := &rayv1.RayJob{ObjectMeta: metav1.ObjectMeta{Name: string(id), Namespace: j.namespace}}
	if err := j.c.Delete(ctx, rj); err != nil && !apierrors.IsNotFound(err) {
		return wrapErr(err)
	}
	return j.deleteClusterAllow(ctx, string(id))
}

// ListJobs returns every RayJob this field manager owns in the namespace.
func (j *JobClient) ListJobs(ctx context.Context) ([]provision.ObservedJob, error) {
	var list rayv1.RayJobList
	if err := j.c.List(ctx, &list, client.InNamespace(j.namespace), client.MatchingLabels{provision.ManagedByLabel: provision.FieldManager}); err != nil {
		return nil, wrapErr(err)
	}
	out := make([]provision.ObservedJob, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, provision.ObservedJobFromRayJob(&list.Items[i]))
	}
	return out, nil
}

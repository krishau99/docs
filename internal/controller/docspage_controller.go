package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/krishau99/docs/api/v1alpha1"
)

const (
	conditionTypeReady = "Ready"
	finalizerName      = "docspage/finalizer"

	// defaultServingPort mirrors the CRD default for spec.serving.port.
	defaultServingPort = 8080
)

// Reasons carried on the Ready condition. Each one says which observation
// produced it, so "why is this not ready" is answerable from the condition
// alone without going to the Deployment or the pods.
const (
	// reasonDeploymentAvailable means the Deployment reports its replicas as
	// available. This is the only reason that accompanies Ready=True.
	reasonDeploymentAvailable = "DeploymentAvailable"
	// reasonDeploymentProgressing means the Deployment exists but the cluster
	// has not caught up with its current spec yet.
	reasonDeploymentProgressing = "DeploymentProgressing"
	// reasonDeploymentUnavailable means the Deployment has been observed and
	// does not have the replicas it wants.
	reasonDeploymentUnavailable = "DeploymentUnavailable"
	// reasonReconcileError means the reconciler itself failed before it could
	// observe anything.
	reasonReconcileError = "ReconcileError"
	// reasonUnknown is a fallback so the condition never carries an empty
	// reason, which the API server rejects.
	reasonUnknown = "Unknown"
)

// DocsPageReconciler reconciles a DocsPage object.
type DocsPageReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// DefaultCACert is an optional cluster-wide CA certificate (PEM) used for
	// external HTTP calls (Gitea, registries) when no per-resource CA secret is configured.
	DefaultCACert []byte
}

// observedStatus is what a reconcile pass learned about the world. It is
// deliberately not written by the code that creates the Deployment and Service:
// a successful write says the objects were accepted, not that documentation is
// being served, and conflating the two is what made status.ready meaningless.
type observedStatus struct {
	currentSHA string
	ready      bool
	reason     string
	message    string
}

// +kubebuilder:rbac:groups=docspage,resources=docspages,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=docspage,resources=docspages/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=docspage,resources=docspages/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch;create;update;patch

// Reconcile is the main reconciliation function for DocsPage resources.
func (r *DocsPageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the DocsPage instance
	dp := &v1alpha1.DocsPage{}
	if err := r.Get(ctx, req.NamespacedName, dp); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !dp.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, dp)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(dp, finalizerName) {
		controllerutil.AddFinalizer(dp, finalizerName)
		if err := r.Update(ctx, dp); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Reconcile based on mode
	var obs observedStatus
	var err error
	switch dp.Spec.Mode {
	case v1alpha1.DocsPageModeBuild:
		obs, err = r.reconcileBuildMode(ctx, dp)
	case v1alpha1.DocsPageModePrebuilt:
		obs, err = r.reconcilePrebuiltMode(ctx, dp)
	default:
		err = fmt.Errorf("unknown mode %q", dp.Spec.Mode)
	}

	if err != nil {
		logger.Error(err, "reconciliation failed")
		obs = observedStatus{
			currentSHA: dp.Status.CurrentSHA,
			ready:      false,
			reason:     reasonReconcileError,
			message:    err.Error(),
		}
	}

	// One status write per pass, whatever happened above.
	if statusErr := r.applyStatus(ctx, dp, obs); statusErr != nil {
		if err == nil {
			return ctrl.Result{}, fmt.Errorf("updating status: %w", statusErr)
		}
		// The reconcile error is the more useful one to surface and requeue on.
		logger.Error(statusErr, "failed to record reconcile error in status")
	}

	// A non-nil error requeues with exponential backoff and makes
	// controller-runtime discard the Result, so never return both.
	if err != nil {
		return ctrl.Result{}, err
	}

	// For build mode, schedule the next poll. Note this is also the path taken
	// when the Deployment is unhealthy: an unready Deployment is an observation,
	// not a reconcile failure, so it must not be pushed into error backoff where
	// status would stop being refreshed.
	if dp.Spec.Mode == v1alpha1.DocsPageModeBuild {
		return ctrl.Result{RequeueAfter: parsePollInterval(dp.Spec.PollInterval)}, nil
	}

	return ctrl.Result{}, nil
}

// reconcileBuildMode handles reconciliation for mode=build.
func (r *DocsPageReconciler) reconcileBuildMode(ctx context.Context, dp *v1alpha1.DocsPage) (observedStatus, error) {
	logger := log.FromContext(ctx)

	if dp.Spec.Repo == nil || dp.Spec.Repo.URL == "" {
		return observedStatus{}, fmt.Errorf("mode is 'build' but spec.repo.url is not set")
	}

	// The operator does not configure the web server, so an unset serving image
	// has no safe default: anything it picked would listen wherever that image
	// happens to listen, not on spec.serving.port. Fail loudly instead.
	if dp.Spec.Serving.Image == "" {
		return observedStatus{}, fmt.Errorf(
			"mode is 'build' but spec.serving.image is not set; "+
				"supply an image that listens on port %d and serves %s",
			servingPort(dp), documentRoot(dp))
	}

	registryURL := ""
	if dp.Spec.Registry != nil {
		registryURL = dp.Spec.Registry.URL
	}

	// Get the latest SHA from the git repository
	latestSHA, err := r.fetchLatestSHA(ctx, dp)
	if err != nil {
		// Don't fail the entire reconciliation on poll error; log and continue
		logger.Error(err, "failed to poll git repository for latest SHA")
		latestSHA = dp.Status.CurrentSHA
	}

	// Reconcile Deployment
	if err := r.reconcileDeployment(ctx, dp, registryURL, latestSHA); err != nil {
		return observedStatus{}, fmt.Errorf("reconciling Deployment: %w", err)
	}

	// Reconcile Service
	if err := r.reconcileService(ctx, dp); err != nil {
		return observedStatus{}, fmt.Errorf("reconciling Service: %w", err)
	}

	obs, err := r.observeDeployment(ctx, dp)
	if err != nil {
		return observedStatus{}, fmt.Errorf("observing Deployment: %w", err)
	}
	obs.currentSHA = latestSHA

	return obs, nil
}

// reconcilePrebuiltMode handles reconciliation for mode=prebuilt.
func (r *DocsPageReconciler) reconcilePrebuiltMode(ctx context.Context, dp *v1alpha1.DocsPage) (observedStatus, error) {
	if dp.Spec.Image == "" {
		return observedStatus{}, fmt.Errorf(
			"mode is 'prebuilt' but spec.image is not set; "+
				"supply an image that listens on port %d", servingPort(dp))
	}

	registryURL := ""
	if dp.Spec.Registry != nil {
		registryURL = dp.Spec.Registry.URL
	}

	// Reconcile Deployment
	if err := r.reconcileDeployment(ctx, dp, registryURL, ""); err != nil {
		return observedStatus{}, fmt.Errorf("reconciling Deployment: %w", err)
	}

	// Reconcile Service
	if err := r.reconcileService(ctx, dp); err != nil {
		return observedStatus{}, fmt.Errorf("reconciling Service: %w", err)
	}

	// There is no repository to poll in prebuilt mode, so currentSHA and
	// lastSyncTime stay unset rather than carrying a meaningless timestamp.
	return r.observeDeployment(ctx, dp)
}

// observeDeployment reads the Deployment back and derives readiness from what
// the cluster reports about it. Nothing here infers readiness from the fact
// that a write succeeded.
func (r *DocsPageReconciler) observeDeployment(ctx context.Context, dp *v1alpha1.DocsPage) (observedStatus, error) {
	deploy := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: dp.Name, Namespace: dp.Namespace}, deploy)
	if errors.IsNotFound(err) {
		// Reconcile only just created it, or the cache has not caught up.
		return observedStatus{
			ready:   false,
			reason:  reasonDeploymentProgressing,
			message: "Deployment has not been observed yet",
		}, nil
	}
	if err != nil {
		return observedStatus{}, err
	}

	wanted := int32(1)
	if deploy.Spec.Replicas != nil {
		wanted = *deploy.Spec.Replicas
	}
	available := deploy.Status.AvailableReplicas

	switch {
	case deploy.Status.ObservedGeneration < deploy.Generation:
		return observedStatus{
			ready:   false,
			reason:  reasonDeploymentProgressing,
			message: "Deployment was updated and the change has not been rolled out yet",
		}, nil

	case wanted == 0:
		return observedStatus{
			ready:   false,
			reason:  reasonDeploymentUnavailable,
			message: "spec.serving.replicas is 0, so nothing is serving documentation",
		}, nil

	case available < wanted:
		message := fmt.Sprintf("%d of %d replicas available", available, wanted)
		if detail := deploymentProblem(deploy); detail != "" {
			message = fmt.Sprintf("%s: %s", message, detail)
		}
		return observedStatus{
			ready:   false,
			reason:  reasonDeploymentUnavailable,
			message: message,
		}, nil

	default:
		return observedStatus{
			ready:   true,
			reason:  reasonDeploymentAvailable,
			message: fmt.Sprintf("%d of %d replicas available and serving documentation", available, wanted),
		}, nil
	}
}

// deploymentProblem returns the Deployment's own explanation for not being
// available, if it has one, so the DocsPage condition does not just report a
// replica count the user then has to go and interpret.
func deploymentProblem(deploy *appsv1.Deployment) string {
	for _, c := range deploy.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse && c.Message != "" {
			return c.Message
		}
	}
	for _, c := range deploy.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionFalse && c.Message != "" {
			return c.Message
		}
	}
	return ""
}

// applyStatus writes the observed status in a single merge patch.
//
// Two things matter here. A merge patch carries no resourceVersion
// precondition, so it cannot fail with "the object has been modified" the way
// the previous read-modify-Update pair did — that conflict used to abort the
// whole reconcile and leave the last written status frozen in place. And when
// nothing has changed the patch is skipped entirely, so a steady state produces
// no writes, and therefore no watch events that would reconcile the resource
// again purely because it had just been written.
func (r *DocsPageReconciler) applyStatus(ctx context.Context, dp *v1alpha1.DocsPage, obs observedStatus) error {
	base := dp.DeepCopy()

	// lastSyncTime marks when the observed commit last changed, not when the
	// operator last ran. Refreshing it every pass would make every reconcile a
	// write, which is the churn this function exists to avoid.
	if obs.currentSHA != "" && obs.currentSHA != dp.Status.CurrentSHA {
		now := metav1.Now()
		dp.Status.CurrentSHA = obs.currentSHA
		dp.Status.LastSyncTime = &now
	}
	dp.Status.Ready = obs.ready

	conditionStatus := metav1.ConditionFalse
	if obs.ready {
		conditionStatus = metav1.ConditionTrue
	}
	reason := obs.reason
	if reason == "" {
		reason = reasonUnknown
	}
	setCondition(dp, conditionTypeReady, conditionStatus, reason, obs.message)

	if reflect.DeepEqual(base.Status, dp.Status) {
		return nil
	}

	return r.Status().Patch(ctx, dp, client.MergeFrom(base))
}

// setCondition updates a condition on the DocsPage status in memory. It does
// not write: the caller decides when and how the status is persisted.
func setCondition(dp *v1alpha1.DocsPage, condType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: dp.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}

	for i, c := range dp.Status.Conditions {
		if c.Type != condType {
			continue
		}
		// LastTransitionTime means what it says: only move it when the status
		// actually flips. Refreshing it every pass would defeat the no-op check
		// in applyStatus.
		if c.Status != status {
			dp.Status.Conditions[i] = condition
		} else {
			dp.Status.Conditions[i].Message = message
			dp.Status.Conditions[i].Reason = reason
			dp.Status.Conditions[i].ObservedGeneration = dp.Generation
		}
		return
	}

	dp.Status.Conditions = append(dp.Status.Conditions, condition)
}

// reconcileDeployment creates or updates the Deployment for a DocsPage.
// If the SHA has changed (build mode), it triggers a rolling restart by
// updating an annotation on the pod template.
func (r *DocsPageReconciler) reconcileDeployment(ctx context.Context, dp *v1alpha1.DocsPage, registryURL, currentSHA string) error {
	logger := log.FromContext(ctx)

	desired := buildDeployment(dp, registryURL, currentSHA)
	if err := controllerutil.SetControllerReference(dp, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting controller reference: %w", err)
	}

	existing := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: dp.Name, Namespace: dp.Namespace}, existing)
	if errors.IsNotFound(err) {
		logger.Info("Creating Deployment", "name", dp.Name)
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// Check if a new SHA requires a rolling restart
	existingSHA := existing.Spec.Template.Annotations[annotationCurrentSHA]
	if currentSHA != "" && currentSHA != existingSHA {
		logger.Info("New commit SHA detected, triggering rolling restart",
			"oldSHA", existingSHA, "newSHA", currentSHA)
		// Update restart annotation and SHA annotation
		if existing.Spec.Template.Annotations == nil {
			existing.Spec.Template.Annotations = make(map[string]string)
		}
		existing.Spec.Template.Annotations[annotationRestartedAt] = time.Now().UTC().Format(time.RFC3339)
		existing.Spec.Template.Annotations[annotationCurrentSHA] = currentSHA
	}

	// Update the spec to reflect any changes
	existing.Spec.Replicas = desired.Spec.Replicas
	existing.Spec.Template.Spec = desired.Spec.Template.Spec

	return r.Update(ctx, existing)
}

// reconcileService creates or updates the Service for a DocsPage.
func (r *DocsPageReconciler) reconcileService(ctx context.Context, dp *v1alpha1.DocsPage) error {
	logger := log.FromContext(ctx)

	desired := buildService(dp)
	if err := controllerutil.SetControllerReference(dp, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting controller reference: %w", err)
	}

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: dp.Name, Namespace: dp.Namespace}, existing)
	if errors.IsNotFound(err) {
		logger.Info("Creating Service", "name", dp.Name)
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// Update the service spec, preserving ClusterIP
	existing.Spec.Ports = desired.Spec.Ports
	existing.Spec.Selector = desired.Spec.Selector

	return r.Update(ctx, existing)
}

// reconcileDelete handles cleanup when a DocsPage is being deleted.
// Child resources (Deployment, Service) are automatically deleted by Kubernetes
// garbage collection because they have the DocsPage as their owner reference.
func (r *DocsPageReconciler) reconcileDelete(ctx context.Context, dp *v1alpha1.DocsPage) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(dp, finalizerName) {
		controllerutil.RemoveFinalizer(dp, finalizerName)
		if err := r.Update(ctx, dp); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// fetchLatestSHA retrieves the latest commit SHA for the DocsPage's repository.
// It fetches optional CA certificate data from the referenced secret.
func (r *DocsPageReconciler) fetchLatestSHA(ctx context.Context, dp *v1alpha1.DocsPage) (string, error) {
	if dp.Spec.Repo == nil {
		return "", fmt.Errorf("spec.repo is not set")
	}

	// Load optional CA certificate; fall back to the cluster-wide default.
	var caPEM []byte
	if dp.Spec.TLS != nil && dp.Spec.TLS.CASecret != "" {
		caSecret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: dp.Spec.TLS.CASecret, Namespace: dp.Namespace}, caSecret); err != nil {
			return "", fmt.Errorf("fetching CA secret %q: %w", dp.Spec.TLS.CASecret, err)
		}
		caPEM = caSecret.Data["ca.crt"]
	} else if len(r.DefaultCACert) > 0 {
		caPEM = r.DefaultCACert
	}

	// Load optional git credentials
	var creds *GitCredentials
	if dp.Spec.Repo.CredentialsSecret != "" {
		credSecret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: dp.Spec.Repo.CredentialsSecret, Namespace: dp.Namespace}, credSecret); err != nil {
			return "", fmt.Errorf("fetching credentials secret %q: %w", dp.Spec.Repo.CredentialsSecret, err)
		}
		creds = &GitCredentials{
			Username: string(credSecret.Data["username"]),
			Password: string(credSecret.Data["password"]),
		}
	}

	poller := NewGitPoller(caPEM)

	branch := dp.Spec.Repo.Branch
	if branch == "" {
		branch = "main"
	}

	return poller.GetLatestSHA(ctx, dp.Spec.Repo.URL, branch, creds)
}

// servingPort returns the configured serving port, or the CRD default when the
// resource predates that default being applied.
func servingPort(dp *v1alpha1.DocsPage) int32 {
	if dp.Spec.Serving.Port != 0 {
		return dp.Spec.Serving.Port
	}
	return defaultServingPort
}

// documentRoot returns the configured document root, or the httpd-shaped default.
func documentRoot(dp *v1alpha1.DocsPage) string {
	if dp.Spec.Serving.DocumentRoot != "" {
		return dp.Spec.Serving.DocumentRoot
	}
	return defaultDocumentRoot
}

// parsePollInterval parses a duration string like "5m" or "1h".
// Falls back to 5 minutes on parse error.
func parsePollInterval(interval string) time.Duration {
	if interval == "" {
		return 5 * time.Minute
	}
	d, err := time.ParseDuration(interval)
	if err != nil {
		return 5 * time.Minute
	}
	return d
}

// SetupWithManager registers the DocsPageReconciler with the controller-runtime Manager.
func (r *DocsPageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.DocsPage{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

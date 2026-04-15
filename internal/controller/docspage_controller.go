package controller

import (
	"context"
	"fmt"
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
	finalizerName      = "zensical.io/finalizer"
)

// DocsPageReconciler reconciles a DocsPage object.
type DocsPageReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=zensical.io,resources=docspages,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=zensical.io,resources=docspages/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=zensical.io,resources=docspages/finalizers,verbs=update
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
	var err error
	switch dp.Spec.Mode {
	case v1alpha1.DocsPageModeBuild:
		err = r.reconcileBuildMode(ctx, dp)
	case v1alpha1.DocsPageModePrebuilt:
		err = r.reconcilePrebuiltMode(ctx, dp)
	default:
		err = fmt.Errorf("unknown mode %q", dp.Spec.Mode)
	}

	if err != nil {
		logger.Error(err, "reconciliation failed")
		if statusErr := r.setCondition(ctx, dp, conditionTypeReady, metav1.ConditionFalse, "ReconcileError", err.Error()); statusErr != nil {
			logger.Error(statusErr, "failed to update status condition")
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// For build mode, schedule the next poll
	if dp.Spec.Mode == v1alpha1.DocsPageModeBuild {
		interval := parsePollInterval(dp.Spec.PollInterval)
		return ctrl.Result{RequeueAfter: interval}, nil
	}

	return ctrl.Result{}, nil
}

// reconcileBuildMode handles reconciliation for mode=build.
func (r *DocsPageReconciler) reconcileBuildMode(ctx context.Context, dp *v1alpha1.DocsPage) error {
	logger := log.FromContext(ctx)

	if dp.Spec.Repo == nil || dp.Spec.Repo.URL == "" {
		return fmt.Errorf("mode is 'build' but spec.repo.url is not set")
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
		return fmt.Errorf("reconciling Deployment: %w", err)
	}

	// Reconcile Service
	if err := r.reconcileService(ctx, dp); err != nil {
		return fmt.Errorf("reconciling Service: %w", err)
	}

	// Update status
	now := metav1.Now()
	dp.Status.CurrentSHA = latestSHA
	dp.Status.LastSyncTime = &now
	dp.Status.Ready = true

	if err := r.setCondition(ctx, dp, conditionTypeReady, metav1.ConditionTrue, "DeploymentReady", "Documentation is being served"); err != nil {
		return fmt.Errorf("updating status: %w", err)
	}

	return nil
}

// reconcilePrebuiltMode handles reconciliation for mode=prebuilt.
func (r *DocsPageReconciler) reconcilePrebuiltMode(ctx context.Context, dp *v1alpha1.DocsPage) error {
	registryURL := ""
	if dp.Spec.Registry != nil {
		registryURL = dp.Spec.Registry.URL
	}

	// Reconcile Deployment
	if err := r.reconcileDeployment(ctx, dp, registryURL, ""); err != nil {
		return fmt.Errorf("reconciling Deployment: %w", err)
	}

	// Reconcile Service
	if err := r.reconcileService(ctx, dp); err != nil {
		return fmt.Errorf("reconciling Service: %w", err)
	}

	// Update status
	now := metav1.Now()
	dp.Status.LastSyncTime = &now
	dp.Status.Ready = true

	if err := r.setCondition(ctx, dp, conditionTypeReady, metav1.ConditionTrue, "DeploymentReady", "Documentation is being served"); err != nil {
		return fmt.Errorf("updating status: %w", err)
	}

	return nil
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

	// Load optional CA certificate
	var caPEM []byte
	if dp.Spec.TLS != nil && dp.Spec.TLS.CASecret != "" {
		caSecret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: dp.Spec.TLS.CASecret, Namespace: dp.Namespace}, caSecret); err != nil {
			return "", fmt.Errorf("fetching CA secret %q: %w", dp.Spec.TLS.CASecret, err)
		}
		caPEM = caSecret.Data["ca.crt"]
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

// setCondition updates a specific condition on the DocsPage status.
func (r *DocsPageReconciler) setCondition(ctx context.Context, dp *v1alpha1.DocsPage, condType string, status metav1.ConditionStatus, reason, message string) error {
	now := metav1.Now()
	condition := metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: dp.Generation,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	}

	// Find existing condition and update, or append
	found := false
	for i, c := range dp.Status.Conditions {
		if c.Type == condType {
			// Only update LastTransitionTime if status changed
			if c.Status != status {
				dp.Status.Conditions[i] = condition
			} else {
				dp.Status.Conditions[i].Message = message
				dp.Status.Conditions[i].Reason = reason
				dp.Status.Conditions[i].ObservedGeneration = dp.Generation
			}
			found = true
			break
		}
	}
	if !found {
		dp.Status.Conditions = append(dp.Status.Conditions, condition)
	}

	return r.Status().Update(ctx, dp)
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

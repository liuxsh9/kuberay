package ray

import (
	"context"
	errstd "errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/lru"

	"github.com/ray-project/kuberay/ray-operator/controllers/ray/common"
	"github.com/ray-project/kuberay/ray-operator/pkg/features"

	cmap "github.com/orcaman/concurrent-map/v2"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ray-project/kuberay/ray-operator/controllers/ray/utils"

	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
)

const (
	ServiceDefaultRequeueDuration   = 2 * time.Second
	RayClusterDeletionDelayDuration = 60 * time.Second
	ENABLE_ZERO_DOWNTIME            = "ENABLE_ZERO_DOWNTIME"
)

// RayServiceReconciler reconciles a RayService object
type RayServiceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Currently, the Ray dashboard doesn't cache the Serve application config.
	// To avoid reapplying the same config repeatedly, cache the config in this map.
	// Cache key is the combination of RayService namespace and name.
	// Cache value is map of RayCluster name to Serve application config.
	ServeConfigs                 *lru.Cache
	RayClusterDeletionTimestamps cmap.ConcurrentMap[string, time.Time]
	dashboardClientFunc          func() utils.RayDashboardClientInterface
	httpProxyClientFunc          func() utils.RayHttpProxyClientInterface
}

// NewRayServiceReconciler returns a new reconcile.Reconciler
func NewRayServiceReconciler(_ context.Context, mgr manager.Manager, provider utils.ClientProvider) *RayServiceReconciler {
	dashboardClientFunc := provider.GetDashboardClient(mgr)
	httpProxyClientFunc := provider.GetHttpProxyClient(mgr)
	return &RayServiceReconciler{
		Client:                       mgr.GetClient(),
		Scheme:                       mgr.GetScheme(),
		Recorder:                     mgr.GetEventRecorderFor("rayservice-controller"),
		ServeConfigs:                 lru.New(utils.ServeConfigLRUSize),
		RayClusterDeletionTimestamps: cmap.New[time.Time](),

		dashboardClientFunc: dashboardClientFunc,
		httpProxyClientFunc: httpProxyClientFunc,
	}
}

// +kubebuilder:rbac:groups=ray.io,resources=rayservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ray.io,resources=rayservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ray.io,resources=rayservices/finalizers,verbs=update
// +kubebuilder:rbac:groups=ray.io,resources=rayclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ray.io,resources=rayclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ray.io,resources=rayclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/proxy,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=services/proxy,verbs=get;update;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;create;update
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=roles,verbs=get;list;watch;create;delete;update
// +kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=rolebindings,verbs=get;list;watch;create;delete

// [WARNING]: There MUST be a newline after kubebuilder markers.
// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// This the top level reconciliation flow for RayService.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.2/pkg/reconcile
func (r *RayServiceReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	isReady := false

	var rayServiceInstance *rayv1.RayService
	var err error

	// Resolve the CR from request.
	if rayServiceInstance, err = r.getRayServiceInstance(ctx, request); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	originalRayServiceInstance := rayServiceInstance.DeepCopy()

	if err := validateRayServiceSpec(rayServiceInstance); err != nil {
		logger.Error(err, "The RayService spec is invalid")
		r.Recorder.Eventf(rayServiceInstance, corev1.EventTypeWarning, string(utils.InvalidRayServiceSpec),
			"The RayService spec is invalid %s/%s: %v", rayServiceInstance.Namespace, rayServiceInstance.Name, err)
		return ctrl.Result{RequeueAfter: ServiceDefaultRequeueDuration}, err
	}

	r.cleanUpServeConfigCache(ctx, rayServiceInstance)

	// TODO (kevin85421): ObservedGeneration should be used to determine whether to update this CR or not.
	rayServiceInstance.Status.ObservedGeneration = rayServiceInstance.ObjectMeta.Generation

	// Find active and pending ray cluster objects given current service name.
	var activeRayClusterInstance *rayv1.RayCluster
	var pendingRayClusterInstance *rayv1.RayCluster
	if activeRayClusterInstance, pendingRayClusterInstance, err = r.reconcileRayCluster(ctx, rayServiceInstance); err != nil {
		return ctrl.Result{RequeueAfter: ServiceDefaultRequeueDuration}, client.IgnoreNotFound(err)
	}

	// Check if we need to create pending RayCluster.
	if rayServiceInstance.Status.PendingServiceStatus.RayClusterName != "" && pendingRayClusterInstance == nil {
		// Update RayService Status since reconcileRayCluster may mark RayCluster restart.
		if errStatus := r.Status().Update(ctx, rayServiceInstance); errStatus != nil {
			logger.Error(errStatus, "Fail to update status of RayService after RayCluster changes", "rayServiceInstance", rayServiceInstance)
			return ctrl.Result{RequeueAfter: ServiceDefaultRequeueDuration}, nil
		}
		logger.Info("Done reconcileRayCluster update status, enter next loop to create new ray cluster.")
		return ctrl.Result{RequeueAfter: ServiceDefaultRequeueDuration}, nil
	}

	/*
		Update ray cluster for 4 possible situations.
		If a ray cluster does not exist, clear its status.
		If only one ray cluster exists, do serve deployment if needed and check dashboard, serve deployment health.
		If both ray clusters exist, update active cluster status and do the pending cluster deployment and health check.
	*/
	if activeRayClusterInstance != nil && pendingRayClusterInstance == nil {
		logger.Info("Reconciling the Serve component. Only the active Ray cluster exists.")
		rayServiceInstance.Status.PendingServiceStatus = rayv1.RayServiceStatus{}
		if isReady, err = r.reconcileServe(ctx, rayServiceInstance, activeRayClusterInstance, true); err != nil {
			logger.Error(err, "Fail to reconcileServe.")
			return ctrl.Result{RequeueAfter: ServiceDefaultRequeueDuration}, nil
		}
	} else if activeRayClusterInstance != nil && pendingRayClusterInstance != nil {
		logger.Info("Reconciling the Serve component. Active and pending Ray clusters exist.")
		// TODO (kevin85421): This can most likely be removed.
		if err = r.updateStatusForActiveCluster(ctx, rayServiceInstance, activeRayClusterInstance); err != nil {
			logger.Error(err, "Failed to update active Ray cluster's status.")
		}

		if isReady, err = r.reconcileServe(ctx, rayServiceInstance, pendingRayClusterInstance, false); err != nil {
			logger.Error(err, "Fail to reconcileServe.")
			return ctrl.Result{RequeueAfter: ServiceDefaultRequeueDuration}, nil
		}
	} else if activeRayClusterInstance == nil && pendingRayClusterInstance != nil {
		rayServiceInstance.Status.ActiveServiceStatus = rayv1.RayServiceStatus{}
		if isReady, err = r.reconcileServe(ctx, rayServiceInstance, pendingRayClusterInstance, false); err != nil {
			logger.Error(err, "Fail to reconcileServe.")
			return ctrl.Result{RequeueAfter: ServiceDefaultRequeueDuration}, nil
		}
	} else {
		logger.Info("Reconciling the Serve component. No Ray cluster exists.")
		rayServiceInstance.Status.ActiveServiceStatus = rayv1.RayServiceStatus{}
		rayServiceInstance.Status.PendingServiceStatus = rayv1.RayServiceStatus{}
	}

	if !isReady {
		logger.Info("Ray Serve applications are not ready to serve requests")
		if !isEagerExposesServicesEnabled() {
			return ctrl.Result{RequeueAfter: ServiceDefaultRequeueDuration}, nil
		}
	}

	// Get the ready Ray cluster instance for service update.
	var rayClusterInstance *rayv1.RayCluster
	if pendingRayClusterInstance != nil {
		rayClusterInstance = pendingRayClusterInstance
		logger.Info("Reconciling the service resources " +
			"on the pending Ray cluster.")
	} else if activeRayClusterInstance != nil {
		rayClusterInstance = activeRayClusterInstance
		logger.Info("Reconciling the service resources " +
			"on the active Ray cluster. No pending Ray cluster found.")
	} else {
		rayClusterInstance = nil
		logger.Info("No Ray cluster found. Skipping service reconciliation.")
	}

	if rayClusterInstance != nil {
		if err := r.reconcileServices(ctx, rayServiceInstance, rayClusterInstance, utils.HeadService); err != nil {
			return ctrl.Result{RequeueAfter: ServiceDefaultRequeueDuration}, err
		}
		if err := r.labelHeadPodForServeStatus(ctx, rayClusterInstance, rayServiceInstance.Spec.ExcludeHeadPodFromServeSvc); err != nil {
			return ctrl.Result{RequeueAfter: ServiceDefaultRequeueDuration}, err
		}
		if err := r.reconcileServices(ctx, rayServiceInstance, rayClusterInstance, utils.ServingService); err != nil {
			return ctrl.Result{RequeueAfter: ServiceDefaultRequeueDuration}, err
		}
	}

	if !isReady {
		return ctrl.Result{RequeueAfter: ServiceDefaultRequeueDuration}, nil
	}

	if err := r.calculateStatus(ctx, rayServiceInstance); err != nil {
		return ctrl.Result{RequeueAfter: ServiceDefaultRequeueDuration}, err
	}

	// Final status update for any CR modification.
	if inconsistentRayServiceStatuses(ctx, originalRayServiceInstance.Status, rayServiceInstance.Status) {
		rayServiceInstance.Status.LastUpdateTime = &metav1.Time{Time: time.Now()}
		if errStatus := r.Status().Update(ctx, rayServiceInstance); errStatus != nil {
			logger.Error(errStatus, "Failed to update RayService status", "rayServiceInstance", rayServiceInstance)
			return ctrl.Result{RequeueAfter: ServiceDefaultRequeueDuration}, errStatus
		}
	}

	return ctrl.Result{RequeueAfter: ServiceDefaultRequeueDuration}, nil
}

func validateRayServiceSpec(rayService *rayv1.RayService) error {
	if headSvc := rayService.Spec.RayClusterSpec.HeadGroupSpec.HeadService; headSvc != nil && headSvc.Name != "" {
		return fmt.Errorf("spec.rayClusterConfig.headGroupSpec.headService.metadata.name should not be set")
	}

	// only NewCluster and None are valid upgradeType
	if rayService.Spec.UpgradeStrategy != nil &&
		rayService.Spec.UpgradeStrategy.Type != nil &&
		*rayService.Spec.UpgradeStrategy.Type != rayv1.None &&
		*rayService.Spec.UpgradeStrategy.Type != rayv1.NewCluster {
		return fmt.Errorf("Spec.UpgradeStrategy.Type value %s is invalid, valid options are %s or %s", *rayService.Spec.UpgradeStrategy.Type, rayv1.NewCluster, rayv1.None)
	}
	return nil
}

func isEagerExposesServicesEnabled() bool {
	return strings.ToLower(os.Getenv(utils.ENABLE_RAYSERVICE_EAGER_EXPOSES_SERVICES)) == "true"
}

func (r *RayServiceReconciler) calculateStatus(ctx context.Context, rayServiceInstance *rayv1.RayService) error {
	serveEndPoints := &corev1.Endpoints{}
	if err := r.Get(ctx, common.RayServiceServeServiceNamespacedName(rayServiceInstance), serveEndPoints); err != nil && !errors.IsNotFound(err) {
		return err
	}

	numServeEndpoints := 0
	// Ray Pod addresses are categorized into subsets based on the IPs they share.
	// subset.Addresses contains a list of Ray Pod addresses with ready serve port.
	for _, subset := range serveEndPoints.Subsets {
		numServeEndpoints += len(subset.Addresses)
	}
	if numServeEndpoints > math.MaxInt32 {
		return errstd.New("numServeEndpoints exceeds math.MaxInt32")
	}
	rayServiceInstance.Status.NumServeEndpoints = int32(numServeEndpoints) //nolint:gosec // This is a false positive from gosec. See https://github.com/securego/gosec/issues/1212 for more details.
	return nil
}

// Checks whether the old and new RayServiceStatus are inconsistent by comparing different fields.
// If the only difference between the old and new status is the HealthLastUpdateTime field,
// the status update will not be triggered.
// The RayClusterStatus field is only for observability in RayService CR, and changes to it will not trigger the status update.
func inconsistentRayServiceStatus(ctx context.Context, oldStatus rayv1.RayServiceStatus, newStatus rayv1.RayServiceStatus) bool {
	logger := ctrl.LoggerFrom(ctx)
	if oldStatus.RayClusterName != newStatus.RayClusterName {
		logger.Info("inconsistentRayServiceStatus RayService RayClusterName", "oldRayClusterName", oldStatus.RayClusterName, "newRayClusterName", newStatus.RayClusterName)
		return true
	}

	if len(oldStatus.Applications) != len(newStatus.Applications) {
		return true
	}

	var ok bool
	for appName, newAppStatus := range newStatus.Applications {
		var oldAppStatus rayv1.AppStatus
		if oldAppStatus, ok = oldStatus.Applications[appName]; !ok {
			logger.Info("inconsistentRayServiceStatus RayService new application found", "appName", appName)
			return true
		}

		if oldAppStatus.Status != newAppStatus.Status {
			logger.Info("inconsistentRayServiceStatus RayService application status changed", "appName", appName, "oldStatus", oldAppStatus.Status, "newStatus", newAppStatus.Status)
			return true
		} else if oldAppStatus.Message != newAppStatus.Message {
			logger.Info("inconsistentRayServiceStatus RayService application status message changed", "appName", appName, "oldStatus", oldAppStatus.Message, "newStatus", newAppStatus.Message)
			return true
		}

		if len(oldAppStatus.Deployments) != len(newAppStatus.Deployments) {
			return true
		}

		for deploymentName, newDeploymentStatus := range newAppStatus.Deployments {
			var oldDeploymentStatus rayv1.ServeDeploymentStatus
			if oldDeploymentStatus, ok = oldAppStatus.Deployments[deploymentName]; !ok {
				logger.Info("inconsistentRayServiceStatus RayService new deployment found in application", "deploymentName", deploymentName, "appName", appName)
				return true
			}

			if oldDeploymentStatus.Status != newDeploymentStatus.Status {
				logger.Info("inconsistentRayServiceStatus RayService DeploymentStatus changed", "oldDeploymentStatus", oldDeploymentStatus.Status, "newDeploymentStatus", newDeploymentStatus.Status)
				return true
			} else if oldDeploymentStatus.Message != newDeploymentStatus.Message {
				logger.Info("inconsistentRayServiceStatus RayService deployment status message changed", "oldDeploymentStatus", oldDeploymentStatus.Message, "newDeploymentStatus", newDeploymentStatus.Message)
				return true
			}
		}
	}

	return false
}

// Determine whether to update the status of the RayService instance.
func inconsistentRayServiceStatuses(ctx context.Context, oldStatus rayv1.RayServiceStatuses, newStatus rayv1.RayServiceStatuses) bool {
	logger := ctrl.LoggerFrom(ctx)
	if oldStatus.ServiceStatus != newStatus.ServiceStatus {
		logger.Info("inconsistentRayServiceStatus RayService ServiceStatus changed", "oldServiceStatus", oldStatus.ServiceStatus, "newServiceStatus", newStatus.ServiceStatus)
		return true
	}

	if oldStatus.NumServeEndpoints != newStatus.NumServeEndpoints {
		logger.Info("inconsistentRayServiceStatus RayService NumServeEndpoints changed", "oldNumServeEndpoints", oldStatus.NumServeEndpoints, "newNumServeEndpoints", newStatus.NumServeEndpoints)
		return true
	}

	if inconsistentRayServiceStatus(ctx, oldStatus.ActiveServiceStatus, newStatus.ActiveServiceStatus) {
		logger.Info("inconsistentRayServiceStatus RayService ActiveServiceStatus changed")
		return true
	}

	if inconsistentRayServiceStatus(ctx, oldStatus.PendingServiceStatus, newStatus.PendingServiceStatus) {
		logger.Info("inconsistentRayServiceStatus RayService PendingServiceStatus changed")
		return true
	}

	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *RayServiceReconciler) SetupWithManager(mgr ctrl.Manager, reconcileConcurrency int) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rayv1.RayService{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.LabelChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Owns(&rayv1.RayCluster{}).
		Owns(&corev1.Service{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: reconcileConcurrency,
			LogConstructor: func(request *reconcile.Request) logr.Logger {
				logger := ctrl.Log.WithName("controllers").WithName("RayService")
				if request != nil {
					logger = logger.WithValues("RayService", request.NamespacedName)
				}
				return logger
			},
		}).
		Complete(r)
}

func (r *RayServiceReconciler) getRayServiceInstance(ctx context.Context, request ctrl.Request) (*rayv1.RayService, error) {
	logger := ctrl.LoggerFrom(ctx)
	rayServiceInstance := &rayv1.RayService{}
	if err := r.Get(ctx, request.NamespacedName, rayServiceInstance); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Read request instance not found error!")
		} else {
			logger.Error(err, "Read request instance error!")
		}
		return nil, err
	}
	return rayServiceInstance, nil
}

func isZeroDowntimeUpgradeEnabled(ctx context.Context, rayService *rayv1.RayService) bool {
	// For LLM serving, some users might not have sufficient GPU resources to run two RayClusters simultaneously.
	// Therefore, KubeRay offers ENABLE_ZERO_DOWNTIME as a feature flag for zero-downtime upgrades.
	// There are two ways to enable zero downtime upgrade. Through ENABLE_ZERO_DOWNTIME env var or setting Spec.UpgradeStrategy.Type.
	// If no fields are set, zero downtime upgrade by default is enabled.
	// Spec.UpgradeStrategy.Type takes precedence over ENABLE_ZERO_DOWNTIME.
	logger := ctrl.LoggerFrom(ctx)
	upgradeStrategy := rayService.Spec.UpgradeStrategy
	if upgradeStrategy != nil {
		upgradeType := upgradeStrategy.Type
		if upgradeType != nil {
			if *upgradeType != rayv1.NewCluster {
				logger.Info("Zero-downtime upgrade is disabled because UpgradeStrategy.Type is not set to NewCluster.")
				return false
			}
			return true
		}
	}
	zeroDowntimeEnvVar := os.Getenv(ENABLE_ZERO_DOWNTIME)
	if strings.ToLower(zeroDowntimeEnvVar) == "false" {
		logger.Info("Zero-downtime upgrade is disabled because ENABLE_ZERO_DOWNTIME is set to false.")
		return false
	}
	return true
}

// reconcileRayCluster checks the active and pending ray cluster instances. It includes 3 parts.
// 1. It will decide whether to generate a pending cluster name.
// 2. It will delete the old pending ray cluster instance.
// 3. It will create a new pending ray cluster instance.
func (r *RayServiceReconciler) reconcileRayCluster(ctx context.Context, rayServiceInstance *rayv1.RayService) (*rayv1.RayCluster, *rayv1.RayCluster, error) {
	logger := ctrl.LoggerFrom(ctx)
	var err error
	if err = r.cleanUpRayClusterInstance(ctx, rayServiceInstance); err != nil {
		return nil, nil, err
	}

	// Get active cluster and pending cluster instances.
	activeRayCluster, err := r.getRayClusterByNamespacedName(ctx, common.RayServiceActiveRayClusterNamespacedName(rayServiceInstance))
	if err != nil {
		return nil, nil, err
	}

	pendingRayCluster, err := r.getRayClusterByNamespacedName(ctx, common.RayServicePendingRayClusterNamespacedName(rayServiceInstance))
	if err != nil {
		return nil, nil, err
	}

	clusterAction := decideClusterAction(ctx, rayServiceInstance, activeRayCluster, pendingRayCluster)
	switch clusterAction {
	case GeneratePendingClusterName:
		markRestartAndAddPendingClusterName(ctx, rayServiceInstance)
		return activeRayCluster, nil, nil
	case CreatePendingCluster:
		logger.Info("Creating a new pending RayCluster instance.")
		pendingRayCluster, err = r.createRayClusterInstance(ctx, rayServiceInstance)
		return activeRayCluster, pendingRayCluster, err
	case UpdatePendingCluster:
		logger.Info("Updating the pending RayCluster instance.")
		pendingRayCluster, err = r.constructRayClusterForRayService(ctx, rayServiceInstance, pendingRayCluster.Name)
		if err != nil {
			return nil, nil, err
		}
		err = r.updateRayClusterInstance(ctx, pendingRayCluster)
		if err != nil {
			return nil, nil, err
		}
		return activeRayCluster, pendingRayCluster, nil
	case UpdateActiveCluster:
		logger.Info("Updating the active RayCluster instance.")
		if activeRayCluster, err = r.constructRayClusterForRayService(ctx, rayServiceInstance, activeRayCluster.Name); err != nil {
			return nil, nil, err
		}
		if err := r.updateRayClusterInstance(ctx, activeRayCluster); err != nil {
			return nil, nil, err
		}
		return activeRayCluster, nil, nil
	case DoNothing:
		return activeRayCluster, pendingRayCluster, nil
	default:
		panic(fmt.Sprintf("Unexpected clusterAction: %v", clusterAction))
	}
}

// cleanUpRayClusterInstance cleans up all the dangling RayCluster instances that are owned by the RayService instance.
func (r *RayServiceReconciler) cleanUpRayClusterInstance(ctx context.Context, rayServiceInstance *rayv1.RayService) error {
	logger := ctrl.LoggerFrom(ctx)
	rayClusterList := rayv1.RayClusterList{}

	var err error
	if err = r.List(ctx, &rayClusterList, common.RayServiceRayClustersAssociationOptions(rayServiceInstance).ToListOptions()...); err != nil {
		return err
	}

	// Clean up RayCluster instances. Each instance is deleted 60 seconds
	for _, rayClusterInstance := range rayClusterList.Items {
		if rayClusterInstance.Name != rayServiceInstance.Status.ActiveServiceStatus.RayClusterName && rayClusterInstance.Name != rayServiceInstance.Status.PendingServiceStatus.RayClusterName {
			cachedTimestamp, exists := r.RayClusterDeletionTimestamps.Get(rayClusterInstance.Name)
			if !exists {
				deletionTimestamp := metav1.Now().Add(RayClusterDeletionDelayDuration)
				r.RayClusterDeletionTimestamps.Set(rayClusterInstance.Name, deletionTimestamp)
				logger.Info(
					"Scheduled dangling RayCluster for deletion",
					"rayClusterName", rayClusterInstance.Name,
					"deletionTimestamp", deletionTimestamp,
				)
			} else {
				reasonForDeletion := ""
				if time.Since(cachedTimestamp) > 0*time.Second {
					reasonForDeletion = fmt.Sprintf("Deletion timestamp %s "+
						"for RayCluster %s has passed. Deleting cluster "+
						"immediately.", cachedTimestamp, rayClusterInstance.Name)
				}

				if reasonForDeletion != "" {
					logger.Info("reconcileRayCluster", "delete Ray cluster", rayClusterInstance.Name, "reason", reasonForDeletion)
					if err := r.Delete(ctx, &rayClusterInstance, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

func (r *RayServiceReconciler) getRayClusterByNamespacedName(ctx context.Context, clusterKey client.ObjectKey) (*rayv1.RayCluster, error) {
	if clusterKey.Name == "" {
		return nil, nil
	}

	rayCluster := &rayv1.RayCluster{}
	if err := r.Get(ctx, clusterKey, rayCluster); client.IgnoreNotFound(err) != nil {
		return nil, err
	}

	return rayCluster, nil
}

// cleanUpServeConfigCache cleans up the unused serve applications config in the cached map.
func (r *RayServiceReconciler) cleanUpServeConfigCache(ctx context.Context, rayServiceInstance *rayv1.RayService) {
	logger := ctrl.LoggerFrom(ctx)
	activeRayClusterName := rayServiceInstance.Status.ActiveServiceStatus.RayClusterName
	pendingRayClusterName := rayServiceInstance.Status.PendingServiceStatus.RayClusterName

	cacheKey := rayServiceInstance.Namespace + "/" + rayServiceInstance.Name
	cacheValue, exist := r.ServeConfigs.Get(cacheKey)
	if !exist {
		return
	}
	clusterNameToServeConfig := cacheValue.(cmap.ConcurrentMap[string, string])

	for key := range clusterNameToServeConfig.Items() {
		if key == activeRayClusterName || key == pendingRayClusterName {
			continue
		}
		logger.Info("Remove stale serve application config", "remove key", key, "activeRayClusterName", activeRayClusterName, "pendingRayClusterName", pendingRayClusterName)
		clusterNameToServeConfig.Remove(key)
	}
}

type ClusterAction int

const (
	DoNothing ClusterAction = iota
	UpdateActiveCluster
	UpdatePendingCluster
	GeneratePendingClusterName
	CreatePendingCluster
)

// decideClusterAction decides the action to take for the underlying RayCluster instances.
// Prepare new RayCluster if:
// 1. No active cluster and no pending cluster
// 2. No pending cluster, and the active RayCluster has changed.
func decideClusterAction(ctx context.Context, rayServiceInstance *rayv1.RayService, activeRayCluster, pendingRayCluster *rayv1.RayCluster) ClusterAction {
	logger := ctrl.LoggerFrom(ctx)

	// Handle pending RayCluster cases.
	if rayServiceInstance.Status.PendingServiceStatus.RayClusterName != "" {
		oldSpec := pendingRayCluster.Spec
		newSpec := rayServiceInstance.Spec.RayClusterSpec
		// If everything is identical except for the Replicas and WorkersToDelete of
		// each WorkerGroup, then do nothing.
		sameHash, err := compareRayClusterJsonHash(oldSpec, newSpec, generateHashWithoutReplicasAndWorkersToDelete)
		if err != nil || sameHash {
			return DoNothing
		}

		// If everything is identical except for the Replicas and WorkersToDelete of the existing workergroups,
		// and one or more new workergroups are added at the end, then update the cluster.
		newSpecWithAddedWorkerGroupsStripped := newSpec.DeepCopy()
		if len(newSpec.WorkerGroupSpecs) > len(oldSpec.WorkerGroupSpecs) {
			// Remove the new worker groups from the new spec.
			newSpecWithAddedWorkerGroupsStripped.WorkerGroupSpecs = newSpecWithAddedWorkerGroupsStripped.WorkerGroupSpecs[:len(oldSpec.WorkerGroupSpecs)]

			sameHash, err = compareRayClusterJsonHash(oldSpec, *newSpecWithAddedWorkerGroupsStripped, generateHashWithoutReplicasAndWorkersToDelete)
			if err != nil {
				return DoNothing
			}
			if sameHash {
				return UpdatePendingCluster
			}
		}

		// Otherwise, create the pending cluster.
		return CreatePendingCluster
	}

	if activeRayCluster == nil {
		logger.Info("No active Ray cluster. RayService operator should prepare a new Ray cluster.")
		return GeneratePendingClusterName
	}

	// If the KubeRay version has changed, update the RayCluster to get the cluster hash and new KubeRay version.
	activeKubeRayVersion := activeRayCluster.ObjectMeta.Annotations[utils.KubeRayVersion]
	if activeKubeRayVersion != utils.KUBERAY_VERSION {
		logger.Info("Active RayCluster config doesn't match goal config due to mismatched KubeRay versions. Updating RayCluster.")
		return UpdateActiveCluster
	}

	// If everything is identical except for the Replicas and WorkersToDelete of
	// each WorkerGroup, then do nothing.
	activeClusterHash := activeRayCluster.ObjectMeta.Annotations[utils.HashWithoutReplicasAndWorkersToDeleteKey]
	goalClusterHash, err := generateHashWithoutReplicasAndWorkersToDelete(rayServiceInstance.Spec.RayClusterSpec)
	errContextFailedToSerialize := "Failed to serialize new RayCluster config. " +
		"Manual config updates will NOT be tracked accurately. " +
		"Please manually tear down the cluster and apply a new config."
	if err != nil {
		logger.Error(err, errContextFailedToSerialize)
		return DoNothing
	}

	if activeClusterHash == goalClusterHash {
		logger.Info("Active Ray cluster config matches goal config. No need to update RayCluster.")
		return DoNothing
	}

	// If everything is identical except for the Replicas and WorkersToDelete of
	// the existing workergroups, and one or more new workergroups are added at the end, then update the cluster.
	activeClusterNumWorkerGroups, err := strconv.Atoi(activeRayCluster.ObjectMeta.Annotations[utils.NumWorkerGroupsKey])
	if err != nil {
		logger.Error(err, errContextFailedToSerialize)
		return DoNothing
	}
	goalNumWorkerGroups := len(rayServiceInstance.Spec.RayClusterSpec.WorkerGroupSpecs)
	logger.Info("number of worker groups", "activeClusterNumWorkerGroups", activeClusterNumWorkerGroups, "goalNumWorkerGroups", goalNumWorkerGroups)
	if goalNumWorkerGroups > activeClusterNumWorkerGroups {

		// Remove the new workergroup(s) from the end before calculating the hash.
		goalClusterSpec := rayServiceInstance.Spec.RayClusterSpec.DeepCopy()
		goalClusterSpec.WorkerGroupSpecs = goalClusterSpec.WorkerGroupSpecs[:activeClusterNumWorkerGroups]

		// Generate the hash of the old worker group specs.
		goalClusterHash, err = generateHashWithoutReplicasAndWorkersToDelete(*goalClusterSpec)
		if err != nil {
			logger.Error(err, errContextFailedToSerialize)
			return DoNothing
		}

		if activeClusterHash == goalClusterHash {
			logger.Info("Active RayCluster config matches goal config, except that one or more entries were appended to WorkerGroupSpecs. Updating RayCluster.")
			return UpdateActiveCluster
		}
	}

	// Otherwise, rollout a new cluster if zero-downtime upgrade is enabled.
	if isZeroDowntimeUpgradeEnabled(ctx, rayServiceInstance) {
		logger.Info(
			"Active RayCluster config doesn't match goal config. "+
				"RayService operator should prepare a new Ray cluster.",
			"activeClusterConfigHash", activeClusterHash,
			"goalClusterConfigHash", goalClusterHash,
		)
		return GeneratePendingClusterName
	}

	logger.Info("Zero-downtime upgrade is disabled. Skip preparing a new RayCluster.")
	return DoNothing
}

// updateRayClusterInstance updates the RayCluster instance.
func (r *RayServiceReconciler) updateRayClusterInstance(ctx context.Context, rayClusterInstance *rayv1.RayCluster) error {
	logger := ctrl.LoggerFrom(ctx)
	logger.Info("updateRayClusterInstance", "Name", rayClusterInstance.Name, "Namespace", rayClusterInstance.Namespace)
	// Printing the whole RayCluster is too noisy. Only print the spec.
	logger.Info("updateRayClusterInstance", "rayClusterInstance.Spec", rayClusterInstance.Spec)

	// Fetch the current state of the RayCluster
	currentRayCluster, err := r.getRayClusterByNamespacedName(ctx, client.ObjectKey{
		Namespace: rayClusterInstance.Namespace,
		Name:      rayClusterInstance.Name,
	})
	if err != nil {
		err = fmt.Errorf("failed to get the current state of RayCluster, namespace: %s, name: %s: %w", rayClusterInstance.Namespace, rayClusterInstance.Name, err)
		return err
	}

	if currentRayCluster == nil {
		logger.Info("RayCluster not found, possibly deleted", "Namespace", rayClusterInstance.Namespace, "Name", rayClusterInstance.Name)
		return nil
	}

	// Update the fetched RayCluster with new changes
	currentRayCluster.Spec = rayClusterInstance.Spec

	// Update the labels and annotations
	currentRayCluster.Labels = rayClusterInstance.Labels
	currentRayCluster.Annotations = rayClusterInstance.Annotations

	// Update the RayCluster
	if err = r.Update(ctx, currentRayCluster); err != nil {
		return err
	}

	logger.Info("updated RayCluster", "rayClusterInstance", currentRayCluster)
	return nil
}

// createRayClusterInstance deletes the old RayCluster instance if exists. Only when no existing RayCluster, create a new RayCluster instance.
// One important part is that if this method deletes the old RayCluster, it will return instantly. It depends on the controller to call it again to generate the new RayCluster instance.
func (r *RayServiceReconciler) createRayClusterInstance(ctx context.Context, rayServiceInstance *rayv1.RayService) (*rayv1.RayCluster, error) {
	logger := ctrl.LoggerFrom(ctx)
	rayClusterKey := common.RayServicePendingRayClusterNamespacedName(rayServiceInstance)

	logger.Info("createRayClusterInstance", "rayClusterInstanceName", rayClusterKey.Name)

	rayClusterInstance := &rayv1.RayCluster{}

	var err error
	// Loop until there is no pending RayCluster.
	err = r.Get(ctx, rayClusterKey, rayClusterInstance)

	// If RayCluster exists, it means the config is updated. Delete the previous RayCluster first.
	if err == nil {
		logger.Info("Ray cluster already exists, config changes. Need to recreate. Delete the pending one now.", "key", rayClusterKey.String(), "rayClusterInstance.Spec", rayClusterInstance.Spec, "rayServiceInstance.Spec.RayClusterSpec", rayServiceInstance.Spec.RayClusterSpec)
		delErr := r.Delete(ctx, rayClusterInstance, client.PropagationPolicy(metav1.DeletePropagationBackground))
		if delErr == nil {
			// Go to next loop and check if the ray cluster is deleted.
			return nil, nil
		} else if !errors.IsNotFound(delErr) {
			return nil, delErr
		}
		// if error is `not found`, then continue.
	} else if !errors.IsNotFound(err) {
		return nil, err
		// if error is `not found`, then continue.
	}

	logger.Info("No pending RayCluster, creating RayCluster.")
	rayClusterInstance, err = r.constructRayClusterForRayService(ctx, rayServiceInstance, rayClusterKey.Name)
	if err != nil {
		return nil, err
	}
	if err = r.Create(ctx, rayClusterInstance); err != nil {
		return nil, err
	}
	logger.Info("created rayCluster for rayService", "rayCluster", rayClusterInstance)

	return rayClusterInstance, nil
}

func (r *RayServiceReconciler) constructRayClusterForRayService(ctx context.Context, rayService *rayv1.RayService, rayClusterName string) (*rayv1.RayCluster, error) {
	logger := ctrl.LoggerFrom(ctx)

	var err error
	rayClusterLabel := make(map[string]string)
	for k, v := range rayService.Labels {
		rayClusterLabel[k] = v
	}
	rayClusterLabel[utils.RayOriginatedFromCRNameLabelKey] = rayService.Name
	rayClusterLabel[utils.RayOriginatedFromCRDLabelKey] = utils.RayOriginatedFromCRDLabelValue(utils.RayServiceCRD)

	rayClusterAnnotations := make(map[string]string)
	for k, v := range rayService.Annotations {
		rayClusterAnnotations[k] = v
	}
	errContext := "Failed to serialize RayCluster config. " +
		"Manual config updates will NOT be tracked accurately. " +
		"Please tear down the cluster and apply a new config."
	rayClusterAnnotations[utils.HashWithoutReplicasAndWorkersToDeleteKey], err = generateHashWithoutReplicasAndWorkersToDelete(rayService.Spec.RayClusterSpec)
	if err != nil {
		logger.Error(err, errContext)
		return nil, err
	}
	rayClusterAnnotations[utils.NumWorkerGroupsKey] = strconv.Itoa(len(rayService.Spec.RayClusterSpec.WorkerGroupSpecs))

	// set the KubeRay version used to create the RayCluster
	rayClusterAnnotations[utils.KubeRayVersion] = utils.KUBERAY_VERSION

	rayCluster := &rayv1.RayCluster{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      rayClusterLabel,
			Annotations: rayClusterAnnotations,
			Name:        rayClusterName,
			Namespace:   rayService.Namespace,
		},
		Spec: rayService.Spec.RayClusterSpec,
	}

	// Set the ownership in order to do the garbage collection by k8s.
	if err := ctrl.SetControllerReference(rayService, rayCluster, r.Scheme); err != nil {
		return nil, err
	}

	return rayCluster, nil
}

func (r *RayServiceReconciler) checkIfNeedSubmitServeDeployment(ctx context.Context, rayServiceInstance *rayv1.RayService, rayClusterInstance *rayv1.RayCluster, serveStatus *rayv1.RayServiceStatus) bool {
	logger := ctrl.LoggerFrom(ctx)

	// If the Serve config has not been cached, update the Serve config.
	cachedServeConfigV2 := r.getServeConfigFromCache(rayServiceInstance, rayClusterInstance.Name)
	if cachedServeConfigV2 == "" {
		logger.Info(
			"shouldUpdate",
			"shouldUpdateServe", true,
			"reason", "Nothing has been cached for the cluster",
			"rayClusterName", rayClusterInstance.Name,
		)
		return true
	}

	// Handle the case that the head Pod has crashed and GCS FT is not enabled.
	if len(serveStatus.Applications) == 0 {
		logger.Info(
			"shouldUpdate",
			"should create Serve applications", true,
			"reason",
			"No Serve application found in the RayCluster, need to create serve applications. "+
				"A possible reason is the head Pod has crashed and GCS FT is not enabled. "+
				"Hence, the RayService CR's Serve application status is set to empty in the previous reconcile.",
			"rayClusterName", rayClusterInstance.Name,
		)
		return true
	}

	// If the Serve config has been cached, check if it needs to be updated.
	shouldUpdate := false
	reason := fmt.Sprintf("Current Serve config matches cached Serve config, "+
		"and some deployments have been deployed for cluster %s", rayClusterInstance.Name)

	if cachedServeConfigV2 != rayServiceInstance.Spec.ServeConfigV2 {
		shouldUpdate = true
		reason = fmt.Sprintf("Current V2 Serve config doesn't match cached Serve config for cluster %s", rayClusterInstance.Name)
	}
	logger.Info("shouldUpdate", "shouldUpdateServe", shouldUpdate, "reason", reason, "cachedServeConfig", cachedServeConfigV2, "current Serve config", rayServiceInstance.Spec.ServeConfigV2)

	return shouldUpdate
}

func (r *RayServiceReconciler) updateServeDeployment(ctx context.Context, rayServiceInstance *rayv1.RayService, rayDashboardClient utils.RayDashboardClientInterface, clusterName string) error {
	logger := ctrl.LoggerFrom(ctx)
	logger.Info("updateServeDeployment", "V2 config", rayServiceInstance.Spec.ServeConfigV2)

	serveConfig := make(map[string]interface{})
	if err := yaml.Unmarshal([]byte(rayServiceInstance.Spec.ServeConfigV2), &serveConfig); err != nil {
		return err
	}

	configJson, err := json.Marshal(serveConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal converted serve config into bytes: %w", err)
	}
	logger.Info("updateServeDeployment", "MULTI_APP json config", string(configJson))
	if err := rayDashboardClient.UpdateDeployments(ctx, configJson); err != nil {
		err = fmt.Errorf(
			"fail to create / update Serve applications. If you observe this error consistently, "+
				"please check \"Issue 5: Fail to create / update Serve applications.\" in "+
				"https://docs.ray.io/en/master/cluster/kubernetes/troubleshooting/rayservice-troubleshooting.html#kuberay-raysvc-troubleshoot for more details. "+
				"err: %v", err)
		return err
	}

	r.cacheServeConfig(rayServiceInstance, clusterName)
	logger.Info("updateServeDeployment", "message", "Cached Serve config for Ray cluster with the key", "rayClusterName", clusterName)
	return nil
}

// `getAndCheckServeStatus` gets Serve applications' and deployments' statuses and check whether the
// Serve applications are ready to serve incoming traffic or not. It returns two values:
//
// (1) `isReady` is used to determine whether the Serve applications in the RayCluster are ready to serve incoming traffic or not.
// (2) `err`: If `err` is not nil, it means that KubeRay failed to get Serve application statuses from the dashboard. We should take a look at dashboard rather than Ray Serve applications.

func getAndCheckServeStatus(ctx context.Context, dashboardClient utils.RayDashboardClientInterface, rayServiceServeStatus *rayv1.RayServiceStatus) (bool, error) {
	logger := ctrl.LoggerFrom(ctx)
	var serveAppStatuses map[string]*utils.ServeApplicationStatus
	var err error
	if serveAppStatuses, err = dashboardClient.GetMultiApplicationStatus(ctx); err != nil {
		err = fmt.Errorf(
			"failed to get Serve application statuses from the dashboard. "+
				"If you observe this error consistently, please check https://docs.ray.io/en/latest/cluster/kubernetes/troubleshooting/rayservice-troubleshooting.html for more details. "+
				"err: %v", err)
		return false, err
	}

	logger.Info("getAndCheckServeStatus", "prev statuses", rayServiceServeStatus.Applications, "serve statuses", serveAppStatuses)

	isReady := true
	timeNow := metav1.Now()

	newApplications := make(map[string]rayv1.AppStatus)
	for appName, app := range serveAppStatuses {
		if appName == "" {
			appName = utils.DefaultServeAppName
		}

		prevApplicationStatus := rayServiceServeStatus.Applications[appName]

		applicationStatus := rayv1.AppStatus{
			Message:              app.Message,
			Status:               app.Status,
			HealthLastUpdateTime: &timeNow,
			Deployments:          make(map[string]rayv1.ServeDeploymentStatus),
		}

		if isServeAppUnhealthyOrDeployedFailed(app.Status) {
			if isServeAppUnhealthyOrDeployedFailed(prevApplicationStatus.Status) {
				if prevApplicationStatus.HealthLastUpdateTime != nil {
					applicationStatus.HealthLastUpdateTime = prevApplicationStatus.HealthLastUpdateTime
					logger.Info("Ray Serve application is unhealthy", "appName", appName, "detail",
						"The status of the serve application has been UNHEALTHY or DEPLOY_FAILED since last updated.",
						"appName", appName,
						"healthLastUpdateTime", prevApplicationStatus.HealthLastUpdateTime)
				}
			}
		}

		// `isReady` is used to determine whether the Serve application is ready or not. The cluster switchover only happens when all Serve
		// applications in this RayCluster are ready so that the incoming traffic will not be dropped.
		if app.Status != rayv1.ApplicationStatusEnum.RUNNING {
			isReady = false
		}

		// Copy deployment statuses
		for deploymentName, deployment := range app.Deployments {
			deploymentStatus := rayv1.ServeDeploymentStatus{
				Status:               deployment.Status,
				Message:              deployment.Message,
				HealthLastUpdateTime: &timeNow,
			}

			if deployment.Status == rayv1.DeploymentStatusEnum.UNHEALTHY {
				prevStatus, exist := prevApplicationStatus.Deployments[deploymentName]
				if exist {
					if prevStatus.Status == rayv1.DeploymentStatusEnum.UNHEALTHY {
						deploymentStatus.HealthLastUpdateTime = prevStatus.HealthLastUpdateTime
					}
				}
			}
			applicationStatus.Deployments[deploymentName] = deploymentStatus
		}
		newApplications[appName] = applicationStatus
	}

	if len(newApplications) == 0 {
		logger.Info("No Serve application found. The RayCluster is not ready to serve requests. Set 'isReady' to false")
		isReady = false
	}
	rayServiceServeStatus.Applications = newApplications
	logger.Info("getAndCheckServeStatus", "new statuses", rayServiceServeStatus.Applications)
	return isReady, nil
}

func (r *RayServiceReconciler) getServeConfigFromCache(rayServiceInstance *rayv1.RayService, clusterName string) string {
	cacheKey := rayServiceInstance.Namespace + "/" + rayServiceInstance.Name
	cacheValue, exist := r.ServeConfigs.Get(cacheKey)
	if !exist {
		return ""
	}
	serveConfigs := cacheValue.(cmap.ConcurrentMap[string, string])
	serveConfig, exist := serveConfigs.Get(clusterName)
	if !exist {
		return ""
	}
	return serveConfig
}

func (r *RayServiceReconciler) cacheServeConfig(rayServiceInstance *rayv1.RayService, clusterName string) {
	serveConfig := rayServiceInstance.Spec.ServeConfigV2
	if serveConfig == "" {
		return
	}
	cacheKey := rayServiceInstance.Namespace + "/" + rayServiceInstance.Name
	cacheValue, exist := r.ServeConfigs.Get(cacheKey)
	var rayServiceServeConfigs cmap.ConcurrentMap[string, string]
	if !exist {
		rayServiceServeConfigs = cmap.New[string]()
		r.ServeConfigs.Add(cacheKey, rayServiceServeConfigs)
	} else {
		rayServiceServeConfigs = cacheValue.(cmap.ConcurrentMap[string, string])
	}
	rayServiceServeConfigs.Set(clusterName, serveConfig)
}

func markRestartAndAddPendingClusterName(ctx context.Context, rayServiceInstance *rayv1.RayService) {
	logger := ctrl.LoggerFrom(ctx)

	// Generate RayCluster name for pending cluster.
	logger.Info("Current cluster is unhealthy, prepare to restart.", "Status", rayServiceInstance.Status)
	rayServiceInstance.Status.ServiceStatus = rayv1.Restarting
	rayServiceInstance.Status.PendingServiceStatus = rayv1.RayServiceStatus{
		RayClusterName: utils.GenerateRayClusterName(rayServiceInstance.Name),
	}
}

func updateRayClusterInfo(ctx context.Context, rayServiceInstance *rayv1.RayService, healthyClusterName string) {
	logger := ctrl.LoggerFrom(ctx)
	logger.Info("updateRayClusterInfo", "ActiveRayClusterName", rayServiceInstance.Status.ActiveServiceStatus.RayClusterName, "healthyClusterName", healthyClusterName)
	if rayServiceInstance.Status.ActiveServiceStatus.RayClusterName != healthyClusterName {
		rayServiceInstance.Status.ActiveServiceStatus = rayServiceInstance.Status.PendingServiceStatus
		rayServiceInstance.Status.PendingServiceStatus = rayv1.RayServiceStatus{}
	}
}

func (r *RayServiceReconciler) reconcileServices(ctx context.Context, rayServiceInstance *rayv1.RayService, rayClusterInstance *rayv1.RayCluster, serviceType utils.ServiceType) error {
	logger := ctrl.LoggerFrom(ctx)
	logger.Info(
		"reconcileServices", "serviceType", serviceType,
	)

	var newSvc *corev1.Service
	var err error

	switch serviceType {
	case utils.HeadService:
		newSvc, err = common.BuildHeadServiceForRayService(ctx, *rayServiceInstance, *rayClusterInstance)
	case utils.ServingService:
		newSvc, err = common.BuildServeServiceForRayService(ctx, *rayServiceInstance, *rayClusterInstance)
	default:
		return fmt.Errorf("unknown service type %v", serviceType)
	}

	if err != nil {
		return err
	}
	logger.Info("reconcileServices", "newSvc", newSvc)

	// Retrieve the Service from the Kubernetes cluster with the name and namespace.
	oldSvc := &corev1.Service{}
	err = r.Get(ctx, client.ObjectKey{Name: newSvc.Name, Namespace: rayServiceInstance.Namespace}, oldSvc)

	if err == nil {
		// Only update the service if the RayCluster switches.
		if newSvc.Spec.Selector[utils.RayClusterLabelKey] == oldSvc.Spec.Selector[utils.RayClusterLabelKey] {
			logger.Info("Service has already exists in the RayCluster, skip Update", "rayCluster", newSvc.Spec.Selector[utils.RayClusterLabelKey], "serviceType", serviceType)
			return nil
		}

		// ClusterIP is immutable. Starting from Kubernetes v1.21.5, if the new service does not specify a ClusterIP,
		// Kubernetes will assign the ClusterIP of the old service to the new one. However, to maintain compatibility
		// with older versions of Kubernetes, we need to assign the ClusterIP here.
		newSvc.Spec.ClusterIP = oldSvc.Spec.ClusterIP

		// TODO (kevin85421): Consider not only the updates of the Spec but also the ObjectMeta.
		oldSvc.Spec = *newSvc.Spec.DeepCopy()
		logger.Info("Update Kubernetes Service", "serviceType", serviceType)
		if updateErr := r.Update(ctx, oldSvc); updateErr != nil {
			return updateErr
		}
	} else if errors.IsNotFound(err) {
		logger.Info("Create a Kubernetes Service", "serviceType", serviceType)
		if err := ctrl.SetControllerReference(rayServiceInstance, newSvc, r.Scheme); err != nil {
			return err
		}
		if createErr := r.Create(ctx, newSvc); createErr != nil {
			if errors.IsAlreadyExists(createErr) {
				logger.Info("The Kubernetes Service already exists, no need to create.")
				return nil
			}
			return createErr
		}
	} else {
		return err
	}

	return nil
}

func (r *RayServiceReconciler) updateStatusForActiveCluster(ctx context.Context, rayServiceInstance *rayv1.RayService, rayClusterInstance *rayv1.RayCluster) error {
	logger := ctrl.LoggerFrom(ctx)
	rayServiceInstance.Status.ActiveServiceStatus.RayClusterStatus = rayClusterInstance.Status

	var err error
	var clientURL string
	rayServiceStatus := &rayServiceInstance.Status.ActiveServiceStatus

	if clientURL, err = utils.FetchHeadServiceURL(ctx, r.Client, rayClusterInstance, utils.DashboardPortName); err != nil || clientURL == "" {
		return err
	}

	rayDashboardClient := r.dashboardClientFunc()
	if err := rayDashboardClient.InitClient(ctx, clientURL, rayClusterInstance); err != nil {
		return err
	}

	var isReady bool
	if isReady, err = getAndCheckServeStatus(ctx, rayDashboardClient, rayServiceStatus); err != nil {
		return err
	}

	logger.Info("Check serve health", "isReady", isReady)

	return err
}

// Reconciles the Serve applications on the RayCluster. Returns (isReady, error).
// The `isReady` flag indicates whether the RayCluster is ready to handle incoming traffic.
func (r *RayServiceReconciler) reconcileServe(ctx context.Context, rayServiceInstance *rayv1.RayService, rayClusterInstance *rayv1.RayCluster, isActive bool) (bool, error) {
	logger := ctrl.LoggerFrom(ctx)
	rayServiceInstance.Status.ActiveServiceStatus.RayClusterStatus = rayClusterInstance.Status
	var err error
	var clientURL string
	var rayServiceStatus *rayv1.RayServiceStatus

	// Pick up service status to be updated.
	if isActive {
		rayServiceStatus = &rayServiceInstance.Status.ActiveServiceStatus
	} else {
		rayServiceStatus = &rayServiceInstance.Status.PendingServiceStatus
	}

	// Check if head pod is running and ready. If not, requeue the resource event to avoid
	// redundant custom resource status updates.
	//
	// TODO (kevin85421): Note that the Dashboard and GCS may take a few seconds to start up
	// after the head pod is running and ready. Hence, some requests to the Dashboard (e.g. `UpdateDeployments`) may fail.
	// This is not an issue since `UpdateDeployments` is an idempotent operation.
	logger.Info("Check the head Pod status of the pending RayCluster", "RayCluster name", rayClusterInstance.Name)

	// check the latest condition of the head Pod to see if it is ready.
	if features.Enabled(features.RayClusterStatusConditions) {
		if !meta.IsStatusConditionTrue(rayClusterInstance.Status.Conditions, string(rayv1.HeadPodReady)) {
			logger.Info("The head Pod is not ready, requeue the resource event to avoid redundant custom resource status updates.")
			return false, nil
		}
	} else {
		if isRunningAndReady, err := r.isHeadPodRunningAndReady(ctx, rayClusterInstance); err != nil || !isRunningAndReady {
			if err != nil {
				logger.Error(err, "Failed to check if head Pod is running and ready!")
			} else {
				logger.Info("Skipping the update of Serve applications because the Ray head Pod is not ready.")
			}
			return false, err
		}
	}

	// TODO(architkulkarni): Check the RayVersion. If < 2.8.0, error.

	if clientURL, err = utils.FetchHeadServiceURL(ctx, r.Client, rayClusterInstance, utils.DashboardPortName); err != nil || clientURL == "" {
		return false, err
	}

	rayDashboardClient := r.dashboardClientFunc()
	if err := rayDashboardClient.InitClient(ctx, clientURL, rayClusterInstance); err != nil {
		return false, err
	}

	shouldUpdate := r.checkIfNeedSubmitServeDeployment(ctx, rayServiceInstance, rayClusterInstance, rayServiceStatus)
	if shouldUpdate {
		if err = r.updateServeDeployment(ctx, rayServiceInstance, rayDashboardClient, rayClusterInstance.Name); err != nil {
			return false, err
		}
	}

	var isReady bool
	if isReady, err = getAndCheckServeStatus(ctx, rayDashboardClient, rayServiceStatus); err != nil {
		return false, err
	}

	logger.Info("Check serve health", "isReady", isReady, "isActive", isActive)

	if isReady {
		rayServiceInstance.Status.ServiceStatus = rayv1.Running
		updateRayClusterInfo(ctx, rayServiceInstance, rayClusterInstance.Name)
	} else {
		rayServiceInstance.Status.ServiceStatus = rayv1.WaitForServeDeploymentReady
		if err := r.Status().Update(ctx, rayServiceInstance); err != nil {
			return false, err
		}
		logger.Info("Mark cluster as waiting for Serve applications", "rayCluster", rayClusterInstance)
	}

	return isReady, nil
}

func (r *RayServiceReconciler) labelHeadPodForServeStatus(ctx context.Context, rayClusterInstance *rayv1.RayCluster, excludeHeadPodFromServeSvc bool) error {
	headPod, err := common.GetRayClusterHeadPod(ctx, r, rayClusterInstance)
	if err != nil {
		return err
	}
	if headPod == nil {
		return fmt.Errorf("found 0 head. cluster name %s, namespace %v", rayClusterInstance.Name, rayClusterInstance.Namespace)
	}

	httpProxyClient := r.httpProxyClientFunc()
	httpProxyClient.InitClient()

	rayContainer := headPod.Spec.Containers[utils.RayContainerIndex]
	servingPort := utils.FindContainerPort(&rayContainer, utils.ServingPortName, utils.DefaultServingPort)
	httpProxyClient.SetHostIp(headPod.Status.PodIP, headPod.Namespace, headPod.Name, servingPort)

	if headPod.Labels == nil {
		headPod.Labels = make(map[string]string)
	}

	// Make a copy of the labels for comparison later, to decide whether we need to push an update.
	originalLabels := make(map[string]string, len(headPod.Labels))
	for key, value := range headPod.Labels {
		originalLabels[key] = value
	}
	if err = httpProxyClient.CheckProxyActorHealth(ctx); err == nil && !excludeHeadPodFromServeSvc {
		headPod.Labels[utils.RayClusterServingServiceLabelKey] = utils.EnableRayClusterServingServiceTrue
	} else {
		headPod.Labels[utils.RayClusterServingServiceLabelKey] = utils.EnableRayClusterServingServiceFalse
	}

	if !reflect.DeepEqual(originalLabels, headPod.Labels) {
		if updateErr := r.Update(ctx, headPod); updateErr != nil {
			return updateErr
		}
	}

	return nil
}

func generateHashWithoutReplicasAndWorkersToDelete(rayClusterSpec rayv1.RayClusterSpec) (string, error) {
	// Mute certain fields that will not trigger new RayCluster preparation. For example,
	// Autoscaler will update `Replicas` and `WorkersToDelete` when scaling up/down.
	updatedRayClusterSpec := rayClusterSpec.DeepCopy()
	for i := 0; i < len(updatedRayClusterSpec.WorkerGroupSpecs); i++ {
		updatedRayClusterSpec.WorkerGroupSpecs[i].Replicas = nil
		updatedRayClusterSpec.WorkerGroupSpecs[i].MaxReplicas = nil
		updatedRayClusterSpec.WorkerGroupSpecs[i].MinReplicas = nil
		updatedRayClusterSpec.WorkerGroupSpecs[i].ScaleStrategy.WorkersToDelete = nil
	}

	// Generate a hash for the RayClusterSpec.
	return utils.GenerateJsonHash(updatedRayClusterSpec)
}

func compareRayClusterJsonHash(spec1 rayv1.RayClusterSpec, spec2 rayv1.RayClusterSpec, hashFunc func(rayv1.RayClusterSpec) (string, error)) (bool, error) {
	hash1, err1 := hashFunc(spec1)
	if err1 != nil {
		return false, err1
	}

	hash2, err2 := hashFunc(spec2)
	if err2 != nil {
		return false, err2
	}
	return hash1 == hash2, nil
}

// isHeadPodRunningAndReady checks if the head pod of the RayCluster is running and ready.
func (r *RayServiceReconciler) isHeadPodRunningAndReady(ctx context.Context, instance *rayv1.RayCluster) (bool, error) {
	headPod, err := common.GetRayClusterHeadPod(ctx, r, instance)
	if err != nil {
		return false, err
	}
	if headPod == nil {
		return false, fmt.Errorf("found 0 head. cluster name %s, namespace %v", instance.Name, instance.Namespace)
	}
	return utils.IsRunningAndReady(headPod), nil
}

func isServeAppUnhealthyOrDeployedFailed(appStatus string) bool {
	return appStatus == rayv1.ApplicationStatusEnum.UNHEALTHY || appStatus == rayv1.ApplicationStatusEnum.DEPLOY_FAILED
}

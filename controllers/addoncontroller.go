package controllers

import (
	"context"
	"fmt"
	"strings"

	operatorsv1 "github.com/operator-framework/api/pkg/operators/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"open-cluster-management.io/addon-framework/pkg/addonfactory"
	"open-cluster-management.io/addon-framework/pkg/addonmanager"
	"open-cluster-management.io/addon-framework/pkg/agent"
	addonframeworkutils "open-cluster-management.io/addon-framework/pkg/utils"
	addonapiv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	addonv1alpha1client "open-cluster-management.io/api/client/addon/clientset/versioned"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	workapiv1 "open-cluster-management.io/api/work/v1"

	volsyncaddonv1alpha1 "github.com/stolostron/volsync-addon-controller/api/v1alpha1"
)

//
// The main addon controller - uses the addon framework to deploy the volsync
// operator on a managed cluster if a ManagedClusterAddon CR exists in the
// cluster namespace on the hub.
//

var (
	genericScheme = runtime.NewScheme()
)

const (
	addonName                      = "volsync"
	OperatorName                   = "volsync-product"
	GlobalOperatorInstallNamespace = "openshift-operators"

	// Defaults for ACM-2.9
	DefaultCatalogSource          = "redhat-operators"
	DefaultCatalogSourceNamespace = "openshift-marketplace"
	DefaultChannel                = "stable-0.8" // No "acm-x.y" channel anymore - aligning ACM-2.9 with stable-0.8
	DefaultStartingCSV            = ""           // By default no starting CSV - will use the latest in the channel
	DefaultInstallPlanApproval    = "Automatic"
)

const (
	// Label on ManagedCluster - if this label is set to value "true" on a ManagedCluster resource on the hub then
	// the addon controller will automatically create a ManagedClusterAddOn for the managed cluster and thus
	// trigger the deployment of the volsync operator on that managed cluster
	ManagedClusterInstallVolSyncLabel      = "addons.open-cluster-management.io/volsync"
	ManagedClusterInstallVolSyncLabelValue = "true"

	// Annotations on the ManagedClusterAddOn for overriding operator settings (in the operator Subscription)
	AnnotationChannelOverride                = "operator-subscription-channel"
	AnnotationInstallPlanApprovalOverride    = "operator-subscription-installPlanApproval"
	AnnotationCatalogSourceOverride          = "operator-subscription-source"
	AnnotationCatalogSourceNamespaceOverride = "operator-subscription-sourceNamespace"
	AnnotationStartingCSVOverride            = "operator-subscription-startingCSV"
)

// Configurable Value Names
// These are the value names contained in the manifest yaml templates - to be replaced and set by user
// supplied values or defaults.
//
// User-supplied values will be specified by annotations on the ManagedClusterAddOn or by addonDeploymentConfigs
// or by volSyncAddonConfig
const (
	ConfigValue_OperatorName           = "OperatorName"
	ConfigValue_InstallNamespace       = "InstallNamespace"
	ConfigValue_CatalogSource          = "CatalogSource"
	ConfigValue_CatalogSourceNamespace = "CatalogSourceNamespace"
	ConfigValue_InstallPlanApproval    = "InstallPlanApproval"
	ConfigValue_Channel                = "Channel"
	ConfigValue_StartingCSV            = "StartingCSV"

	ConfigValue_ResourceRequests = "ResourceRequests"
	ConfigValue_ResourceLimits   = "ResourceLimits"

	ConfigValue_Tolerations  = "Tolerations"
	ConfigValue_NodeSelector = "NodeSelector"
)

func init() {
	utilruntime.Must(scheme.AddToScheme(genericScheme))
	utilruntime.Must(operatorsv1.AddToScheme(genericScheme))
	utilruntime.Must(operatorsv1alpha1.AddToScheme(genericScheme))
	utilruntime.Must(volsyncaddonv1alpha1.AddToScheme(genericScheme))
}

// Another agent with registration enabled.
type VolSyncAgent struct {
	addonClient      addonv1alpha1client.Interface
	controllerClient client.Client
}

func NewVolSyncAgent(addOnClient addonv1alpha1client.Interface, controllerClient client.Client) *VolSyncAgent {
	return &VolSyncAgent{addOnClient, controllerClient}
}

var _ agent.AgentAddon = &VolSyncAgent{}

func (h *VolSyncAgent) Manifests(cluster *clusterv1.ManagedCluster,
	addon *addonapiv1alpha1.ManagedClusterAddOn) ([]runtime.Object, error) {
	if !clusterSupportsAddonInstall(cluster) {
		klog.InfoS("Cluster is not OpenShift, not deploying addon", "addonName",
			addonName, "cluster", cluster.GetName())
		return []runtime.Object{}, nil
	}

	subscriptionObj, err := h.manifestObjectForVolsyncOperatorSubscription(cluster, addon)
	if subscriptionObj == nil || err != nil {
		return nil, err
	}

	// List of objects for manifest  will just be the 1 VolSync operator subscription
	return []runtime.Object{subscriptionObj}, nil
}

func (h *VolSyncAgent) GetAgentAddonOptions() agent.AgentAddonOptions {
	return agent.AgentAddonOptions{
		AddonName: addonName,
		//InstallStrategy: agent.InstallAllStrategy(operatorSuggestedNamespace),
		InstallStrategy: agent.InstallByLabelStrategy(
			"", /* this controller will ignore the ns in the spec so set to empty */
			metav1.LabelSelector{
				MatchLabels: map[string]string{
					ManagedClusterInstallVolSyncLabel: ManagedClusterInstallVolSyncLabelValue,
				},
			},
		),
		HealthProber: &agent.HealthProber{
			Type: agent.HealthProberTypeWork,
			WorkProber: &agent.WorkHealthProber{
				ProbeFields: []agent.ProbeField{
					{
						ResourceIdentifier: workapiv1.ResourceIdentifier{
							Group:     "operators.coreos.com",
							Resource:  "subscriptions",
							Name:      OperatorName,
							Namespace: getInstallNamespace(),
						},
						ProbeRules: []workapiv1.FeedbackRule{
							{
								Type: workapiv1.JSONPathsType,
								JsonPaths: []workapiv1.JsonPath{
									{
										Name: "installedCSV",
										Path: ".status.installedCSV",
									},
								},
							},
						},
					},
				},
				HealthCheck: subHealthCheck,
			},
		},
		SupportedConfigGVRs: []schema.GroupVersionResource{
			addonframeworkutils.AddOnDeploymentConfigGVR,
			volsyncaddonv1alpha1.VolSyncAddOnConfigGVR,
		},
	}
}

func subHealthCheck(identifier workapiv1.ResourceIdentifier, result workapiv1.StatusFeedbackResult) error {
	for _, feedbackValue := range result.Values {
		if feedbackValue.Name == "installedCSV" {
			klog.InfoS("Addon subscription", "installedCSV", feedbackValue.Value)
			if feedbackValue.Value.Type != workapiv1.String || feedbackValue.Value.String == nil ||
				!strings.HasPrefix(*feedbackValue.Value.String, OperatorName) {

				installedCSVErr := fmt.Errorf("addon subscription has unexpected installedCSV value")
				klog.ErrorS(installedCSVErr, "Sub may not have installed CSV")
				return installedCSVErr
			}
		}
	}
	klog.InfoS("health check successful")
	return nil
}

// returns the runtime.Object for the volsync operator subscription that should get embedded into a manifest
func (h *VolSyncAgent) manifestObjectForVolsyncOperatorSubscription(cluster *clusterv1.ManagedCluster,
	addon *addonapiv1alpha1.ManagedClusterAddOn) (runtime.Object, error) {
	// Get volsyncAddOnConfig if present (may be specified globally or per cluster)
	vsAddOnConfig, err := h.getVolSyncAddOnConfig(addon)
	if err != nil {
		return nil, err
	}
	var vsAddOnConfigSpec *volsyncaddonv1alpha1.VolSyncAddOnConfigSpec
	if vsAddOnConfig != nil {
		vsAddOnConfigSpec = vsAddOnConfig.Spec
	}

	// Get addOnDeploymentConfig if present (may be specified globally or per cluster)
	// Getting the "values" here - we're concerned with NodeSelector and Tolerations
	deploymentConfigValues, err := addonfactory.GetAddOnDeploymentConfigValues(
		addonfactory.NewAddOnDeploymentConfigGetter(h.addonClient),
		addonfactory.ToAddOnDeploymentConfigValues,
	)(cluster, addon)
	if err != nil {
		return nil, err
	}

	volsyncOperatorSubscription, err := h.VolsyncOperatorSubscription(addon, vsAddOnConfigSpec, deploymentConfigValues)
	if err != nil {
		return nil, err
	}

	// Note that the version/kind doesn't get set, so manually set it
	gvks, _, _ := h.controllerClient.Scheme().ObjectKinds(volsyncOperatorSubscription)
	for _, gvk := range gvks {
		if gvk.Kind != "" && gvk.Version != "" {
			volsyncOperatorSubscription.GetObjectKind().SetGroupVersionKind(gvk)
			break
		}
	}

	return volsyncOperatorSubscription, nil
}

func (h *VolSyncAgent) VolsyncOperatorSubscription(addon *addonapiv1alpha1.ManagedClusterAddOn,
	vsAddOnConfigSpec *volsyncaddonv1alpha1.VolSyncAddOnConfigSpec,
	deploymentConfigValues addonfactory.Values) (*operatorsv1alpha1.Subscription, error) {
	// Operator Subscription
	operatorSubscription := &operatorsv1alpha1.Subscription{
		ObjectMeta: metav1.ObjectMeta{
			Name:      OperatorName,
			Namespace: getInstallNamespace(),
		},
		Spec: &operatorsv1alpha1.SubscriptionSpec{
			Package:                OperatorName,
			CatalogSource:          getCatalogSource(addon, vsAddOnConfigSpec),
			CatalogSourceNamespace: getCatalogSourceNamespace(addon, vsAddOnConfigSpec),
			Channel:                getChannel(addon, vsAddOnConfigSpec),
			InstallPlanApproval:    getInstallPlanApproval(addon, vsAddOnConfigSpec),
			StartingCSV:            getStartingCSV(addon, vsAddOnConfigSpec),
			Config:                 getSubscriptionConfig(vsAddOnConfigSpec, deploymentConfigValues),
		},
	}

	return operatorSubscription, nil
}

func (h *VolSyncAgent) getVolSyncAddOnConfig(addon *addonapiv1alpha1.ManagedClusterAddOn) (*volsyncaddonv1alpha1.VolSyncAddOnConfig, error) {
	// If multiple volSyncAddOnConfigs, the last one in the list will take precedence
	for _, config := range addon.Status.ConfigReferences {
		if config.ConfigGroupResource.Group != volsyncaddonv1alpha1.GroupVersion.Group ||
			config.ConfigGroupResource.Resource != volsyncaddonv1alpha1.VolSyncAddOnConfigGroupVersionResourcePlural {
			continue
		}

		vsAddOnConfig := &volsyncaddonv1alpha1.VolSyncAddOnConfig{}
		if err := h.controllerClient.Get(context.Background(),
			types.NamespacedName{Name: config.Name, Namespace: config.Namespace}, vsAddOnConfig); err != nil {
			return nil, err
		}
		return vsAddOnConfig, nil
	}

	return nil, nil
}

func getInstallNamespace() string {
	// The only namespace supported is openshift-operators, so ignore whatever is in the spec
	return GlobalOperatorInstallNamespace
}

// These getters will get the value from:
// 1. The VolSyncAddonConfig if there is one.
// 2. If not set, will fallback to getting it from the annotation on the ManagedClusterAddOn
// 3. If still not set, will use the default value
func getCatalogSource(addon *addonapiv1alpha1.ManagedClusterAddOn,
	volSyncAddOnConfigSpec *volsyncaddonv1alpha1.VolSyncAddOnConfigSpec) string {
	// Get value from volsyncaddonconfigspec if set
	if volSyncAddOnConfigSpec != nil && volSyncAddOnConfigSpec.SubscriptionCatalogSource != "" {
		return volSyncAddOnConfigSpec.SubscriptionCatalogSource
	}
	// Get value from the managedclusteraddon annotation next, or fallback to default value
	return getAnnotationOverrideOrDefault(addon, AnnotationCatalogSourceOverride, DefaultCatalogSource)
}
func getCatalogSourceNamespace(addon *addonapiv1alpha1.ManagedClusterAddOn,
	volSyncAddOnConfigSpec *volsyncaddonv1alpha1.VolSyncAddOnConfigSpec) string {
	if volSyncAddOnConfigSpec != nil && volSyncAddOnConfigSpec.SubscriptionCatalogSourceNamespace != "" {
		return volSyncAddOnConfigSpec.SubscriptionCatalogSourceNamespace
	}
	return getAnnotationOverrideOrDefault(addon, AnnotationCatalogSourceNamespaceOverride,
		DefaultCatalogSourceNamespace)
}
func getChannel(addon *addonapiv1alpha1.ManagedClusterAddOn,
	volSyncAddOnConfigSpec *volsyncaddonv1alpha1.VolSyncAddOnConfigSpec) string {
	if volSyncAddOnConfigSpec != nil && volSyncAddOnConfigSpec.SubscriptionChannel != "" {
		return volSyncAddOnConfigSpec.SubscriptionChannel
	}
	return getAnnotationOverrideOrDefault(addon, AnnotationChannelOverride, DefaultChannel)
}
func getInstallPlanApproval(addon *addonapiv1alpha1.ManagedClusterAddOn,
	volSyncAddOnConfigSpec *volsyncaddonv1alpha1.VolSyncAddOnConfigSpec) operatorsv1alpha1.Approval {
	if volSyncAddOnConfigSpec != nil &&
		volSyncAddOnConfigSpec.SubscriptionInstallPlanApproval != nil &&
		*volSyncAddOnConfigSpec.SubscriptionInstallPlanApproval != "" {
		return *volSyncAddOnConfigSpec.SubscriptionInstallPlanApproval
	}
	return operatorsv1alpha1.Approval(
		getAnnotationOverrideOrDefault(addon, AnnotationInstallPlanApprovalOverride, DefaultInstallPlanApproval))
}
func getStartingCSV(addon *addonapiv1alpha1.ManagedClusterAddOn,
	volSyncAddOnConfigSpec *volsyncaddonv1alpha1.VolSyncAddOnConfigSpec) string {
	if volSyncAddOnConfigSpec != nil && volSyncAddOnConfigSpec.SubscriptionStartingCSV != "" {
		return volSyncAddOnConfigSpec.SubscriptionStartingCSV
	}
	return getAnnotationOverrideOrDefault(addon, AnnotationStartingCSVOverride, DefaultStartingCSV)
}

func getSubscriptionConfig(volSyncAddOnConfigSpec *volsyncaddonv1alpha1.VolSyncAddOnConfigSpec,
	deploymentConfigValues addonfactory.Values) *operatorsv1alpha1.SubscriptionConfig {
	var subscriptionConfig *operatorsv1alpha1.SubscriptionConfig

	// Use settings from the volSyncAddOnConfig if set
	if volSyncAddOnConfigSpec != nil && volSyncAddOnConfigSpec.SubscriptionConfig != nil {
		subscriptionConfig = volSyncAddOnConfigSpec.SubscriptionConfig
	}

	// If nodeSelector or Tolerations are set in the deploymentConfig, then use these values instead
	// of values in the volSyncAddOnConfig (deploymentConfig takes precedence)
	if deploymentConfigValues != nil {
		nodeSelectorFromDeploymentConfig, ok := deploymentConfigValues["NodeSelector"]
		if ok && nodeSelectorFromDeploymentConfig != nil {
			if subscriptionConfig == nil {
				subscriptionConfig = &operatorsv1alpha1.SubscriptionConfig{}
			}
			subscriptionConfig.NodeSelector = nodeSelectorFromDeploymentConfig.(map[string]string)
		}
		tolerationsFromDeploymentConfig, ok := deploymentConfigValues["Tolerations"]
		if ok && tolerationsFromDeploymentConfig != nil {
			if subscriptionConfig == nil {
				subscriptionConfig = &operatorsv1alpha1.SubscriptionConfig{}
			}
			subscriptionConfig.Tolerations = tolerationsFromDeploymentConfig.([]corev1.Toleration)
		}
	}

	return subscriptionConfig
}

func getAnnotationOverrideOrDefault(addon *addonapiv1alpha1.ManagedClusterAddOn,
	annotationName, defaultValue string) string {
	// Allow to be overriden with an annotation
	annotationOverride, ok := addon.Annotations[annotationName]
	if ok && annotationOverride != "" {
		return annotationOverride
	}
	return defaultValue
}

func clusterSupportsAddonInstall(cluster *clusterv1.ManagedCluster) bool {
	vendor, ok := cluster.Labels["vendor"]
	if !ok || !strings.EqualFold(vendor, "OpenShift") {
		return false
	}
	return true
}

func StartControllers(ctx context.Context, config *rest.Config) error {
	addonClient, err := addonv1alpha1client.NewForConfig(config)
	if err != nil {
		return err
	}

	// Direct client, no caching
	controllerClient, err := client.New(config, client.Options{Scheme: genericScheme})
	if err != nil {
		return err
	}

	mgr, err := addonmanager.New(config)
	if err != nil {
		return err
	}
	err = mgr.AddAgent(&VolSyncAgent{addonClient, controllerClient})
	if err != nil {
		return err
	}

	err = mgr.Start(ctx)
	if err != nil {
		return err
	}

	<-ctx.Done()

	return nil
}

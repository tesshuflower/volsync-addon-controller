package controllers

import (
	"context"
	"embed"
	"fmt"
	"strings"

	"github.com/openshift/library-go/pkg/assets"
	operatorsv1 "github.com/operator-framework/api/pkg/operators/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
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
	genericCodecs = serializer.NewCodecFactory(genericScheme)
	genericCodec  = genericCodecs.UniversalDeserializer()
)

const (
	addonName                      = "volsync"
	operatorName                   = "volsync-product"
	globalOperatorInstallNamespace = "openshift-operators"

	// Defaults for ACM-2.8
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

//go:embed manifests
var fs embed.FS

// If operator is deployed to a single namespace, the Namespace, OperatorGroup (and role to create the operatorgroup)
// is required, along with the Subscription for the operator
// This particular operator is deploying into all namespaces, but into a specific target namespace
// (Requires the annotation  operatorframework.io/suggested-namespace: "mynamespace"  to be set on the operator CSV)
var manifestFilesAllNamespacesInstallIntoSuggestedNamespace = []string{
	"manifests/operatorgroup-aggregate-clusterrole.yaml",
	"manifests/operator-namespace.yaml",
	"manifests/operator-group-allnamespaces.yaml",
	"manifests/operator-subscription.yaml",
}

// Use these manifest files if deploying an operator into own namespace
//var manifestFilesOwnNamepace = []string{
//	"manifests/operatorgroup-aggregate-clusterrole.yaml",
//	"manifests/operator-namespace.yaml",
//	"manifests/operator-group-ownnamespace.yaml",
//	"manifests/operator-subscription.yaml",
//}

// If operator is deployed to a all namespaces and the operator wil be deployed into the global operators namespace
// (openshift-operators on OCP), the only thing needed is the Subscription for the operator
var manifestFilesAllNamespaces = []string{
	"manifests/operator-subscription.yaml",
}

// Another agent with registration enabled.
type volsyncAgent struct {
	addonClient      addonv1alpha1client.Interface
	controllerClient client.Client
}

var _ agent.AgentAddon = &volsyncAgent{}

func (h *volsyncAgent) Manifests(cluster *clusterv1.ManagedCluster,
	addon *addonapiv1alpha1.ManagedClusterAddOn) ([]runtime.Object, error) {
	if !clusterSupportsAddonInstall(cluster) {
		klog.InfoS("Cluster is not OpenShift, not deploying addon", "addonName",
			addonName, "cluster", cluster.GetName())
		return []runtime.Object{}, nil
	}

	objects := []runtime.Object{}
	for _, file := range getManifestFileList(addon) {
		object, err := h.loadManifestFromFile(file, cluster, addon)
		if err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	return objects, nil
}

func (h *volsyncAgent) GetAgentAddonOptions() agent.AgentAddonOptions {
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
							Name:      operatorName,
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
				!strings.HasPrefix(*feedbackValue.Value.String, operatorName) {

				installedCSVErr := fmt.Errorf("addon subscription has unexpected installedCSV value")
				klog.ErrorS(installedCSVErr, "Sub may not have installed CSV")
				return installedCSVErr
			}
		}
	}
	klog.InfoS("health check successful")
	return nil
}

func (h *volsyncAgent) loadManifestFromFile(file string, cluster *clusterv1.ManagedCluster,
	addon *addonapiv1alpha1.ManagedClusterAddOn) (runtime.Object, error) {

	// Base values from defaults (or overridden by annotations on the ManagedClusterAddOn)
	baseConfigValues := addonfactory.Values{
		ConfigValue_OperatorName:           operatorName,
		ConfigValue_InstallNamespace:       getInstallNamespace(),
		ConfigValue_CatalogSource:          getCatalogSource(addon),
		ConfigValue_CatalogSourceNamespace: getCatalogSourceNamespace(addon),
		ConfigValue_InstallPlanApproval:    getInstallPlanApproval(addon),
		ConfigValue_Channel:                getChannel(addon),
		ConfigValue_StartingCSV:            getStartingCSV(addon),
	}

	//
	// Get values from addOnDeploymentConfig
	//
	deploymentConfigValues, err := addonfactory.GetAddOnDeploymentConfigValues(
		addonfactory.NewAddOnDeploymentConfigGetter(h.addonClient),
		addonfactory.ToAddOnDeploymentConfigValues,
	)(cluster, addon)
	if err != nil {
		return nil, err
	}
	mergedValues := addonfactory.MergeValues(baseConfigValues, deploymentConfigValues)

	// Especially if the config gets more complicated - consider just creating a subscription resource and
	// replacing the values we want rather than dealing with the go template in controllers/manifests/operator-subscription.yaml
	//
	// Get values from volSyncAddOnConfig
	//
	volSyncAddOnConfigValues, err := getVolSyncAddOnConfigValues(h.controllerClient, addon)
	if err != nil {
		return nil, err
	}
	finalMergedValues := addonfactory.MergeValues(mergedValues, volSyncAddOnConfigValues)

	template, err := fs.ReadFile(file)
	if err != nil {
		return nil, err
	}

	raw := assets.MustCreateAssetFromTemplate(file, template, &finalMergedValues).Data
	object, _, err := genericCodec.Decode(raw, nil, nil)
	if err != nil {
		klog.ErrorS(err, "Error decoding manifest file", "filename", file)
		return nil, err
	}
	return object, nil
}

func getVolSyncAddOnConfigValues(controllerClient client.Client,
	addon *addonapiv1alpha1.ManagedClusterAddOn) (addonfactory.Values, error) {
	var lastValues = addonfactory.Values{}
	// If multiple volSyncAddOnConfigs, the last one in the list will take precedence
	for _, config := range addon.Status.ConfigReferences {
		if config.ConfigGroupResource.Group != volsyncaddonv1alpha1.GroupVersion.Group ||
			config.ConfigGroupResource.Resource != volsyncaddonv1alpha1.VolSyncAddOnConfigGroupVersionResourcePlural {
			continue
		}

		vsAddOnConfig := &volsyncaddonv1alpha1.VolSyncAddOnConfig{}
		if err := controllerClient.Get(context.Background(),
			types.NamespacedName{Name: config.Name, Namespace: config.Namespace}, vsAddOnConfig); err != nil {
			return nil, err
		}

		values, err := volSyncAddOnConfigToValues(*vsAddOnConfig)
		if err != nil {
			return nil, err
		}
		lastValues = addonfactory.MergeValues(lastValues, values)
	}

	return lastValues, nil
}

func volSyncAddOnConfigToValues(vsAddOnConfig volsyncaddonv1alpha1.VolSyncAddOnConfig) (addonfactory.Values, error) {
	subSpec := vsAddOnConfig.Spec
	if subSpec == nil {
		return addonfactory.Values{}, nil
	}

	valuesFromVSAddOnConfig := addonfactory.Values{}

	setValueIfDefined(valuesFromVSAddOnConfig, ConfigValue_CatalogSource, subSpec.SubscriptionCatalogSource)
	setValueIfDefined(valuesFromVSAddOnConfig, ConfigValue_CatalogSourceNamespace,
		subSpec.SubscriptionCatalogSourceNamespace)
	// Not allowing override of "Package"
	setValueIfDefined(valuesFromVSAddOnConfig, ConfigValue_Channel, subSpec.SubscriptionChannel)
	setValueIfDefined(valuesFromVSAddOnConfig, ConfigValue_StartingCSV, subSpec.SubscriptionStartingCSV)
	setValueIfDefined(valuesFromVSAddOnConfig, ConfigValue_InstallPlanApproval, string(subSpec.SubscriptionInstallPlanApproval))

	// Now look for overrides from subscriptionConfig (if defined)
	subSpecConfig := subSpec.SubscriptionConfig
	if subSpecConfig == nil {
		// Then we're done, return
		return valuesFromVSAddOnConfig, nil
	}

	subSpecConfigResources := subSpecConfig.Resources
	if subSpecConfigResources != nil {
		setValueIfDefined(valuesFromVSAddOnConfig, ConfigValue_ResourceRequests, subSpecConfigResources.Requests)
		setValueIfDefined(valuesFromVSAddOnConfig, ConfigValue_ResourceLimits, subSpecConfigResources.Limits)
	}

	// These will override values in addonDeploymentConfig if specified
	setValueIfDefined(valuesFromVSAddOnConfig, ConfigValue_Tolerations, subSpecConfig.Tolerations)
	setValueIfDefined(valuesFromVSAddOnConfig, ConfigValue_NodeSelector, subSpecConfig.NodeSelector)

	// Not currently doing any overrides on:
	// - Resources.Claims
	// - Selector
	// - EnvFrom
	// - Env
	// - Volumes
	// - VolumeMounts
	// - Affinity

	return valuesFromVSAddOnConfig, nil
}

// Update currentValues if value is not the empty value
func setValueIfDefined(currentValues addonfactory.Values, valueName string, value interface{}) {
	switch v := value.(type) {
	case string:
		if v != "" {
			currentValues[valueName] = v
		}
	case corev1.ResourceList:
		if v != nil {
			currentValues[valueName] = v
		}
	case []corev1.Toleration:
		if v != nil {
			currentValues[valueName] = v
		}
	case map[string]string:
		if v != nil {
			currentValues[valueName] = v
		}
	}
}

func getManifestFileList(addon *addonapiv1alpha1.ManagedClusterAddOn) []string {
	installNamespace := getInstallNamespace()
	if installNamespace == globalOperatorInstallNamespace {
		// Do not need to create an operator group, namespace etc if installing into the global operator ns
		return manifestFilesAllNamespaces
	}
	return manifestFilesAllNamespacesInstallIntoSuggestedNamespace
}

func getInstallNamespace() string {
	// The only namespace supported is openshift-operators, so ignore whatever is in the spec
	return globalOperatorInstallNamespace
}

func getCatalogSource(addon *addonapiv1alpha1.ManagedClusterAddOn) string {
	return getAnnotationOverrideOrDefault(addon, AnnotationCatalogSourceOverride, DefaultCatalogSource)
}

func getCatalogSourceNamespace(addon *addonapiv1alpha1.ManagedClusterAddOn) string {
	return getAnnotationOverrideOrDefault(addon, AnnotationCatalogSourceNamespaceOverride,
		DefaultCatalogSourceNamespace)
}

func getInstallPlanApproval(addon *addonapiv1alpha1.ManagedClusterAddOn) string {
	return getAnnotationOverrideOrDefault(addon, AnnotationInstallPlanApprovalOverride, DefaultInstallPlanApproval)
}

func getChannel(addon *addonapiv1alpha1.ManagedClusterAddOn) string {
	return getAnnotationOverrideOrDefault(addon, AnnotationChannelOverride, DefaultChannel)
}

func getStartingCSV(addon *addonapiv1alpha1.ManagedClusterAddOn) string {
	return getAnnotationOverrideOrDefault(addon, AnnotationStartingCSVOverride, DefaultStartingCSV)
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
	err = mgr.AddAgent(&volsyncAgent{addonClient, controllerClient})
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

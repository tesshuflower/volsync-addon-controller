package controllers

import (
	"embed"
	"fmt"
	"strings"

	"github.com/openshift/library-go/pkg/assets"
	operatorsv1 "github.com/operator-framework/api/pkg/operators/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"

	"open-cluster-management.io/addon-framework/pkg/addonfactory"
	"open-cluster-management.io/addon-framework/pkg/agent"
	addonframeworkutils "open-cluster-management.io/addon-framework/pkg/utils"
	addonapiv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	addonv1alpha1client "open-cluster-management.io/api/client/addon/clientset/versioned"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	workapiv1 "open-cluster-management.io/api/work/v1"
	appsubscriptionv1 "open-cluster-management.io/multicloud-operators-subscription/pkg/apis/apps/v1"
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

// Change these values to suit your operator
const (
	addonName                      = "volsync"
	operatorName                   = "volsync-product"
	globalOperatorInstallNamespace = "openshift-operators"

	// Defaults for ACM-2.12
	DefaultCatalogSource          = "redhat-operators"
	DefaultCatalogSourceNamespace = "openshift-marketplace"
	DefaultChannel                = "stable-0.11" // No "acm-x.y" channel anymore - aligning ACM-2.12 with stable-0.11
	DefaultStartingCSV            = ""            // By default no starting CSV - will use the latest in the channel
	DefaultInstallPlanApproval    = "Automatic"

	DefaultHelmSource         = "https://backube.github.io/helm-charts"
	DefaultHelmChartName      = "volsync"
	DefaultHelmPackageVersion = "0.10" //FIXME: update
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

func init() {
	utilruntime.Must(scheme.AddToScheme(genericScheme))
	utilruntime.Must(operatorsv1.AddToScheme(genericScheme))
	utilruntime.Must(operatorsv1alpha1.AddToScheme(genericScheme))
	utilruntime.Must(appsubscriptionv1.SchemeBuilder.AddToScheme(genericScheme))
}

//go:embed manifests
var fs embed.FS

/*
// If operator is deployed to a single namespace, the Namespace, OperatorGroup (and role to create the operatorgroup)
// is required, along with the Subscription for the operator
// This particular operator is deploying into all namespaces, but into a specific target namespace
// (Requires the annotation  operatorframework.io/suggested-namespace: "mynamespace"  to be set on the operator CSV)
var manifestFilesAllNamespacesInstallIntoSuggestedNamespace = []string{
	"manifests/operator/operatorgroup-aggregate-clusterrole.yaml",
	"manifests/operator/operator-namespace.yaml",
	"manifests/operator/operator-group-allnamespaces.yaml",
	"manifests/operator/operator-subscription.yaml",
}
*/

// If operator is deployed to a all namespaces and the operator wil be deployed into the global operators namespace
// (openshift-operators on OCP), the only thing needed is the Subscription for the operator
var manifestFilesAllNamespaces = []string{
	"manifests/operator/operator-subscription.yaml",
}

var manifestFilesHelmChartInstall = []string{
	"manifests/helm-chart/namespace.yaml",
	"manifests/helm-chart/subscription.yaml",
	//FIXME:how to handle the channel?  "manifests/helm-chart/
}

// Another agent with registration enabled.
type volsyncAgent struct {
	addonClient addonv1alpha1client.Interface
}

var _ agent.AgentAddon = &volsyncAgent{}

func (h *volsyncAgent) Manifests(cluster *clusterv1.ManagedCluster,
	addon *addonapiv1alpha1.ManagedClusterAddOn,
) ([]runtime.Object, error) {
	/*
		if !clusterSupportsAddonInstall(cluster) {
			klog.InfoS("Cluster is not OpenShift, not deploying addon", "addonName",
				addonName, "cluster", cluster.GetName())
			return []runtime.Object{}, nil
		}
	*/

	isClusterOpenShift := isOpenShift(cluster)

	objects := []runtime.Object{}
	for _, file := range getManifestFileList(addon, isClusterOpenShift) {
		klog.InfoS("File for manifest: ", "file name", file) //TODO: remove
		object, err := h.loadManifestFromFile(file, addon, cluster, isClusterOpenShift)
		if err != nil {
			return nil, err
		}
		klog.InfoS("Object for manifest: ", "object", object) //TODO: remove
		objects = append(objects, object)
	}
	return objects, nil
}

func (h *volsyncAgent) GetAgentAddonOptions() agent.AgentAddonOptions {
	return agent.AgentAddonOptions{
		AddonName: addonName,
		HealthProber: &agent.HealthProber{
			//FIXME: how to do this for the helm chart?
			Type: agent.HealthProberTypeWork,
			WorkProber: &agent.WorkHealthProber{
				ProbeFields: []agent.ProbeField{
					{
						ResourceIdentifier: workapiv1.ResourceIdentifier{
							Group:    "operators.coreos.com",
							Resource: "subscriptions",
							Name:     operatorName,
							//Namespace: getInstallNamespace(),
							Namespace: "openshift-operators", //FIXME:
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

func (h *volsyncAgent) loadManifestFromFile(file string, addon *addonapiv1alpha1.ManagedClusterAddOn,
	cluster *clusterv1.ManagedCluster, isClusterOpenShift bool,
) (runtime.Object, error) {
	manifestConfig := struct {
		InstallNamespace string

		// OpenShift target cluster parameters - for OLM operator install of VolSync
		OperatorName           string
		OperatorGroupSpec      string
		CatalogSource          string
		CatalogSourceNamespace string
		InstallPlanApproval    string
		Channel                string
		StartingCSV            string

		// Helm based install parameters for non-OpenShift target clusters
		HelmSource         string
		HelmChartName      string
		HelmPackageVersion string
	}{
		InstallNamespace: getInstallNamespace(addon, isClusterOpenShift),

		OperatorName:           operatorName,
		CatalogSource:          getCatalogSource(addon),
		CatalogSourceNamespace: getCatalogSourceNamespace(addon),
		InstallPlanApproval:    getInstallPlanApproval(addon),
		Channel:                getChannel(addon),
		StartingCSV:            getStartingCSV(addon),

		HelmSource:         getHelmSource(addon),
		HelmChartName:      getHelmChartName(addon),
		HelmPackageVersion: getHelmPackageVersion(addon),
	}

	manifestConfigValues := addonfactory.StructToValues(manifestConfig)

	// Get values from addonDeploymentConfig
	deploymentConfigValues, err := addonfactory.GetAddOnDeploymentConfigValues(
		addonframeworkutils.NewAddOnDeploymentConfigGetter(h.addonClient),
		addonfactory.ToAddOnDeploymentConfigValues,
	)(cluster, addon)
	if err != nil {
		return nil, err
	}

	// Merge manifestConfig and deploymentConfigValues
	mergedValues := addonfactory.MergeValues(manifestConfigValues, deploymentConfigValues)

	template, err := fs.ReadFile(file)
	if err != nil {
		return nil, err
	}

	raw := assets.MustCreateAssetFromTemplate(file, template, &mergedValues).Data
	object, _, err := genericCodec.Decode(raw, nil, nil)
	if err != nil {
		klog.ErrorS(err, "Error decoding manifest file", "filename", file)
		return nil, err
	}
	return object, nil
}

func getManifestFileList(_ *addonapiv1alpha1.ManagedClusterAddOn, isClusterOpenShift bool) []string {
	if isClusterOpenShift {
		/*
				installNamespace := getInstallNamespace()
				if installNamespace == globalOperatorInstallNamespace {
					//TODO: remove this old stuff
					// Do not need to create an operator group, namespace etc if installing into the global operator ns
					return manifestFilesAllNamespaces
				}
			return manifestFilesAllNamespacesInstallIntoSuggestedNamespace
		*/
		return manifestFilesAllNamespaces
	}

	// Non OpenShift cluster - use manifestwork containing helm chart namespace/subscription
	return manifestFilesHelmChartInstall
}

func getInstallNamespace(addon *addonapiv1alpha1.ManagedClusterAddOn, isClusterOpenShift bool) string {
	if isClusterOpenShift {
		// The only namespace supported is openshift-operators, so ignore whatever is in the spec
		return globalOperatorInstallNamespace
	}

	// non-OpenShift target cluster
	installNamespace := "volsync-system" // Default value //TODO: make a constant
	if addon.Spec.InstallNamespace != "" && addon.Spec.InstallNamespace != "default" {
		installNamespace = addon.Spec.InstallNamespace
	}
	return installNamespace
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

func getHelmSource(_ *addonapiv1alpha1.ManagedClusterAddOn) string {
	//TODO: allow overriding with annotations?
	return DefaultHelmSource
}

func getHelmChartName(_ *addonapiv1alpha1.ManagedClusterAddOn) string {
	return DefaultHelmChartName
}

func getHelmPackageVersion(_ *addonapiv1alpha1.ManagedClusterAddOn) string {
	//TODO: allow overriding with annotations?
	return DefaultHelmPackageVersion
}

func getAnnotationOverrideOrDefault(addon *addonapiv1alpha1.ManagedClusterAddOn,
	annotationName, defaultValue string,
) string {
	// Allow to be overridden with an annotation
	annotationOverride, ok := addon.Annotations[annotationName]
	if ok && annotationOverride != "" {
		return annotationOverride
	}
	return defaultValue
}

func isOpenShift(cluster *clusterv1.ManagedCluster) bool {
	vendor, ok := cluster.Labels["vendor"]
	if !ok || !strings.EqualFold(vendor, "OpenShift") {
		return false
	}

	//FIXME: this is just for test purposes
	_, notOS := cluster.Labels["notopenshift"]
	if notOS {
		klog.InfoS("Cluster has our fake notopenshift label", "cluster name", cluster.GetName())
		return false
	}
	//end FIXME: this is just for test purposes

	return true
}

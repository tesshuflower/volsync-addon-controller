package controllers

import (
	"embed"
	"fmt"
	"strings"

	operatorsv1 "github.com/operator-framework/api/pkg/operators/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"

	"open-cluster-management.io/addon-framework/pkg/agent"
	addonframeworkutils "open-cluster-management.io/addon-framework/pkg/utils"
	addonapiv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	addonv1alpha1client "open-cluster-management.io/api/client/addon/clientset/versioned"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	workapiv1 "open-cluster-management.io/api/work/v1"

	//helmreleasev1 "open-cluster-management.io/multicloud-operators-subscription/pkg/apis/apps/helmrelease/v1"
	//appsubscriptionv1 "open-cluster-management.io/multicloud-operators-subscription/pkg/apis/apps/v1"
	policyv1beta1 "open-cluster-management.io/config-policy-controller/api/v1beta1"
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

	// Defaults for ACM-2.13 Operator deploy
	DefaultCatalogSource          = "redhat-operators"
	DefaultCatalogSourceNamespace = "openshift-marketplace"
	DefaultChannel                = "stable-0.12" // aligning ACM-2.13 with stable-0.12
	DefaultStartingCSV            = ""            // By default no starting CSV - will use the latest in the channel
	DefaultInstallPlanApproval    = "Automatic"

	// Defaults for ACM-2.13 helm-based deploy
	DefaultHelmChartKey         = DefaultChannel // named the same as our operator channel
	DefaultHelmChartName        = "volsync"
	DefaultHelmInstallNamespace = "volsync-system"
)

const (
	// Label on ManagedCluster - if this label is set to value "true" on a ManagedCluster resource on the hub then
	// the addon controller will automatically create a ManagedClusterAddOn for the managed cluster and thus
	// trigger the deployment of the volsync operator on that managed cluster
	ManagedClusterInstallVolSyncLabel      = "addons.open-cluster-management.io/volsync"
	ManagedClusterInstallVolSyncLabelValue = "true"
)

const (
	// Annotations on the ManagedClusterAddOn for overriding operator settings (in the operator Subscription)
	AnnotationChannelOverride                = "operator-subscription-channel"
	AnnotationInstallPlanApprovalOverride    = "operator-subscription-installPlanApproval"
	AnnotationCatalogSourceOverride          = "operator-subscription-source"
	AnnotationCatalogSourceNamespaceOverride = "operator-subscription-sourceNamespace"
	AnnotationStartingCSVOverride            = "operator-subscription-startingCSV"
)

const (
	AnnotationVolSyncAddonDeployTypeOverride = "volsync-addon-deploy-type"
	//AnnotationVolSyncAddonDeployTypeOverrideHelmValue = "helm"
	AnnotationVolSyncAddonDeployTypeOverrideOLMValue = "olm"

	AnnotationHelmChartKey = "helm-chart-key" //TODO: come up with a better name?
)

func init() {
	utilruntime.Must(scheme.AddToScheme(genericScheme))
	utilruntime.Must(operatorsv1.AddToScheme(genericScheme))
	utilruntime.Must(operatorsv1alpha1.AddToScheme(genericScheme))
	//utilruntime.Must(appsubscriptionv1.SchemeBuilder.AddToScheme(genericScheme)) //TODO: remove if we don't use it
	//utilruntime.Must(helmreleasev1.SchemeBuilder.AddToScheme(genericScheme))     //TODO: remove if we don't use it
	utilruntime.Must(apiextensionsv1.AddToScheme(genericScheme))
	utilruntime.Must(policyv1beta1.AddToScheme(genericScheme))
}

//go:embed manifests
var embedFS embed.FS

// If operator is deployed to a all namespaces and the operator wil be deployed into the global operators namespace
// (openshift-operators on OCP), the only thing needed is the Subscription for the operator
var manifestFilesOperatorDeploy = []string{
	"manifests/operator/operator-subscription.yaml",
}

var manifestFilesHelmDeploy = []string{
	"manifests/helm-chart/namespace.yaml",
}

var manifestFilesHelmDeployOpenShift = []string{
	// Policy to remove the operator since we're going to deploy as a helm chart instead
	"manifests/helm-chart/volsync-operatorpolicy-aggregate-clusterrole.yaml",
	"manifests/helm-chart/volsync-operatorpolicy-remove-operator.yaml",
	"manifests/helm-chart/namespace.yaml",
}

// Another agent with registration enabled.
type volsyncAgent struct {
	addonClient addonv1alpha1client.Interface
}

var _ agent.AgentAddon = &volsyncAgent{}

func (h *volsyncAgent) Manifests(cluster *clusterv1.ManagedCluster,
	addon *addonapiv1alpha1.ManagedClusterAddOn,
) ([]runtime.Object, error) {
	mh := getManifestHelper(embedFS, h.addonClient, cluster, addon)
	return mh.loadManifests()
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
							Group:     "operators.coreos.com",
							Resource:  "subscriptions",
							Name:      operatorName,
							Namespace: "openshift-operators",
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
					{
						ResourceIdentifier: workapiv1.ResourceIdentifier{
							Group:     appsv1.GroupName,
							Resource:  "deployments",
							Name:      "volsync",
							Namespace: "*",
							//Namespace: "volsync-system",
						},
						ProbeRules: []workapiv1.FeedbackRule{
							{
								Type: workapiv1.WellKnownStatusType,
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
	klog.InfoS("## Sub health check ##", "identifier", identifier, "result", result) //TODO: remove
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
		//TODO: deployment check (for helm chart deploy)
		// TODO: if feedbackValue is the helm chart deploy

		/*
			// only support deployments and daemonsets for now
			if identifier.Resource != "deployments" {
				return fmt.Errorf("unsupported resource type %s", identifier.Resource)
			}
			if identifier.Group != appsv1.GroupName {
				return fmt.Errorf("unsupported resource group %s", identifier.Group)
			}
			if len(result.Values) == 0 {
				return fmt.Errorf("no values are probed for %s %s/%s",
					identifier.Resource, identifier.Namespace, identifier.Name)
			}

			readyReplicas := -1
			desiredNumberReplicas := -1
			for _, value := range result.Values {
				if value.Name == "ReadyReplicas" {
					readyReplicas = int(*value.Value.Integer)
				}
				if value.Name == "Replicas" {
					desiredNumberReplicas = int(*value.Value.Integer)
				}
			}

			if readyReplicas == -1 {
				return fmt.Errorf("readyReplica is not probed")
			}
			if desiredNumberReplicas == -1 {
				return fmt.Errorf("desiredNumberReplicas is not probed")
			}

			if desiredNumberReplicas == 0 || readyReplicas >= 1 {
				return nil
			}

			return fmt.Errorf("desiredNumberReplicas is %d but readyReplica is %d for %s %s/%s",
				desiredNumberReplicas, readyReplicas, identifier.Resource, identifier.Namespace, identifier.Name)
		*/
	}
	klog.InfoS("health check successful")
	return nil
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

	return true
}

package controllers

import (
	"embed"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/openshift/library-go/pkg/assets"
	operatorsv1 "github.com/operator-framework/api/pkg/operators/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/stolostron/volsync-addon-controller/controllers/helmutils"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	helmreleasev1 "open-cluster-management.io/multicloud-operators-subscription/pkg/apis/apps/helmrelease/v1"
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

	DefaultHelmSource                   = "https://backube.github.io/helm-charts"
	DefaultHelmChartName                = "volsync"
	DefaultHelmOperatorInstallNamespace = "volsync-system"
	DefaultHelmPackageVersion           = "0.10" //FIXME: update
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
	utilruntime.Must(appsubscriptionv1.SchemeBuilder.AddToScheme(genericScheme)) //TODO: remove if we don't use it
	utilruntime.Must(helmreleasev1.SchemeBuilder.AddToScheme(genericScheme))     //TODO: remove if we don't use it
	utilruntime.Must(apiextensionsv1.AddToScheme(genericScheme))
}

//go:embed manifests
var embedFS embed.FS

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

var manifestFilesHelmChartInstallAppSub = []string{
	"manifests/helm-chart/namespace.yaml",
	//"manifests/helm-chart/helmrelease-aggregate-clusterrole.yaml",
	//"manifests/helm-chart/helmrelease.yaml",
	"manifests/helm-chart/subscription.yaml",
	//FIXME:how to handle the channel?  "manifests/helm-chart/
}

var manifestFilesHelmrChartInstallHelmRelease = []string{
	"manifests/helm-chart/namespace.yaml",
	"manifests/helm-chart/helmrelease-aggregate-clusterrole.yaml",
	"manifests/helm-chart/helmrelease.yaml",
}

// TODO: we could lookup the stable-0.11 dir and find all files there (in a function)
var manifestFilesHelmChartInstallEmbedded = []string{
	"manifests/helm-chart/namespace.yaml",
}

// List of kinds of objects in the manifestwork - anything in this list will not have
// the namespace updated before adding to the manifestwork
var globalKinds = []string{
	"CustomResourceDefinition",
	"ClusterRole",
	"ClusterRoleBinding",
}

const crdKind = "CustomResourceDefinition"

// var helmChartInstallType = "embedded"
var helmChartInstallType = "embedded-tgz" //FIXME: remove - just for testing different ways
//var helmChartInstallType = "helm-remote-repo" //FIXME: remove - just for testing different ways

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

	values, err := h.getValuesForManifest(addon, cluster, isClusterOpenShift)
	if err != nil {
		return nil, err
	}

	fileList, helmChartDir := getManifestFileList(addon, isClusterOpenShift)
	for _, file := range fileList {
		klog.InfoS("File for manifest: ", "file name", file) //TODO: remove
		//object, err := h.loadManifestFromFile(file, addon, cluster, isClusterOpenShift)
		object, err := h.loadManifestFromFile(file, values)
		if err != nil {
			return nil, err
		}
		klog.InfoS("Object for manifest: ", "object", object) //TODO: remove
		objects = append(objects, object)
	}

	var helmObjs []runtime.Object
	if helmChartDir != "" {
		// THis is the prototype of using embedded (chart yaml files packaged locally)
		// we have a helm chart to render into objects
		//TODO:
		helmObjs, err = h.loadManifestsFromHelmChartsDir(helmChartDir, values, cluster)
	} else {
		// This is the embeded tgz option - or possibly getting the chart from a remote helm repo
		helmObjs, err = h.loadManifestsFromHelmRepo(values, cluster)
	}

	if err != nil {
		return nil, err
	}
	objects = append(objects, helmObjs...)

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
					{
						ResourceIdentifier: workapiv1.ResourceIdentifier{
							Group:     appsv1.GroupName,
							Resource:  "deployments",
							Name:      "volsync",        //FIXME:
							Namespace: "volsync-system", //FIXME: How to do this dynamically?
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
	}
	klog.InfoS("health check successful")
	return nil
}

func (h *volsyncAgent) getValuesForManifest(addon *addonapiv1alpha1.ManagedClusterAddOn,
	cluster *clusterv1.ManagedCluster, isClusterOpenShift bool,
) (addonfactory.Values, error) {
	manifestConfig := struct {
		OperatorInstallNamespace string

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
		OperatorInstallNamespace: getOperatorInstallNamespace(addon, isClusterOpenShift),

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

	return mergedValues, nil
}

// func (h *volsyncAgent) loadManifestFromFile(file string, addon *addonapiv1alpha1.ManagedClusterAddOn,
//
//	cluster *clusterv1.ManagedCluster, isClusterOpenShift bool, values addonfactory.Values,
func (h *volsyncAgent) loadManifestFromFile(file string, values addonfactory.Values,
) (runtime.Object, error) {
	/*
		manifestConfig := struct {
			OperatorInstallNamespace string

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
			OperatorInstallNamespace: getOperatorInstallNamespace(addon, isClusterOpenShift),

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
	*/

	template, err := embedFS.ReadFile(file)
	if err != nil {
		return nil, err
	}

	raw := assets.MustCreateAssetFromTemplate(file, template, &values).Data
	object, _, err := genericCodec.Decode(raw, nil, nil)
	if err != nil {
		klog.ErrorS(err, "Error decoding manifest file", "filename", file)
		return nil, err
	}
	return object, nil
}

// TODO: move this to some helm pkg?
func (h *volsyncAgent) loadManifestsFromHelmChartsDir(chartsDir string, values addonfactory.Values,
	cluster *clusterv1.ManagedCluster,
) ([]runtime.Object, error) {
	//TODO: load from embedded FS instead?
	klog.InfoS("#### Loading helm chart from dir ####", "chartsDir", chartsDir)
	chart, err := loader.LoadDir(chartsDir)
	if err != nil {
		klog.Error(err, "Unable to load chart", "chartsDir", chartsDir)
		return nil, err
	}

	return h.renderManifestsFromChart(chart, values, cluster)
}

// This is the prototype way of using either an embedded chart tgz in the image (i.e. local filesystem)
// or from a remote helm repo
func (h *volsyncAgent) loadManifestsFromHelmRepo(values addonfactory.Values, cluster *clusterv1.ManagedCluster,
) ([]runtime.Object, error) {
	chartName := values["HelmChartName"].(string)
	desiredVolSyncVersion := values["HelmPackageVersion"].(string)

	var chart *chart.Chart
	var err error
	if helmChartInstallType == "embedded-tgz" { //FIXME: this is just for quick prototyping - need to specify somewhere
		chart, err = helmutils.EnsureEmbeddedChart(chartName, desiredVolSyncVersion)
	} else {
		helmRepoUrl := values["HelmSource"].(string)
		chart, err = helmutils.EnsureLocalChart(helmRepoUrl, chartName, desiredVolSyncVersion, false)
	}

	if err != nil {
		klog.ErrorS(err, "unable to load or render chart")
		return nil, err
	}

	return h.renderManifestsFromChart(chart, values, cluster)
}

//nolint:funlen
func (h volsyncAgent) renderManifestsFromChart(chart *chart.Chart, values addonfactory.Values,
	cluster *clusterv1.ManagedCluster,
) ([]runtime.Object, error) {
	helmObjs := []runtime.Object{}

	// This only loads crds from the crds/ dir - consider putting them in that format upstream?
	// OTherwise, maybe we don't need this section getting CRDs, just process them with the rest
	crds := chart.CRDObjects()
	for _, crd := range crds {
		klog.InfoS("#### CRD ####", "crd.Name", crd.Name)
		crdObj, _, err := genericCodec.Decode(crd.File.Data, nil, nil)
		if err != nil {
			klog.Error(err, "Unable to decode CRD", "crd.Name", crd.Name)
			return nil, err
		}
		helmObjs = append(helmObjs, crdObj)
	}

	helmEngine := engine.Engine{
		Strict:   true,
		LintMode: false,
	}
	klog.InfoS("Testing helmengine", "helmEngine", helmEngine, "values", values) //TODO: remove

	releaseOptions := chartutil.ReleaseOptions{
		Name:      values["HelmChartName"].(string),
		Namespace: values["OperatorInstallNamespace"].(string),
	}

	capabilities := &chartutil.Capabilities{
		KubeVersion: chartutil.KubeVersion{Version: cluster.Status.Version.Kubernetes},
		//TODO: any other capabilities?
	}

	chartValues, err := chartutil.ToRenderValues(chart, values, releaseOptions, capabilities)
	if err != nil {
		klog.Error(err, "Unable to render values for chart", "chart.Name()", chart.Name())
		return nil, err
	}

	klog.InfoS("### releaseOptions ###", "releaseOptions", releaseOptions) //TODO: remove
	klog.InfoS("### capabilities ###", "capabilities", capabilities)       //TODO: remove
	klog.InfoS("### Chart values ###", "chartValues", chartValues)         //TODO: remove

	templates, err := helmEngine.Render(chart, chartValues)
	if err != nil {
		klog.Error(err, "Unable to render chart", "chart.Name()", chart.Name())
		return nil, err
	}

	//TODO: can we update the manifest to tell it not to cleanup CRDs when we delete?

	// sort the filenames of the templates so the manifests are ordered consistently
	keys := make([]string, len(templates))
	i := 0
	for k := range templates {
		keys[i] = k
		i++
	}
	sort.Strings(keys)

	// VolSync CRDs are going to be last (start with volsync.backube_), so go through our sorted keys in reverse order
	for j := len(keys) - 1; j >= 0; j-- {
		fileName := keys[j]
		// skip files that are not .yaml or empty
		fileExt := filepath.Ext(fileName)

		templateData := templates[fileName]
		if (fileExt != ".yaml" && fileExt != ".yml") || len(templateData) == 0 || templateData == "\n" {
			klog.InfoS("Skipping template", "fileName", fileName)
			continue
		}

		templateObj, gvk, err := genericCodec.Decode([]byte(templateData), nil, nil)
		if err != nil {
			klog.Error(err, "Error decoding rendered template", "fileName", fileName)
			return nil, err
		}

		if gvk != nil {
			if !slices.Contains(globalKinds, gvk.Kind) {
				// Helm rendering does not set namespace on the templates, it will rely on the kubectl install/apply
				// to do it (which does not happen since these objects end up directly in our manifestwork).
				// So set the namespace ourselves for any object with kind not in our globalKinds list
				templateObj.(metav1.Object).SetNamespace(releaseOptions.Namespace)
			}

			if gvk.Kind == crdKind {
				// Add annotation to indicate we do not want the CRD deleted when the manifestwork is deleted
				// (i.e. when the managedclusteraddon is deleted)
				crdAnnotations := templateObj.(metav1.Object).GetAnnotations()
				crdAnnotations[addonapiv1alpha1.DeletionOrphanAnnotationKey] = ""
				templateObj.(metav1.Object).SetAnnotations(crdAnnotations)
			}
		}

		helmObjs = append(helmObjs, templateObj)
	}

	return helmObjs, nil
}

// returns list of files to put in manifestwork
// 2nd return arg is a dir containing helmcharts to resolve and put into manifestwork
func getManifestFileList(_ *addonapiv1alpha1.ManagedClusterAddOn, isClusterOpenShift bool) ([]string, string) {
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
		return manifestFilesAllNamespaces, ""
	}

	// Non OpenShift cluster - use manifestwork containing helm chart namespace/subscription
	if helmChartInstallType == "helmrelease" {
		klog.InfoS("Installing volsync helm chart via helmrelease")
		return manifestFilesHelmrChartInstallHelmRelease, ""
	} else if helmChartInstallType == "appsub" {
		klog.InfoS("Installing volsync helm chart via appsub")
		return manifestFilesHelmChartInstallAppSub, ""
	} else if helmChartInstallType == "embedded" {
		klog.InfoS("Installing volsync helm chart via embedded charts")
		//helmChartDir := "manifests/helm-chart/stable-0.11/volsync" //TODO: look this up based on desired version requested
		// Using local dir for quick test
		helmChartDir :=
			"/Users/tflower/DEV/tesshuflower/volsync-addon-controller/controllers/manifests/helm-chart/stable-0.11/volsync"
		return manifestFilesHelmChartInstallEmbedded, helmChartDir
	}

	klog.InfoS("Installing volsync helm chart via embedded repo with tgz and index.html or remote repo")
	return manifestFilesHelmChartInstallEmbedded, ""
}

func getOperatorInstallNamespace(_ *addonapiv1alpha1.ManagedClusterAddOn, isClusterOpenShift bool) string {
	if isClusterOpenShift {
		// The only namespace supported is openshift-operators, so ignore whatever is in the spec
		return globalOperatorInstallNamespace
	}

	// non-OpenShift target cluster
	return DefaultHelmOperatorInstallNamespace //TODO: allow overriding
	/*
		installNamespace := "volsync-system" // Default value //TODO: make a constant
		if addon.Spec.InstallNamespace != "" && addon.Spec.InstallNamespace != "default" {
			installNamespace = addon.Spec.InstallNamespace
		}
		return installNamespace
	*/
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

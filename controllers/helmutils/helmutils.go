package helmutils

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"

	addonapiv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
)

const (
	crdKind            = "CustomResourceDefinition"
	serviceAccountKind = "ServiceAccount"
)

// List of kinds of objects in the manifestwork - anything in this list will not have
// the namespace updated before adding to the manifestwork
var globalKinds = []string{
	"CustomResourceDefinition",
	"ClusterRole",
	"ClusterRoleBinding",
}

// key will be stable-X.Y (depends on release)
// value is the loaded *chart.Chart
var loadedChartsMap sync.Map

// key will be stable-X.Y (depends on release)
// value is a map containing values for volsync & kube-rbac-proxy image
// Usually we will only have one dir, the default one (e.g. stable-0.12)
// However this allows for us to insert another stable-x.y release at some
// point if desired with specific image values in the images.yaml
var loadedImagesMap sync.Map

// New - only load helm charts directly from embedded dirs
func InitEmbeddedCharts(embeddedChartsDir string, defaultChartKey string, defaultImages map[string]string) error {
	if embeddedChartsDir == "" {
		return fmt.Errorf("error loading embedded charts, no dir provided")
	}

	// Embedded Charts dir contains subdirectories - each subdir should contain 1 chart
	subDirs, err := os.ReadDir(embeddedChartsDir)
	if err != nil {
		klog.ErrorS(err, "error loading embedded charts", "embeddedChartsDir", embeddedChartsDir)
		return err
	}

	for _, subDir := range subDirs {
		chartKey := subDir.Name()

		// Load the helm charts from the volsnc dir
		chartsPath := filepath.Join(embeddedChartsDir, chartKey, "volsync")
		klog.InfoS("Loading charts", "chartsPath", chartsPath)

		chart, err := loader.Load(chartsPath)
		if err != nil {
			klog.ErrorS(err, "Error loading chart", "chartsPath", chartsPath)
			return err
		}
		klog.InfoS("Successfully loaded chart", "chartKey", chartKey, "Name", chart.Name(), "AppVersion", chart.AppVersion())

		// Save chart into memory
		loadedChartsMap.Store(chartKey, chart)

		// Now look to see if we need to override images (either from defaults passed in
		// or from an images.yaml)
		if chartKey == defaultChartKey && len(defaultImages) > 0 {
			// Save default image values for the default chartKey
			klog.InfoS("Default operand images (from mch configmap defaults)",
				"chartKey", chartKey, "vsDefaultImages", defaultImages)
			loadedImagesMap.Store(chartKey, defaultImages)
		} else {
			// Load image defaults from images.yaml for subdirs that aren't the default
			imagesYamlPath := filepath.Join(embeddedChartsDir, subDir.Name(), "images.yaml")
			imagesYamlFile, err := os.ReadFile(imagesYamlPath)
			if err != nil {
				klog.InfoS("no default images will be loaded",
					"imagesYamlPath", imagesYamlPath)
				continue
			}

			var vsDefaultImages map[string]string // The defaultImages for this chartKey
			err = yaml.Unmarshal(imagesYamlFile, &vsDefaultImages)
			if err != nil {
				klog.ErrorS(err, "unable to parse images.yaml", "imagesYamlPath", imagesYamlPath)
				return err
			}
			klog.InfoS("Default operand images (from images.yaml)",
				"chartKey", chartKey, "vsDefaultImages", vsDefaultImages)

			// Save default image values for this chartKey
			loadedImagesMap.Store(chartKey, vsDefaultImages)
		}
	}

	return nil
}

func GetVolSyncDefaultImagesMap(chartKey string) (map[string]string, error) {
	vsDefaultImages, ok := loadedImagesMap.Load(chartKey)
	if !ok {
		return map[string]string{}, nil // No defaults set by us for this chart - will use values in the chart itself
	}
	return vsDefaultImages.(map[string]string), nil
}

func GetEmbeddedChart(chartKey string) (*chart.Chart, error) {
	loadedChart, ok := loadedChartsMap.Load(chartKey)
	if !ok {
		return nil, fmt.Errorf("unable to find chart %s", chartKey)
	}
	return loadedChart.(*chart.Chart), nil
}

//nolint:funlen
func RenderManifestsFromChart(
	chart *chart.Chart,
	namespace string,
	cluster *clusterv1.ManagedCluster,
	clusterIsOpenShift bool,
	chartValues map[string]interface{},
	runtimeDecoder runtime.Decoder,
) ([]runtime.Object, error) {
	helmObjs := []runtime.Object{}

	/*
		// This only loads crds from the crds/ dir - consider putting them in that format upstream?
		// OTherwise, maybe we don't need this section getting CRDs, just process them with the rest
		crds := chart.CRDObjects()
		for _, crd := range crds {
			crdObj, _, err := runtimeDecoder.Decode(crd.File.Data, nil, nil)
			if err != nil {
				klog.Error(err, "Unable to decode CRD", "crd.Name", crd.Name)
				return nil, err
			}
			helmObjs = append(helmObjs, crdObj)
		}
	*/

	helmEngine := engine.Engine{
		Strict:   true,
		LintMode: false,
	}

	releaseOptions := chartutil.ReleaseOptions{
		Name:      chart.Name(),
		Namespace: namespace,
	}

	capabilities := &chartutil.Capabilities{
		KubeVersion: chartutil.KubeVersion{Version: cluster.Status.Version.Kubernetes},
		APIVersions: chartutil.DefaultVersionSet,
	}

	if clusterIsOpenShift {
		// Add openshift scc to apiversions so capabilities in our helm charts that check this will work
		capabilities.APIVersions = append(capabilities.APIVersions, "security.openshift.io/v1/SecurityContextConstraints")
	}

	renderedChartValues, err := chartutil.ToRenderValues(chart, chartValues, releaseOptions, capabilities)
	if err != nil {
		klog.ErrorS(err, "Unable to render values for chart", "chart.Name()", chart.Name())
		return nil, err
	}

	templates, err := helmEngine.Render(chart, renderedChartValues)
	if err != nil {
		klog.ErrorS(err, "Unable to render chart", "chart.Name()", chart.Name())
		return nil, err
	}

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
			klog.V(4).InfoS("Skipping template", "fileName", fileName)
			continue
		}

		templateObj, gvk, err := runtimeDecoder.Decode([]byte(templateData), nil, nil)
		if err != nil {
			klog.ErrorS(err, "Error decoding rendered template", "fileName", fileName)
			return nil, err
		}

		if gvk == nil {
			gvkErr := fmt.Errorf("no gvk for template")
			klog.ErrorS(gvkErr, "Error decoding gvk from rendered template", "fileName", fileName)
			return nil, gvkErr
		}

		if !slices.Contains(globalKinds, gvk.Kind) {
			// Helm rendering does not set namespace on the templates, it will rely on the kubectl install/apply
			// to do it (which does not happen since these objects end up directly in our manifestwork).
			// So set the namespace ourselves for any object with kind not in our globalKinds list
			templateObj.(metav1.Object).SetNamespace(releaseOptions.Namespace)
		}

		// Special cases for specific resource kinds
		switch gvk.Kind {
		case crdKind:
			// Add annotation to indicate we do not want the CRD deleted when the manifestwork is deleted
			// (i.e. when the managedclusteraddon is deleted)
			crdAnnotations := templateObj.(metav1.Object).GetAnnotations()
			crdAnnotations[addonapiv1alpha1.DeletionOrphanAnnotationKey] = ""
			templateObj.(metav1.Object).SetAnnotations(crdAnnotations)
		}

		helmObjs = append(helmObjs, templateObj)
	}

	return helmObjs, nil
}

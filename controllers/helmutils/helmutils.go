package helmutils

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"k8s.io/klog/v2"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/downloader"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/repo"
)

const (
	volsyncChartName        = "volsync"
	localRepoDir            = "/tmp/helmrepos" // FIXME: mount memory pvc to put files outside of /tmp
	localRepoCacheDir       = localRepoDir + "/.cache/helm/repository"
	localRepoConfigFileName = localRepoDir + "/.config/helm/repositories.yaml"
)

//var volsyncRepoURL = "https://tesshuflower.github.io/helm-charts/" //TODO: set default somewhere, allow overriding

// key will be the file name of the chart (e.g. volsync-v0.11.0.tgz)
// value is the loaded *chart.Chart
var loadedChartsMap sync.Map
var localEmbeddedIndexFile *repo.IndexFile // Don't access directly - use loadEmbeddedHelmIndexFile()

func loadLocalRepoConfig() (*repo.File, error) {
	err := os.MkdirAll(filepath.Dir(localRepoConfigFileName), os.ModePerm)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}

	repoConfigFile, err := repo.LoadFile(localRepoConfigFileName)
	if err != nil {
		// Need to init local config file
		repoConfigFile = repo.NewFile()

		if err := repo.NewFile().WriteFile(localRepoConfigFileName, 0600); err != nil {
			klog.ErrorS(err, "Error initializing local repo file", "localRepoConfigFile", localRepoConfigFileName)
			return nil, err
		}
		klog.InfoS("Initialized local repo file", "localRepoConfigFile", localRepoConfigFileName)
	}

	/*
		  if len(f.Repositories) == 0 {
						return errors.New("no repositories to show")
					}
	*/

	return repoConfigFile, nil
}

// Name we give the repo - compute based on url in case we end up with multiple different repo urls to use
func getLocalRepoNameFromURL(repoUrl string) string {
	return base64.StdEncoding.EncodeToString([]byte(repoUrl))
}

func getChartRef(repoUrl string, chartName string) string {
	// Returns reponame/chartname
	return getLocalRepoNameFromURL(repoUrl) + "/" + chartName
}

// Lock (on writing/downloading/updating local repository charts & info)
var lock sync.Mutex

// Creates local repo if it doesn't exist
// If it exists already, update it if update=true
func EnsureLocalRepo(repoUrl string, update bool) error {
	lock.Lock()
	defer lock.Unlock()

	repoConfigFile, err := loadLocalRepoConfig()
	if err != nil {
		klog.ErrorS(err, "error loading repo config")
		return err
	}

	repoName := getLocalRepoNameFromURL(repoUrl)

	if repoConfigFile.Has(repoName) && !update {
		// No update needed
		return nil
	}

	// This is either a new repo or we need to update
	repoCfg := repo.Entry{
		Name:                  repoName,
		URL:                   repoUrl,
		InsecureSkipTLSverify: true,
	}

	//var defaultOptions = []getter.Option{getter.WithTimeout(time.Second * getter.DefaultHTTPTimeout)}
	//httpProvider, err := getter.NewHTTPGetter(defaultOptions...)
	/*
		providers := getter.Providers{getter.Provider{
			Schemes: []string{"http", "https"},
			New:     getter.NewHTTPGetter, //TODO: do we need any http options?
		}}*/

	r, err := repo.NewChartRepository(&repoCfg, getProviders())
	if err != nil {
		klog.ErrorS(err, "error creating new local chart repo", "repoName", repoName)
		return err
	}
	r.CachePath = localRepoCacheDir

	// Download the index file (this is equivalent to a helm update on the repo)
	if _, err := r.DownloadIndexFile(); err != nil {
		klog.ErrorS(err, "error downloading index from repo", "repoUrl", repoUrl)
		return err
	}

	if !repoConfigFile.Has(repoName) {
		// update repo config file with this repo (only needed when adding)
		repoConfigFile.Update(&repoCfg)

		if err := repoConfigFile.WriteFile(localRepoConfigFileName, 0600); err != nil {
			klog.ErrorS(err, "error updating repo config file", "localRepoConfigFile", localRepoConfigFileName,
				"repoName", repoName)
			return err
		}
	}

	return nil
}

func getProviders() getter.Providers {
	return getter.Providers{getter.Provider{
		Schemes: []string{"http", "https"},
		New:     getter.NewHTTPGetter, //TODO: do we need any http options?
	}}
}

var embeddedHelmIndexOnce sync.Once

// Only load our embedded index once (index we package in the volsync-addon-controller img), and then
// save in var localEmbeddedIndexFile
func loadEmbeddedHelmIndexFile(indexFileFullPath string) (*repo.IndexFile, error) {
	var indexFile *repo.IndexFile
	var err error
	embeddedHelmIndexOnce.Do(func() {
		indexFile, err = repo.LoadIndexFile(indexFileFullPath)
	})

	if err != nil {
		return nil, err
		//TODO: consider calling this 1x from main.go and panicking if it fails
	}
	// Save in memory so we don't keep reloading this embedded index
	localEmbeddedIndexFile = indexFile

	return localEmbeddedIndexFile, nil
}

func EnsureEmbeddedChart(chartName, version string) (*chart.Chart, error) {
	//TODO: locking, ensure local chart etc
	//FIXME: don't hardcode this here
	embeddedChartsDir :=
		"/Users/tflower/DEV/tesshuflower/volsync-addon-controller/controllers/manifests/helm-chart/charts/"
	//FIXME: where to embed this file?  If embedded need to save to /tmp/ somewhere?
	indexFileFullPath := embeddedChartsDir + "index.yaml"

	indexFile, err := loadEmbeddedHelmIndexFile(indexFileFullPath)
	if err != nil {
		return nil, err
	}

	chartVersion, err := indexFile.Get(chartName, version)
	if err != nil {
		return nil, fmt.Errorf("unable to get chart version %s, error: %w", version, err)
	}
	tgzUrl := chartVersion.URLs
	if len(tgzUrl) == 0 {
		return nil, fmt.Errorf("unable to get chart version url for version: %s", version)
	}

	chartZipFileName := filepath.Base(chartVersion.URLs[0])

	//klog.InfoS("Embedded - resolved chart version", "chartVersion", chartVersion)
	klog.InfoS("Embedded - resolved chart url", "chartVersion.URLs", chartVersion.URLs)
	klog.InfoS("Embedded - resolved chart tgz", "chartZipFileName", chartZipFileName)

	// check memory to see if the chart for this zip name exists
	loadedChart, ok := loadedChartsMap.Load(chartZipFileName)
	if ok {
		klog.InfoS("Chart in memory", "chart.Name()", loadedChart.(*chart.Chart).Name(),
			"chartZipFileName", chartZipFileName) //TODO: remove
		return loadedChart.(*chart.Chart), nil
	}

	// Find the chart locally - don't need url from the index file, just the name of the desired .tgz
	chartZipFullPath := embeddedChartsDir + chartZipFileName

	// Now load the chart into memory
	chart, err := loader.Load(chartZipFullPath)
	if err != nil {
		klog.ErrorS(err, "Error loading chart", "chartZipFullPath", chartZipFullPath)
	}
	klog.InfoS("Successfully loaded chart", "chart.Name()", chart.Name())

	// Save into memory
	loadedChartsMap.Store(chartZipFileName, chart)

	return chart, nil
}

//nolint:funlen
func EnsureLocalChart(repoUrl, chartName, version string, update bool) (*chart.Chart, error) {
	err := EnsureLocalRepo(repoUrl, update) // This does a lock
	if err != nil {
		return nil, err
	}

	// Lock to make sure we're not in the process of downloading this chart
	lock.Lock()
	defer lock.Unlock() //TODO: fix locking or to keep simple have 1 entry point

	//FIXME: do not download if the chart already has been downloaded
	/*
		  regClient, err := newRegistryClient(client.CertFile, client.KeyFile, client.CaFile,
						client.InsecureSkipTLSverify, client.PlainHTTP)
					if err != nil {
						return fmt.Errorf("missing registry client: %w", err)
					}
	*/

	chDownloader := downloader.ChartDownloader{
		Out:     os.Stdout, //TODO:
		Getters: getProviders(),
		Options: []getter.Option{
			//getter.WithPassCredentialsAll(c.PassCredentialsAll),
			//getter.WithTLSClientConfig(c.CertFile, c.KeyFile, c.CaFile),
			getter.WithInsecureSkipVerifyTLS(false),
			//getter.WithPlainHTTP(c.PlainHTTP),
		},
		RepositoryConfig: localRepoConfigFileName,
		RepositoryCache:  localRepoCacheDir,
		RegistryClient:   nil, // Seems only used for OCI urls
		//TODO: Verify:           downloader.VerifyAlways,
	}

	chartRef := getChartRef(repoUrl, chartName)

	url, err := chDownloader.ResolveChartVersion(chartRef, version)
	if err != nil {
		klog.ErrorS(err, "error resolving chart version", "chartRef", chartRef, "version", version)
	}

	chartZipFileName := filepath.Base(url.Path)

	klog.InfoS("Chart version resolved",
		"url", url,
		"chartZipFileName", chartZipFileName,
	) //TODO: remove

	// check memory to see if the chart for this zip name exists
	loadedChart, ok := loadedChartsMap.Load(chartZipFileName)
	if ok {
		klog.InfoS("Chart in memory", "chart.Name()", loadedChart.(*chart.Chart).Name()) //TODO: remove
		return loadedChart.(*chart.Chart), nil
	}

	// Check if the chart has already been downloaded & unzipped locally
	/*
		chartZipFullPath := getChartZipFullPath(chartZipFileName)
		if _, err := os.Stat(chartZipFullPath); err != nil { //TODO: possibly re-download if not in mem?
			klog.InfoS("FILE NOT FOUND - Downloading chart locally", "chartZipFullPath", chartZipFullPath)
		}
	*/

	klog.InfoS("Downloading chart locally ...")
	// Download the chart .tgz from the remote helm repo into our local repo cache
	chartZipFullPath, _, err := chDownloader.DownloadTo(chartRef, version, localRepoCacheDir)
	if err != nil {
		klog.ErrorS(err, "Error downloading chart")
	}
	klog.InfoS("chart downloaded", "chartZipFullPath", chartZipFullPath) //TODO: remove

	// Now load the chart into memory
	chart, err := loader.Load(chartZipFullPath)
	if err != nil {
		klog.ErrorS(err, "Error loading chart", "chartZipFullPath", chartZipFullPath)
	}

	klog.InfoS("Successfully loaded chart", "chart.Name()", chart.Name())

	// Save into memory
	loadedChartsMap.Store(chartZipFileName, chart)

	return chart, nil
}

/*
func getChartZipFullPath(chartZipFileName string) string {
	// Save the zips into the localCacheDir
	return localRepoCacheDir + "/" + chartZipFileName
}
*/

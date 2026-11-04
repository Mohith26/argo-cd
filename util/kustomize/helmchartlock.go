package kustomize

import (
	"os"
	"path/filepath"
	"slices"

	"github.com/argoproj/pkg/v2/sync"
	log "github.com/sirupsen/logrus"
	"sigs.k8s.io/yaml"
)

// defaultHelmChartHome is the directory, relative to a kustomization root, into which kustomize's
// HelmChartInflationGenerator pulls charts when helmGlobals.chartHome is not set.
const defaultHelmChartHome = "charts"

// helmChartHomeLock serializes `kustomize build --enable-helm` runs that inflate Helm charts into the
// same chart home directory. The repo-server builds Kustomize applications from a shared checkout in
// parallel, and kustomize pulls remote charts with `helm pull --untar` into <kustomization>/charts
// whenever the chart is not there yet. Two overlays of a common base racing on that directory make
// the slower pull fail with "a file or directory with the name ... already exists", or make one
// build read a chart the other is still extracting. See
// https://github.com/argoproj/argo-cd/issues/8008.
var helmChartHomeLock = sync.NewKeyLock()

// lockHelmChartHomes acquires the lock for every chart home the kustomization at appPath may pull
// Helm charts into and returns a function releasing them. Builds that share no chart home keep
// running concurrently.
func lockHelmChartHomes(repoRoot, appPath string) func() {
	homes := findHelmChartHomes(repoRoot, appPath)
	if len(homes) == 0 {
		return func() {}
	}
	// Always acquire in sorted order so two builds sharing several chart homes cannot deadlock.
	for _, home := range homes {
		helmChartHomeLock.Lock(home)
	}
	return func() {
		for _, home := range slices.Backward(homes) {
			helmChartHomeLock.Unlock(home)
		}
	}
}

// findHelmChartHomes returns the sorted, de-duplicated absolute chart home directories used by the
// kustomization at appPath and by every local kustomization it references through resources,
// bases, components or generators. Only files inside repoRoot are read; remote references and
// anything that cannot be read or parsed are skipped, leaving error reporting to `kustomize build`.
func findHelmChartHomes(repoRoot, appPath string) []string {
	root, err := os.OpenRoot(repoRoot)
	if err != nil {
		log.Debugf("Not locking Helm chart homes: failed to open repo root %s: %v", repoRoot, err)
		return nil
	}
	defer func() { _ = root.Close() }()

	f := &chartHomeFinder{repoRoot: filepath.Clean(repoRoot), root: root, visited: map[string]bool{}, homes: map[string]bool{}}
	if rel, ok := f.relToRoot(appPath); ok {
		f.visit(rel)
	}
	homes := make([]string, 0, len(f.homes))
	for home := range f.homes {
		homes = append(homes, home)
	}
	slices.Sort(homes)
	return homes
}

type chartHomeFinder struct {
	repoRoot string
	root     *os.Root
	visited  map[string]bool
	homes    map[string]bool
}

// kustomizationRefs holds the parts of a kustomization file relevant to Helm chart inflation.
type kustomizationRefs struct {
	Resources   []string `json:"resources"`
	Bases       []string `json:"bases"`
	Components  []string `json:"components"`
	Generators  []string `json:"generators"`
	HelmGlobals *struct {
		ChartHome string `json:"chartHome"`
	} `json:"helmGlobals"`
	HelmCharts []any `json:"helmCharts"`
	// HelmChartInflationGenerator is the deprecated predecessor of HelmCharts/HelmGlobals.
	HelmChartInflationGenerator []struct {
		ChartHome string `json:"chartHome"`
	} `json:"helmChartInflationGenerator"`
}

// helmGeneratorConfig is a HelmChartInflationGenerator config file listed under `generators`.
type helmGeneratorConfig struct {
	Kind      string `json:"kind"`
	ChartHome string `json:"chartHome"`
}

// relToRoot converts an absolute path into a path relative to the repo root, rejecting paths that
// lie outside of it.
func (f *chartHomeFinder) relToRoot(path string) (string, bool) {
	rel, err := filepath.Rel(f.repoRoot, filepath.Clean(path))
	if err != nil || !filepath.IsLocal(rel) && rel != "." {
		return "", false
	}
	return rel, true
}

// visit records the chart homes of the kustomization in dir (relative to the repo root) and
// recurses into the local kustomizations it references.
func (f *chartHomeFinder) visit(dir string) {
	if f.visited[dir] {
		return
	}
	f.visited[dir] = true

	var data []byte
	for _, name := range KustomizationNames {
		b, err := f.root.ReadFile(filepath.Join(dir, name))
		if err == nil {
			data = b
			break
		}
	}
	if data == nil {
		return
	}
	var k kustomizationRefs
	if err := yaml.Unmarshal(data, &k); err != nil {
		log.Debugf("Not inspecting %s for Helm charts: %v", dir, err)
		return
	}

	if len(k.HelmCharts) > 0 {
		chartHome := ""
		if k.HelmGlobals != nil {
			chartHome = k.HelmGlobals.ChartHome
		}
		f.addHome(dir, chartHome)
	}
	for _, g := range k.HelmChartInflationGenerator {
		f.addHome(dir, g.ChartHome)
	}

	for _, ref := range slices.Concat(k.Resources, k.Bases, k.Components) {
		f.visitRef(dir, ref)
	}
	for _, ref := range k.Generators {
		f.visitGenerator(dir, ref)
	}
}

// visitRef follows a resources/bases/components entry if it is a local directory.
func (f *chartHomeFinder) visitRef(dir, ref string) {
	target := filepath.Join(dir, ref)
	if !filepath.IsLocal(target) && target != "." {
		return
	}
	if fi, err := f.root.Stat(target); err == nil && fi.IsDir() {
		f.visit(target)
	}
}

// visitGenerator follows a generators entry, which is either a local kustomization directory or a
// generator config file such as a HelmChartInflationGenerator.
func (f *chartHomeFinder) visitGenerator(dir, ref string) {
	target := filepath.Join(dir, ref)
	if !filepath.IsLocal(target) && target != "." {
		return
	}
	fi, err := f.root.Stat(target)
	if err != nil {
		return
	}
	if fi.IsDir() {
		f.visit(target)
		return
	}
	b, err := f.root.ReadFile(target)
	if err != nil {
		return
	}
	var g helmGeneratorConfig
	if yaml.Unmarshal(b, &g) == nil && g.Kind == "HelmChartInflationGenerator" {
		f.addHome(dir, g.ChartHome)
	}
}

// addHome records the chart home of the kustomization in dir. Like kustomize, a relative chartHome
// is resolved against the kustomization directory and an empty one defaults to "charts".
func (f *chartHomeFinder) addHome(dir, chartHome string) {
	if chartHome == "" {
		chartHome = defaultHelmChartHome
	}
	if !filepath.IsAbs(chartHome) {
		chartHome = filepath.Join(f.repoRoot, dir, chartHome)
	}
	f.homes[filepath.Clean(chartHome)] = true
}

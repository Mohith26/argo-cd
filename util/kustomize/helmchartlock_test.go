package kustomize

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/util/git"
)

// packageTestChart returns a gzipped tarball of a minimal Helm chart, as `helm package` would produce.
func packageTestChart(t *testing.T, name, version string) []byte {
	t.Helper()
	files := map[string]string{
		name + "/Chart.yaml":               fmt.Sprintf("apiVersion: v2\nname: %s\nversion: %s\n", name, version),
		name + "/values.yaml":              "data: bar\n",
		name + "/templates/configmap.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}-cm\ndata:\n  foo: {{ .Values.data }}\n",
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, dir := range []string{name + "/", name + "/templates/"} {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0o755}))
	}
	for path, content := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: path, Mode: 0o644, Size: int64(len(content))}))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// newTestChartRepo serves a single-chart Helm repository. Chart downloads are slowed down so that
// concurrent builds reliably overlap while the chart is being pulled.
func newTestChartRepo(t *testing.T, name, version string, delay time.Duration) *httptest.Server {
	t.Helper()
	chart := packageTestChart(t, name, version)
	tgz := fmt.Sprintf("%s-%s.tgz", name, version)
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "apiVersion: v1\nentries:\n  %s:\n  - apiVersion: v2\n    name: %s\n    version: %s\n    urls:\n    - %s/%s\n", name, name, version, srv.URL, tgz)
	})
	mux.HandleFunc("/"+tgz, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(delay)
		_, _ = w.Write(chart)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestKustomizeBuildConcurrentAppsSharingHelmChart is a regression test for
// https://github.com/argoproj/argo-cd/issues/8008: several Applications whose overlays share a base
// with a helmCharts entry are built concurrently by the repo-server. Each `kustomize build` pulls
// the chart into the same <base>/charts directory, and without serialization the concurrent
// `helm pull --untar` calls collide ("a file or directory with the name ... already exists") or
// read a half-extracted chart.
func TestKustomizeBuildConcurrentAppsSharingHelmChart(t *testing.T) {
	repo := newTestChartRepo(t, "shared", "0.1.0", 300*time.Millisecond)
	repoRoot := t.TempDir()
	base := filepath.Join(repoRoot, "base")
	require.NoError(t, os.MkdirAll(base, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(base, "kustomization.yaml"), fmt.Appendf(nil, `helmCharts:
- name: shared
  repo: %s
  version: 0.1.0
  releaseName: shared
`, repo.URL), 0o644))

	const apps = 6
	for i := range apps {
		overlay := filepath.Join(repoRoot, "overlays", fmt.Sprintf("app%d", i))
		require.NoError(t, os.MkdirAll(overlay, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), fmt.Appendf(nil, "namespace: app%d\nresources:\n- ../../base\n", i), 0o644))
	}

	var wg sync.WaitGroup
	errs := make([]error, apps)
	for i := range apps {
		wg.Go(func() {
			k := NewKustomizeApp(repoRoot, filepath.Join(repoRoot, "overlays", fmt.Sprintf("app%d", i)), git.NopCreds{}, "", "", "", "")
			objs, _, _, err := k.Build(nil, &v1alpha1.KustomizeOptions{BuildOptions: "--enable-helm"}, nil, &BuildOpts{})
			if err == nil && len(objs) != 1 {
				err = fmt.Errorf("expected 1 object, got %d", len(objs))
			}
			errs[i] = err
		})
	}
	wg.Wait()
	for i, err := range errs {
		assert.NoError(t, err, "build of app%d failed", i)
	}
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
}

func TestFindHelmChartHomes(t *testing.T) {
	absHome := filepath.Join(t.TempDir(), "shared-charts")
	tests := []struct {
		name     string
		files    map[string]string
		app      string
		expected []string
	}{
		{
			name:  "no helm charts",
			files: map[string]string{"app/kustomization.yaml": "resources:\n- deployment.yaml\n"},
			app:   "app",
		},
		{
			name: "helm charts in the app itself use the default chart home",
			files: map[string]string{
				"app/kustomization.yaml": "helmCharts:\n- name: foo\n  repo: https://example.com\n  version: 1.0.0\n",
			},
			app:      "app",
			expected: []string{"app/charts"},
		},
		{
			name: "overlay inherits the chart home of its base",
			files: map[string]string{
				"base/kustomization.yaml":            "helmCharts:\n- name: foo\n",
				"overlays/a/kustomization.yml":       "resources:\n- ../../base\n- configmap.yaml\n",
				"overlays/a/configmap.yaml":          "apiVersion: v1\nkind: ConfigMap\n",
				"overlays/b/kustomization.yaml":      "resources:\n- ../../base\n",
				"overlays/b/unrelated/Kustomization": "helmCharts:\n- name: bar\n",
			},
			app:      "overlays/a",
			expected: []string{"base/charts"},
		},
		{
			name: "relative and absolute helmGlobals.chartHome",
			files: map[string]string{
				"app/kustomization.yaml":   "resources:\n- ../base1\n- ../base2\nhelmCharts:\n- name: foo\nhelmGlobals:\n  chartHome: " + absHome + "\n",
				"base1/kustomization.yaml": "helmGlobals:\n  chartHome: ../vendor/charts\nhelmCharts:\n- name: bar\n",
				"base2/kustomization.yaml": "helmGlobals:\n  chartHome: ../vendor/charts/\nhelmCharts:\n- name: baz\n",
			},
			app:      "app",
			expected: []string{absHome, "vendor/charts"},
		},
		{
			name: "bases, components and generators are followed",
			files: map[string]string{
				"app/kustomization.yaml":       "bases:\n- ../legacy\ncomponents:\n- ../component\ngenerators:\n- ../genkust\n- helm-generator.yaml\n- other-generator.yaml\n",
				"app/helm-generator.yaml":      "apiVersion: builtin\nkind: HelmChartInflationGenerator\nmetadata:\n  name: x\nchartName: foo\nchartHome: generated\n",
				"app/other-generator.yaml":     "apiVersion: builtin\nkind: ConfigMapGenerator\nmetadata:\n  name: y\n",
				"legacy/kustomization.yaml":    "helmChartInflationGenerator:\n- chartName: foo\n  chartHome: old\n",
				"component/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\nhelmCharts:\n- name: foo\n",
				"genkust/kustomization.yaml":   "helmCharts:\n- name: foo\n",
			},
			app:      "app",
			expected: []string{"app/generated", "component/charts", "genkust/charts", "legacy/old"},
		},
		{
			name: "cycles, remote and out-of-repo references are ignored",
			files: map[string]string{
				"a/kustomization.yaml": "resources:\n- ../b\n- https://github.com/org/repo//base?ref=v1\n- ../../outside\n- missing\nhelmCharts:\n- name: foo\n",
				"b/kustomization.yaml": "resources:\n- ../a\n",
			},
			app:      "a",
			expected: []string{"a/charts"},
		},
		{
			name:  "invalid kustomization is skipped",
			files: map[string]string{"app/kustomization.yaml": "helmCharts: [\n"},
			app:   "app",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoRoot := t.TempDir()
			writeFiles(t, repoRoot, tt.files)
			expected := []string{}
			for _, e := range tt.expected {
				if !filepath.IsAbs(e) {
					e = filepath.Join(repoRoot, e)
				}
				expected = append(expected, e)
			}
			assert.Equal(t, expected, findHelmChartHomes(repoRoot, filepath.Join(repoRoot, tt.app)))
		})
	}
}

func TestFindHelmChartHomesAppOutsideRepoRoot(t *testing.T) {
	repoRoot := t.TempDir()
	other := t.TempDir()
	writeFiles(t, other, map[string]string{"kustomization.yaml": "helmCharts:\n- name: foo\n"})
	assert.Empty(t, findHelmChartHomes(repoRoot, other))
}

func TestLockHelmChartHomes(t *testing.T) {
	repoRoot := t.TempDir()
	writeFiles(t, repoRoot, map[string]string{
		"base/kustomization.yaml":       "helmCharts:\n- name: foo\n",
		"overlays/a/kustomization.yaml": "resources:\n- ../../base\n",
		"overlays/b/kustomization.yaml": "resources:\n- ../../base\n",
		"other/kustomization.yaml":      "helmCharts:\n- name: foo\n",
		"plain/kustomization.yaml":      "resources: []\n",
	})
	unlockA := lockHelmChartHomes(repoRoot, filepath.Join(repoRoot, "overlays", "a"))

	// Builds that don't share the chart home are not blocked.
	for _, app := range []string{"other", "plain"} {
		done := make(chan struct{})
		go func() {
			lockHelmChartHomes(repoRoot, filepath.Join(repoRoot, app))()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("build of %s was blocked by an unrelated chart home", app)
		}
	}

	// A build sharing the base's chart home waits until the first one releases it.
	acquired := make(chan struct{})
	go func() {
		unlockB := lockHelmChartHomes(repoRoot, filepath.Join(repoRoot, "overlays", "b"))
		close(acquired)
		unlockB()
	}()
	select {
	case <-acquired:
		t.Fatal("overlay b acquired the shared chart home while overlay a held it")
	case <-time.After(200 * time.Millisecond):
	}
	unlockA()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("overlay b never acquired the shared chart home")
	}
}

package directives

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
	helmregistry "helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/repo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/kustomize/kyaml/yaml"

	kargoapi "github.com/akuity/kargo/api/v1alpha1"
	"github.com/akuity/kargo/internal/credentials"
	"github.com/akuity/kargo/internal/helm"
)

func Test_helmUpdateChartDirective_run(t *testing.T) {
	tests := []struct {
		name            string
		context         *StepContext
		cfg             HelmUpdateChartConfig
		chartMetadata   *chart.Metadata
		setupRepository func(t *testing.T) (string, func())
		assertions      func(*testing.T, string, Result, error)
	}{
		{
			name: "successful run with HTTP repository",
			context: &StepContext{
				Project: "test-project",
				Freight: kargoapi.FreightCollection{
					Freight: map[string]kargoapi.FreightReference{
						"Warehouse/test-warehouse": {
							Origin: kargoapi.FreightOrigin{Kind: "Warehouse", Name: "test-warehouse"},
							Charts: []kargoapi.Chart{
								{RepoURL: "https://charts.example.com", Name: "examplechart", Version: "0.1.0"},
							},
						},
					},
				},
				FreightRequests: []kargoapi.FreightRequest{
					{
						Origin: kargoapi.FreightOrigin{Kind: "Warehouse", Name: "test-warehouse"},
					},
				},
			},
			cfg: HelmUpdateChartConfig{
				Path: "testchart",
				Charts: []Chart{
					{
						Repository: "https://charts.example.com",
						Name:       "examplechart",
						FromOrigin: &ChartFromOrigin{
							Kind: "Warehouse",
							Name: "test-warehouse",
						},
					},
				},
			},
			chartMetadata: &chart.Metadata{
				APIVersion: chart.APIVersionV2,
				Name:       "test-chart",
				Version:    "0.1.0",
				Dependencies: []*chart.Dependency{
					{
						Name:       "examplechart",
						Version:    ">=0.0.1",
						Repository: "https://charts.example.com",
					},
				},
			},
			setupRepository: func(t *testing.T) (string, func()) {
				httpRepositoryRoot := t.TempDir()
				require.NoError(t, copyFile(
					"testdata/helm/charts/examplechart-0.1.0.tgz",
					filepath.Join(httpRepositoryRoot, "examplechart-0.1.0.tgz"),
				))
				httpRepository := httptest.NewServer(http.FileServer(http.Dir(httpRepositoryRoot)))

				repoIndex, err := repo.IndexDirectory(httpRepositoryRoot, httpRepository.URL)
				require.NoError(t, err)
				require.NoError(t, repoIndex.WriteFile(filepath.Join(httpRepositoryRoot, "index.yaml"), 0o600))

				return httpRepository.URL, httpRepository.Close
			},
			assertions: func(t *testing.T, tempDir string, result Result, err error) {
				assert.NoError(t, err)
				assert.Equal(t, Result{
					Status: StatusSuccess,
					Output: State{
						"commitMessage": `Updated chart dependencies for testchart

- examplechart: 0.1.0`,
					},
				}, result)

				// Check if Chart.yaml was updated correctly
				updatedChartYaml, err := os.ReadFile(filepath.Join(tempDir, "testchart", "Chart.yaml"))
				require.NoError(t, err)
				assert.Contains(t, string(updatedChartYaml), "version: 0.1.0")

				// Check if the dependency was downloaded
				assert.FileExists(t, filepath.Join(tempDir, "testchart", "charts", "examplechart-0.1.0.tgz"))

				// Check if the Chart.lock file was created
				assert.FileExists(t, filepath.Join(tempDir, "testchart", "Chart.lock"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, kargoapi.AddToScheme(scheme))

			stepCtx := tt.context
			stepCtx.WorkDir = t.TempDir()
			chartMetadata := tt.chartMetadata

			if tt.setupRepository != nil {
				repoURL, cleanup := tt.setupRepository(t)
				defer cleanup()

				// Update the repository URL in the configuration and the
				// chart metadata
				for i := range tt.cfg.Charts {
					tt.cfg.Charts[i].Repository = repoURL
				}
				for _, freight := range stepCtx.Freight.Freight {
					for i := range freight.Charts {
						freight.Charts[i].RepoURL = repoURL
					}
				}
				for _, dep := range chartMetadata.Dependencies {
					dep.Repository = repoURL
				}
			}

			if chartMetadata != nil {
				chartPath := filepath.Join(stepCtx.WorkDir, tt.cfg.Path)
				require.NoError(t, os.MkdirAll(chartPath, 0o700))

				b, err := yaml.Marshal(chartMetadata)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(chartPath, "Chart.yaml"), b, 0o600))
			}

			d := &helmUpdateChartDirective{}
			result, err := d.run(context.Background(), stepCtx, tt.cfg)
			tt.assertions(t, stepCtx.WorkDir, result, err)
		})
	}
}

func Test_helmUpdateChartDirective_processChartUpdates(t *testing.T) {
	tests := []struct {
		name              string
		objects           []client.Object
		context           *StepContext
		cfg               HelmUpdateChartConfig
		chartDependencies []chartDependency
		assertions        func(*testing.T, map[string]string, error)
	}{
		{
			name: "finds chart update",
			objects: []client.Object{
				&kargoapi.Warehouse{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-warehouse",
						Namespace: "test-project",
					},
					Spec: kargoapi.WarehouseSpec{
						Subscriptions: []kargoapi.RepoSubscription{
							{
								Chart: &kargoapi.ChartSubscription{
									RepoURL: "https://charts.example.com",
									Name:    "test-chart",
								},
							},
						},
					},
				},
			},
			context: &StepContext{
				Project: "test-project",
				Freight: kargoapi.FreightCollection{
					Freight: map[string]kargoapi.FreightReference{
						"Warehouse/test-warehouse": {
							Origin: kargoapi.FreightOrigin{Kind: "Warehouse", Name: "test-warehouse"},
							Charts: []kargoapi.Chart{
								{RepoURL: "https://charts.example.com", Name: "test-chart", Version: "1.0.0"},
							},
						},
					},
				},
				FreightRequests: []kargoapi.FreightRequest{
					{
						Origin: kargoapi.FreightOrigin{Kind: "Warehouse", Name: "test-warehouse"},
					},
				},
			},
			cfg: HelmUpdateChartConfig{
				Charts: []Chart{
					{Repository: "https://charts.example.com", Name: "test-chart"},
				},
			},
			chartDependencies: []chartDependency{
				{Repository: "https://charts.example.com", Name: "test-chart"},
			},
			assertions: func(t *testing.T, changes map[string]string, err error) {
				assert.NoError(t, err)
				assert.Equal(t, map[string]string{"dependencies.0.version": "1.0.0"}, changes)
			},
		},
		{
			name: "chart not found",
			context: &StepContext{
				Project:         "test-project",
				Freight:         kargoapi.FreightCollection{},
				FreightRequests: []kargoapi.FreightRequest{},
			},
			cfg: HelmUpdateChartConfig{
				Charts: []Chart{
					{Repository: "https://charts.example.com", Name: "non-existent-chart"},
				},
			},
			chartDependencies: []chartDependency{
				{Repository: "https://charts.example.com", Name: "non-existent-chart"},
			},
			assertions: func(t *testing.T, changes map[string]string, err error) {
				assert.NoError(t, err)
				assert.Empty(t, changes)
			},
		},
		{
			name: "multiple charts, one not found",
			objects: []client.Object{
				&kargoapi.Warehouse{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-warehouse",
						Namespace: "test-project",
					},
					Spec: kargoapi.WarehouseSpec{
						Subscriptions: []kargoapi.RepoSubscription{
							{
								Chart: &kargoapi.ChartSubscription{
									RepoURL: "https://charts.example.com",
									Name:    "chart1",
								},
							},
						},
					},
				},
			},
			context: &StepContext{
				Project: "test-project",
				Freight: kargoapi.FreightCollection{
					Freight: map[string]kargoapi.FreightReference{
						"Warehouse/test-warehouse": {
							Origin: kargoapi.FreightOrigin{Kind: "Warehouse", Name: "test-warehouse"},
							Charts: []kargoapi.Chart{
								{RepoURL: "https://charts.example.com", Name: "chart1", Version: "1.0.0"},
							},
						},
					},
				},
				FreightRequests: []kargoapi.FreightRequest{
					{
						Origin: kargoapi.FreightOrigin{Kind: "Warehouse", Name: "test-warehouse"},
					},
				},
			},
			cfg: HelmUpdateChartConfig{
				Charts: []Chart{
					{Repository: "https://charts.example.com", Name: "chart1"},
					{Repository: "https://charts.example.com", Name: "chart2"},
				},
			},
			chartDependencies: []chartDependency{
				{Repository: "https://charts.example.com", Name: "chart1"},
				{Repository: "https://charts.example.com", Name: "chart2"},
			},
			assertions: func(t *testing.T, changes map[string]string, err error) {
				assert.NoError(t, err)
				assert.Equal(t, map[string]string{"dependencies.0.version": "1.0.0"}, changes)
			},
		},
		{
			name: "chart with FromOrigin specified",
			objects: []client.Object{
				&kargoapi.Warehouse{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-warehouse",
						Namespace: "test-project",
					},
					Spec: kargoapi.WarehouseSpec{
						Subscriptions: []kargoapi.RepoSubscription{
							{
								Chart: &kargoapi.ChartSubscription{
									RepoURL: "https://charts.example.com",
									Name:    "origin-chart",
								},
							},
						},
					},
				},
			},
			context: &StepContext{
				Project: "test-project",
				Freight: kargoapi.FreightCollection{
					Freight: map[string]kargoapi.FreightReference{
						"Warehouse/test-warehouse": {
							Origin: kargoapi.FreightOrigin{Kind: "Warehouse", Name: "test-warehouse"},
							Charts: []kargoapi.Chart{
								{RepoURL: "https://charts.example.com", Name: "origin-chart", Version: "2.0.0"},
							},
						},
					},
				},
				FreightRequests: []kargoapi.FreightRequest{
					{
						Origin: kargoapi.FreightOrigin{Kind: "Warehouse", Name: "test-warehouse"},
					},
				},
			},
			cfg: HelmUpdateChartConfig{
				Charts: []Chart{
					{
						Repository: "https://charts.example.com",
						Name:       "origin-chart",
						FromOrigin: &ChartFromOrigin{Kind: "Warehouse", Name: "test-warehouse"},
					},
				},
			},
			chartDependencies: []chartDependency{
				{Repository: "https://charts.example.com", Name: "origin-chart"},
			},
			assertions: func(t *testing.T, changes map[string]string, err error) {
				assert.NoError(t, err)
				assert.Equal(t, map[string]string{"dependencies.0.version": "2.0.0"}, changes)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, kargoapi.AddToScheme(scheme))

			stepCtx := tt.context
			stepCtx.KargoClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.objects...).Build()

			d := &helmUpdateChartDirective{}
			changes, err := d.processChartUpdates(context.Background(), stepCtx, tt.cfg, tt.chartDependencies)
			tt.assertions(t, changes, err)
		})
	}
}

func Test_helmUpdateChartDirective_updateDependencies(t *testing.T) {
	t.Run("updates dependencies", func(t *testing.T) {
		// Set up the HTTP repository
		httpRepositoryRoot := t.TempDir()
		require.NoError(t, copyFile(
			"testdata/helm/charts/examplechart-0.1.0.tgz",
			filepath.Join(httpRepositoryRoot, "examplechart-0.1.0.tgz"),
		))
		httpRepository := httptest.NewServer(http.FileServer(http.Dir(httpRepositoryRoot)))
		t.Cleanup(httpRepository.Close)

		repoIndex, err := repo.IndexDirectory(httpRepositoryRoot, httpRepository.URL)
		require.NoError(t, err)
		require.NoError(t, repoIndex.WriteFile(filepath.Join(httpRepositoryRoot, "index.yaml"), 0o600))

		// Set up the OCI registry
		ociRegistry := httptest.NewServer(registry.New())
		t.Cleanup(ociRegistry.Close)

		ociClient, err := helm.NewRegistryClient(t.TempDir())
		require.NoError(t, err)

		b, err := os.ReadFile("testdata/helm/charts/demo-0.1.0.tgz")
		require.NoError(t, err)
		repositoryRef := strings.TrimPrefix(ociRegistry.URL, "http://")
		_, err = ociClient.Push(b, repositoryRef+"/demo:0.1.0")
		require.NoError(t, err)

		// Prepare the dependant chart with a Chart.yaml file
		chartPath := t.TempDir()
		metadata := chart.Metadata{
			APIVersion: chart.APIVersionV2,
			Name:       "test-chart",
			Version:    "0.1.0",
			Dependencies: []*chart.Dependency{
				{
					Name:       "examplechart",
					Version:    "0.1.0",
					Repository: httpRepository.URL,
				},
				{
					Name:       "demo",
					Version:    "0.1.0",
					Repository: "oci://" + repositoryRef,
				},
			},
		}
		b, err = yaml.Marshal(metadata)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(chartPath, "Chart.yaml"), b, 0o600))

		// Run the directive and assert the dependencies are updated
		d := &helmUpdateChartDirective{}
		newVersions, err := d.updateDependencies(
			context.Background(),
			&StepContext{},
			t.TempDir(),
			chartPath,
			nil,
		)
		require.NoError(t, err)
		require.DirExists(t, filepath.Join(chartPath, "charts"))
		assert.FileExists(t, filepath.Join(chartPath, "charts", "examplechart-0.1.0.tgz"))
		assert.FileExists(t, filepath.Join(chartPath, "charts", "demo-0.1.0.tgz"))
		assert.Equal(t, map[string]string{
			"examplechart": "0.1.0",
			"demo":         "0.1.0",
		}, newVersions)
	})

	t.Run("updates dependencies with credentials", func(t *testing.T) {
		// Set up the OCI registry
		ociRegistry := newAuthRegistryServer("username", "password")
		ociRegistry.Start()
		t.Cleanup(ociRegistry.Close)

		ociClient, err := helm.NewRegistryClient(t.TempDir())
		require.NoError(t, err)

		b, err := os.ReadFile("testdata/helm/charts/demo-0.1.0.tgz")
		require.NoError(t, err)

		repositoryRef := strings.TrimPrefix(ociRegistry.URL, "http://")
		require.NoError(t, ociClient.Login(
			repositoryRef,
			helmregistry.LoginOptBasicAuth("username", "password"),
		))
		_, err = ociClient.Push(b, repositoryRef+"/demo:0.1.0")
		require.NoError(t, err)

		// Prepare the dependant chart with a Chart.yaml file
		chartPath := t.TempDir()
		metadata := chart.Metadata{
			APIVersion: chart.APIVersionV2,
			Name:       "test-chart",
			Version:    "0.1.0",
			Dependencies: []*chart.Dependency{
				{
					Name:       "demo",
					Version:    "0.1.0",
					Repository: "oci://" + repositoryRef,
				},
			},
		}
		b, err = yaml.Marshal(metadata)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(chartPath, "Chart.yaml"), b, 0o600))

		// Prepare the credentials database
		credentialsDB := &credentials.FakeDB{
			GetFn: func(context.Context, string, credentials.Type, string) (credentials.Credentials, bool, error) {
				return credentials.Credentials{
					Username: "username",
					Password: "password",
				}, true, nil
			},
		}

		// Run the directive and assert the dependency is updated
		d := &helmUpdateChartDirective{}
		newVersions, err := d.updateDependencies(context.Background(), &StepContext{
			CredentialsDB: credentialsDB,
		}, t.TempDir(), chartPath, []chartDependency{
			{
				Name:       "demo",
				Repository: "oci://" + repositoryRef,
			},
		})
		require.NoError(t, err)
		require.DirExists(t, filepath.Join(chartPath, "charts"))
		assert.FileExists(t, filepath.Join(chartPath, "charts", "demo-0.1.0.tgz"))
		assert.Equal(t, map[string]string{
			"demo": "0.1.0",
		}, newVersions)
	})

	tests := []struct {
		name              string
		credentialsDB     credentials.Database
		chartDependencies []chartDependency
		assertions        func(*testing.T, string, string, error)
	}{
		{
			name: "error loading dependency credentials",
			credentialsDB: &credentials.FakeDB{
				GetFn: func(context.Context, string, credentials.Type, string) (credentials.Credentials, bool, error) {
					return credentials.Credentials{}, false, fmt.Errorf("something went wrong")
				},
			},
			chartDependencies: []chartDependency{
				{
					Name:       "dep1",
					Repository: "https://charts.example.com",
				},
			},
			assertions: func(t *testing.T, _, _ string, err error) {
				require.ErrorContains(t, err, "failed to obtain credentials")
				require.ErrorContains(t, err, "something went wrong")
			},
		},
		{
			name: "writes repository file",
			credentialsDB: &credentials.FakeDB{
				GetFn: func(context.Context, string, credentials.Type, string) (credentials.Credentials, bool, error) {
					return credentials.Credentials{
						Username: "username",
						Password: "password",
					}, true, nil
				},
			},
			chartDependencies: []chartDependency{
				{
					Name:       "dep1",
					Repository: "https://charts.example.com",
				},
			},
			assertions: func(t *testing.T, helmHome, _ string, _ error) {
				repoFilePath := filepath.Join(helmHome, "repositories.yaml")
				require.FileExists(t, repoFilePath)

				repoFile, err := repo.LoadFile(filepath.Join(helmHome, "repositories.yaml"))
				require.NoError(t, err)
				require.Len(t, repoFile.Repositories, 1)
				assert.Equal(t, "https://charts.example.com", repoFile.Repositories[0].URL)
			},
		},
		{
			name: "error updating dependencies on empty chart",
			assertions: func(t *testing.T, _ string, _ string, err error) {
				require.ErrorContains(t, err, "failed to update chart dependencies")
				require.ErrorContains(t, err, "Chart.yaml file is missing")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			helmHome, chartPath := t.TempDir(), t.TempDir()

			d := &helmUpdateChartDirective{}
			_, err := d.updateDependencies(context.Background(), &StepContext{
				CredentialsDB: tt.credentialsDB,
			}, helmHome, chartPath, tt.chartDependencies)
			tt.assertions(t, helmHome, chartPath, err)
		})
	}
}

func Test_helmUpdateChartDirective_loadDependencyCredentials(t *testing.T) {
	tests := []struct {
		name              string
		credentialsDB     credentials.Database
		repositoryFile    *repo.File
		newRegistryClient func(*testing.T) (string, *helmregistry.Client)
		newOCIServer      func(*testing.T) string
		buildDependencies func(string) []chartDependency
		assertions        func(*testing.T, string, string, *repo.File, error)
	}{
		{
			name: "HTTP credentials",
			credentialsDB: &credentials.FakeDB{
				GetFn: func(context.Context, string, credentials.Type, string) (credentials.Credentials, bool, error) {
					return credentials.Credentials{
						Username: "username",
						Password: "password",
					}, true, nil
				},
			},
			repositoryFile: repo.NewFile(),
			newRegistryClient: func(*testing.T) (string, *helmregistry.Client) {
				return "", nil
			},
			buildDependencies: func(string) []chartDependency {
				return []chartDependency{
					{
						Name:       "dep1",
						Repository: "https://charts.example.com",
					},
				}
			},
			assertions: func(t *testing.T, _, _ string, repositoryFile *repo.File, err error) {
				require.NoError(t, err)
				require.Len(t, repositoryFile.Repositories, 1)
				assert.Equal(t, "https://charts.example.com", repositoryFile.Repositories[0].URL)
				assert.Equal(t, "username", repositoryFile.Repositories[0].Username)
				assert.Equal(t, "password", repositoryFile.Repositories[0].Password)
			},
		},
		{
			name: "OCI credentials",
			credentialsDB: &credentials.FakeDB{
				GetFn: func(context.Context, string, credentials.Type, string) (credentials.Credentials, bool, error) {
					return credentials.Credentials{
						Username: "username",
						Password: "password",
					}, true, nil
				},
			},
			buildDependencies: func(registryURL string) []chartDependency {
				return []chartDependency{
					{
						Name:       "dep1",
						Repository: "oci://" + registryURL,
					},
				}
			},
			newRegistryClient: func(t *testing.T) (string, *helmregistry.Client) {
				home := t.TempDir()
				c, err := helm.NewRegistryClient(home)
				require.NoError(t, err)
				return home, c
			},
			newOCIServer: func(t *testing.T) string {
				srv := newAuthRegistryServer("username", "password")
				srv.Start()
				t.Cleanup(srv.Close)
				return srv.URL
			},
			assertions: func(t *testing.T, home, registryURL string, _ *repo.File, err error) {
				require.NoError(t, err)

				require.FileExists(t, filepath.Join(home, ".docker", "config.json"))
				b, _ := os.ReadFile(filepath.Join(home, ".docker", "config.json"))
				assert.Contains(t, string(b), registryURL)
			},
		},
		{
			name: "multiple dependencies",
			credentialsDB: &credentials.FakeDB{
				GetFn: func(context.Context, string, credentials.Type, string) (credentials.Credentials, bool, error) {
					return credentials.Credentials{
						Username: "username",
						Password: "password",
					}, true, nil
				},
			},
			repositoryFile: repo.NewFile(),
			newRegistryClient: func(*testing.T) (string, *helmregistry.Client) {
				return "", nil
			},
			buildDependencies: func(string) []chartDependency {
				return []chartDependency{
					{
						Name:       "dep1",
						Repository: "https://charts.example.com",
					},
					{
						Name:       "dep2",
						Repository: "https://example.com/repository/",
					},
				}
			},
			assertions: func(t *testing.T, _, _ string, repositoryFile *repo.File, err error) {
				require.NoError(t, err)
				require.Len(t, repositoryFile.Repositories, 2)
				assert.Equal(t, "https://charts.example.com", repositoryFile.Repositories[0].URL)
				assert.Equal(t, "username", repositoryFile.Repositories[0].Username)
				assert.Equal(t, "password", repositoryFile.Repositories[0].Password)
				assert.Equal(t, "https://example.com/repository/", repositoryFile.Repositories[1].URL)
				assert.Equal(t, "username", repositoryFile.Repositories[1].Username)
				assert.Equal(t, "password", repositoryFile.Repositories[1].Password)
			},
		},
		{
			name: "error getting credentials",
			credentialsDB: &credentials.FakeDB{
				GetFn: func(context.Context, string, credentials.Type, string) (credentials.Credentials, bool, error) {
					return credentials.Credentials{}, false, fmt.Errorf("something went wrong")
				},
			},
			buildDependencies: func(string) []chartDependency {
				return []chartDependency{
					{
						Name:       "dep1",
						Repository: "https://charts.example.com",
					},
				}
			},
			newRegistryClient: func(*testing.T) (string, *helmregistry.Client) {
				return "", nil
			},
			assertions: func(t *testing.T, _, _ string, _ *repo.File, err error) {
				require.ErrorContains(t, err, "failed to obtain credentials")
				require.ErrorContains(t, err, "something went wrong")
			},
		},
		{
			name: "unauthenticated repository",
			credentialsDB: &credentials.FakeDB{
				GetFn: func(context.Context, string, credentials.Type, string) (credentials.Credentials, bool, error) {
					return credentials.Credentials{}, false, nil
				},
			},
			buildDependencies: func(string) []chartDependency {
				return []chartDependency{
					{
						Name:       "dep1",
						Repository: "https://charts.example.com",
					},
				}
			},
			newRegistryClient: func(*testing.T) (string, *helmregistry.Client) {
				return "", nil
			},
			assertions: func(t *testing.T, _, _ string, _ *repo.File, err error) {
				require.NoError(t, err)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			helmHome, registryClient := tt.newRegistryClient(t)

			var registryURL string
			if tt.newOCIServer != nil {
				registryURL = tt.newOCIServer(t)
			}

			dependencies := tt.buildDependencies(registryURL)

			d := &helmUpdateChartDirective{}
			err := d.loadDependencyCredentials(
				context.Background(),
				tt.credentialsDB,
				registryClient,
				tt.repositoryFile,
				"fake-project",
				dependencies,
			)
			tt.assertions(t, helmHome, registryURL, tt.repositoryFile, err)
		})
	}
}

func Test_helmUpdateChartDirective_generateCommitMessage(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		newVersions map[string]string
		assertions  func(*testing.T, string)
	}{
		{
			name:        "empty newVersions",
			path:        "charts/mychart",
			newVersions: map[string]string{},
			assertions: func(t *testing.T, got string) {
				assert.Empty(t, got)
			},
		},
		{
			name: "single update",
			path: "charts/mychart",
			newVersions: map[string]string{
				"chart1": "1.0.0 -> 1.1.0",
			},
			assertions: func(t *testing.T, got string) {
				assert.Contains(t, got, "Updated chart dependencies for charts/mychart")
				assert.Contains(t, got, "- chart1: 1.0.0 -> 1.1.0")
				assert.Equal(t, 2, strings.Count(got, "\n"))
			},
		},
		{
			name: "multiple updates",
			path: "charts/mychart",
			newVersions: map[string]string{
				"chart1": "1.0.0 -> 1.1.0",
				"chart2": "2.0.0 -> 2.1.0",
				"chart3": "3.0.0 -> 3.1.0",
			},
			assertions: func(t *testing.T, got string) {
				assert.Contains(t, got, "Updated chart dependencies for charts/mychart")
				assert.Contains(t, got, "- chart1: 1.0.0 -> 1.1.0")
				assert.Contains(t, got, "- chart2: 2.0.0 -> 2.1.0")
				assert.Contains(t, got, "- chart3: 3.0.0 -> 3.1.0")
				assert.Equal(t, 4, strings.Count(got, "\n"))
			},
		},
		{
			name: "updates and removals",
			path: "charts/mychart",
			newVersions: map[string]string{
				"chart1": "1.0.0 -> 1.1.0",
				"chart2": "",
				"chart3": "3.0.0 -> 3.1.0",
			},
			assertions: func(t *testing.T, got string) {
				assert.Contains(t, got, "Updated chart dependencies for charts/mychart")
				assert.Contains(t, got, "- chart1: 1.0.0 -> 1.1.0")
				assert.Contains(t, got, "- chart2: removed")
				assert.Contains(t, got, "- chart3: 3.0.0 -> 3.1.0")
				assert.Equal(t, 4, strings.Count(got, "\n"))
			},
		},
		{
			name: "only removals",
			path: "charts/mychart",
			newVersions: map[string]string{
				"chart1": "",
				"chart2": "",
			},
			assertions: func(t *testing.T, got string) {
				assert.Contains(t, got, "Updated chart dependencies for charts/mychart")
				assert.Contains(t, got, "- chart1: removed")
				assert.Contains(t, got, "- chart2: removed")
				assert.Equal(t, 3, strings.Count(got, "\n"))
			},
		},
		{
			name: "new additions",
			path: "charts/mychart",
			newVersions: map[string]string{
				"chart1": "1.0.0",
				"chart2": "2.0.0",
			},
			assertions: func(t *testing.T, got string) {
				assert.Contains(t, got, "Updated chart dependencies for charts/mychart")
				assert.Contains(t, got, "- chart1: 1.0.0")
				assert.Contains(t, got, "- chart2: 2.0.0")
				assert.Equal(t, 3, strings.Count(got, "\n"))
			},
		},
		{
			name: "mixed updates, removals, and additions",
			path: "charts/mychart",
			newVersions: map[string]string{
				"chart1": "1.0.0 -> 1.1.0",
				"chart2": "",
				"chart3": "3.0.0",
				"chart4": "4.0.0 -> 4.1.0",
			},
			assertions: func(t *testing.T, got string) {
				assert.Contains(t, got, "Updated chart dependencies for charts/mychart")
				assert.Contains(t, got, "- chart1: 1.0.0 -> 1.1.0")
				assert.Contains(t, got, "- chart2: removed")
				assert.Contains(t, got, "- chart3: 3.0.0")
				assert.Contains(t, got, "- chart4: 4.0.0 -> 4.1.0")
				assert.Equal(t, 5, strings.Count(got, "\n"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &helmUpdateChartDirective{}
			got := d.generateCommitMessage(tt.path, tt.newVersions)
			tt.assertions(t, got)
		})
	}
}

func Test_normalizeChartReference(t *testing.T) {
	tests := []struct {
		name            string
		repoURL         string
		chartName       string
		expectedRepoURL string
		expectedChart   string
	}{
		{
			name:            "OCI repository",
			repoURL:         "oci://example.com/charts",
			chartName:       "mychart",
			expectedRepoURL: "oci://example.com/charts/mychart",
			expectedChart:   "",
		},
		{
			name:            "OCI repository with trailing slash",
			repoURL:         "oci://example.com/charts/",
			chartName:       "mychart",
			expectedRepoURL: "oci://example.com/charts/mychart",
			expectedChart:   "",
		},
		{
			name:            "HTTP repository",
			repoURL:         "https://charts.example.com",
			chartName:       "mychart",
			expectedRepoURL: "https://charts.example.com",
			expectedChart:   "mychart",
		},
		{
			name:            "HTTP repository with path",
			repoURL:         "https://example.com/charts",
			chartName:       "mychart",
			expectedRepoURL: "https://example.com/charts",
			expectedChart:   "mychart",
		},
		{
			name:            "local path",
			repoURL:         "./charts",
			chartName:       "mychart",
			expectedRepoURL: "./charts",
			expectedChart:   "mychart",
		},
		{
			name:            "empty repo URL",
			repoURL:         "",
			chartName:       "mychart",
			expectedRepoURL: "",
			expectedChart:   "mychart",
		},
		{
			name:            "empty chart name",
			repoURL:         "https://charts.example.com",
			chartName:       "",
			expectedRepoURL: "https://charts.example.com",
			expectedChart:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoURL, chart := normalizeChartReference(tt.repoURL, tt.chartName)
			assert.Equal(t, tt.expectedRepoURL, repoURL)
			assert.Equal(t, tt.expectedChart, chart)
		})
	}
}

func Test_readChartDependencies(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(*testing.T) string
		assertions func(*testing.T, []chartDependency, error)
	}{
		{
			name: "valid chart.yaml",
			setup: func(t *testing.T) string {
				tmpDir := t.TempDir()

				const chartYAML = `---
apiVersion: v2
name: test-chart
version: 0.1.0
dependencies:
- name: dep1
  version: 1.0.0
  repository: https://charts.example.com
- name: dep2
  version: 2.0.0
  repository: oci://registry.example.com/charts
`
				chartPath := filepath.Join(tmpDir, "Chart.yaml")
				require.NoError(t, os.WriteFile(chartPath, []byte(chartYAML), 0o600))

				return chartPath
			},
			assertions: func(t *testing.T, dependencies []chartDependency, err error) {
				require.NoError(t, err)
				assert.Len(t, dependencies, 2)

				assert.Equal(t, "dep1", dependencies[0].Name)
				assert.Equal(t, "https://charts.example.com", dependencies[0].Repository)
				assert.Equal(t, "dep2", dependencies[1].Name)
				assert.Equal(t, "oci://registry.example.com/charts", dependencies[1].Repository)
			},
		},
		{
			name: "invalid Chart.yaml",
			setup: func(t *testing.T) string {
				tmpDir := t.TempDir()

				const chartYAML = `---
this is not a valid chart.yaml
`
				chartPath := filepath.Join(tmpDir, "Chart.yaml")
				require.NoError(t, os.WriteFile(chartPath, []byte(chartYAML), 0o600))

				return chartPath
			},
			assertions: func(t *testing.T, dependencies []chartDependency, err error) {
				require.ErrorContains(t, err, "failed to unmarshal")
				assert.Nil(t, dependencies)
			},
		},
		{
			name: "missing Chart.yaml",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "Chart.yaml")
			},
			assertions: func(t *testing.T, dependencies []chartDependency, err error) {
				require.ErrorContains(t, err, "failed to read file")
				assert.Nil(t, dependencies)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chartPath := tt.setup(t)
			dependencies, err := readChartDependencies(chartPath)
			tt.assertions(t, dependencies, err)
		})
	}
}

func Test_readChartLock(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(*testing.T) string
		assertions func(*testing.T, map[string]string, error)
	}{
		{
			name: "valid Chart.lock",
			setup: func(t *testing.T) string {
				tmpDir := t.TempDir()

				const chartLock = `---
dependencies:
- name: dep1
  version: 1.0.0
  repository: https://charts.example.com
- name: dep2
  version: 2.0.0
  repository: oci://registry.example.com/charts
`
				require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "Chart.lock"), []byte(chartLock), 0o600))
				return tmpDir
			},
			assertions: func(t *testing.T, charts map[string]string, err error) {
				require.NoError(t, err)

				assert.Len(t, charts, 2)
				assert.Equal(t, "1.0.0", charts["dep1"])
				assert.Equal(t, "2.0.0", charts["dep2"])
			},
		},
		{
			name: "invalid Chart.lock",
			setup: func(t *testing.T) string {
				tmpDir := t.TempDir()

				const chartLock = `---
this is not a valid Chart.lock
`
				require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "Chart.lock"), []byte(chartLock), 0o600))
				return tmpDir
			},
			assertions: func(t *testing.T, charts map[string]string, err error) {
				require.ErrorContains(t, err, "failed to parse Chart.lock")
				assert.Empty(t, charts)
			},
		},
		{
			name: "missing Chart.lock",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			assertions: func(t *testing.T, charts map[string]string, err error) {
				require.NoError(t, err)
				assert.Empty(t, charts)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chartPath := tt.setup(t)
			charts, err := readChartLock(chartPath)
			tt.assertions(t, charts, err)
		})
	}
}

func Test_compareChartVersions(t *testing.T) {
	tests := []struct {
		name   string
		before map[string]string
		after  map[string]string
		want   map[string]string
	}{
		{
			name:   "No changes",
			before: map[string]string{"chart1": "1.0.0", "chart2": "2.0.0"},
			after:  map[string]string{"chart1": "1.0.0", "chart2": "2.0.0"},
			want:   map[string]string{},
		},
		{
			name:   "version update",
			before: map[string]string{"chart1": "1.0.0", "chart2": "2.0.0"},
			after:  map[string]string{"chart1": "1.1.0", "chart2": "2.0.0"},
			want:   map[string]string{"chart1": "1.0.0 -> 1.1.0"},
		},
		{
			name:   "new chart added",
			before: map[string]string{"chart1": "1.0.0"},
			after:  map[string]string{"chart1": "1.0.0", "chart2": "2.0.0"},
			want:   map[string]string{"chart2": "2.0.0"},
		},
		{
			name:   "chart removed",
			before: map[string]string{"chart1": "1.0.0", "chart2": "2.0.0"},
			after:  map[string]string{"chart1": "1.0.0"},
			want:   map[string]string{"chart2": ""},
		},
		{
			name:   "multiple changes",
			before: map[string]string{"chart1": "1.0.0", "chart2": "2.0.0", "chart3": "3.0.0"},
			after:  map[string]string{"chart1": "1.1.0", "chart2": "2.0.0", "chart4": "4.0.0"},
			want:   map[string]string{"chart1": "1.0.0 -> 1.1.0", "chart3": "", "chart4": "4.0.0"},
		},
		{
			name:   "empty before",
			before: map[string]string{},
			after:  map[string]string{"chart1": "1.0.0", "chart2": "2.0.0"},
			want:   map[string]string{"chart1": "1.0.0", "chart2": "2.0.0"},
		},
		{
			name:   "empty after",
			before: map[string]string{"chart1": "1.0.0", "chart2": "2.0.0"},
			after:  map[string]string{},
			want:   map[string]string{"chart1": "", "chart2": ""},
		},
		{
			name:   "both empty",
			before: map[string]string{},
			after:  map[string]string{},
			want:   map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, compareChartVersions(tt.before, tt.after))
		})
	}
}

func copyFile(src, dst string) error {
	srcF, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("error opening source file: %v", err)
	}
	defer srcF.Close()

	dstF, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("error creating destination file: %v", err)
	}
	defer dstF.Close()

	if _, err = io.Copy(dstF, srcF); err != nil {
		return fmt.Errorf("error copying file: %v", err)
	}

	srcI, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("error getting source file info: %v", err)
	}

	return os.Chmod(dst, srcI.Mode())
}

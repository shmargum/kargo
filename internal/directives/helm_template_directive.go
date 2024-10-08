package directives

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/xeipuuv/gojsonschema"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/cli/values"
)

func init() {
	// Register the helm-template directive with the builtins registry.
	builtins.RegisterDirective(newHelmTemplateDirective(), &DirectivePermissions{
		AllowArgoCDClient:  true,
		AllowCredentialsDB: true,
	})
}

type helmTemplateDirective struct {
	schemaLoader gojsonschema.JSONLoader
}

// newHelmTemplateDirective creates a new helm-update-image directive.
func newHelmTemplateDirective() Directive {
	d := &helmTemplateDirective{}
	d.schemaLoader = getConfigSchemaLoader(d.Name())
	return d
}

// Name implements the Directive interface.
func (d *helmTemplateDirective) Name() string {
	return "helm-template"
}

// Run implements the Directive interface.
func (d *helmTemplateDirective) Run(ctx context.Context, stepCtx *StepContext) (Result, error) {
	failure := Result{Status: StatusFailure}

	// Validate the configuration against the JSON Schema
	if err := validate(
		d.schemaLoader,
		gojsonschema.NewGoLoader(stepCtx.Config),
		d.Name(),
	); err != nil {
		return failure, err
	}

	// Convert the configuration into a typed struct
	cfg, err := configToStruct[HelmTemplateConfig](stepCtx.Config)
	if err != nil {
		return failure, fmt.Errorf("could not convert config into %s config: %w", d.Name(), err)
	}

	return d.run(ctx, stepCtx, cfg)
}

func (d *helmTemplateDirective) run(
	ctx context.Context,
	stepCtx *StepContext,
	cfg HelmTemplateConfig,
) (Result, error) {
	composedValues, err := d.composeValues(stepCtx.WorkDir, cfg.ValuesFiles)
	if err != nil {
		return Result{Status: StatusFailure}, fmt.Errorf("failed to compose values: %w", err)
	}

	chartRequested, err := d.loadChart(stepCtx.WorkDir, cfg.Path)
	if err != nil {
		return Result{Status: StatusFailure}, fmt.Errorf("failed to load chart from %q: %w", cfg.Path, err)
	}

	if err = d.checkDependencies(chartRequested); err != nil {
		return Result{Status: StatusFailure}, fmt.Errorf("missing chart dependencies: %w", err)
	}

	install, err := d.newInstallAction(cfg, stepCtx.Project)
	if err != nil {
		return Result{Status: StatusFailure}, fmt.Errorf("failed to initialize Helm action config: %w", err)
	}

	rls, err := install.RunWithContext(ctx, chartRequested, composedValues)
	if err != nil {
		return Result{Status: StatusFailure}, fmt.Errorf("failed to render chart: %w", err)
	}

	if err = d.writeOutput(stepCtx.WorkDir, cfg.OutPath, rls.Manifest); err != nil {
		return Result{Status: StatusFailure}, fmt.Errorf("failed to write rendered chart: %w", err)
	}
	return Result{Status: StatusSuccess}, nil
}

// composeValues composes the values from the given values files. It merges the
// values in the order they are provided.
func (d *helmTemplateDirective) composeValues(workDir string, valuesFiles []string) (map[string]any, error) {
	valueOpts := &values.Options{}
	for _, p := range valuesFiles {
		absValuesPath, err := securejoin.SecureJoin(workDir, p)
		if err != nil {
			return nil, fmt.Errorf("failed to join path %q: %w", p, err)
		}
		valueOpts.ValueFiles = append(valueOpts.ValueFiles, absValuesPath)
	}
	return valueOpts.MergeValues(nil)
}

// newInstallAction creates a new Helm install action with the given
// configuration. It sets the action to dry-run mode and client-only mode,
// meaning that it will not install the chart, but only render the manifest.
func (d *helmTemplateDirective) newInstallAction(cfg HelmTemplateConfig, project string) (*action.Install, error) {
	client := action.NewInstall(&action.Configuration{})

	client.DryRun = true
	client.DryRunOption = "client"
	client.Replace = true
	client.ClientOnly = true
	client.ReleaseName = defaultValue(cfg.ReleaseName, "release-name")
	client.Namespace = defaultValue(cfg.Namespace, project)
	client.IncludeCRDs = cfg.IncludeCRDs
	client.APIVersions = cfg.APIVersions

	if cfg.KubeVersion != "" {
		kubeVersion, err := chartutil.ParseKubeVersion(cfg.KubeVersion)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Kubernetes version %q: %w", cfg.KubeVersion, err)
		}
		client.KubeVersion = kubeVersion
	}

	return client, nil
}

// loadChart loads the chart from the given path.
func (d *helmTemplateDirective) loadChart(workDir, path string) (*chart.Chart, error) {
	absChartPath, err := securejoin.SecureJoin(workDir, path)
	if err != nil {
		return nil, fmt.Errorf("failed to join path %q: %w", path, err)
	}
	return loader.Load(absChartPath)
}

// checkDependencies checks if the chart has all its dependencies.
func (d *helmTemplateDirective) checkDependencies(chartRequested *chart.Chart) error {
	if req := chartRequested.Metadata.Dependencies; req != nil {
		if err := action.CheckDependencies(chartRequested, req); err != nil {
			return err
		}
	}
	return nil
}

// writeOutput writes the rendered manifest to the output path. It creates the
// directory if it does not exist.
func (d *helmTemplateDirective) writeOutput(workDir, outPath, manifest string) error {
	absOutPath, err := securejoin.SecureJoin(workDir, outPath)
	if err != nil {
		return fmt.Errorf("failed to join path %q: %w", outPath, err)
	}
	if err = os.MkdirAll(filepath.Dir(absOutPath), 0o700); err != nil {
		return fmt.Errorf("failed to create directory %q: %w", outPath, err)
	}
	return os.WriteFile(absOutPath, []byte(manifest), 0o600)
}

// defaultValue returns the value if it is not zero or empty, otherwise it
// returns the default value.
func defaultValue[T any](value, defaultValue T) T {
	if v := reflect.ValueOf(value); !v.IsValid() || v.IsZero() || (v.Kind() == reflect.Slice && v.Len() == 0) {
		return defaultValue
	}
	return value
}
